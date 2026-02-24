#!/usr/bin/env python3
"""
ClipFeed Ingestion Worker
Processes video sources: download -> analyze -> split -> transcode -> transcribe -> upload
"""

import os
import re
import json
import time
import uuid
import sqlite3
import signal
import logging
import subprocess
from pathlib import Path
from datetime import datetime, timedelta
from concurrent.futures import ThreadPoolExecutor

import struct

import numpy as np
from minio import Minio
from faster_whisper import WhisperModel
from keybert import KeyBERT
from sentence_transformers import SentenceTransformer

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s"
)
log = logging.getLogger("worker")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
MINIO_ENDPOINT = os.getenv("MINIO_ENDPOINT", "localhost:9000")
MINIO_ACCESS = os.getenv("MINIO_ACCESS_KEY", "clipfeed")
MINIO_SECRET = os.getenv("MINIO_SECRET_KEY", "changeme123")
MINIO_BUCKET = os.getenv("MINIO_BUCKET", "clips")
MINIO_SSL = os.getenv("MINIO_USE_SSL", "false") == "true"
WHISPER_MODEL = os.getenv("WHISPER_MODEL", "base")
MAX_CONCURRENT = int(os.getenv("MAX_CONCURRENT_JOBS", "2"))
FFMPEG_THREADS = os.getenv("FFMPEG_THREADS", "2")
WHISPER_THREADS = int(os.getenv("WHISPER_THREADS", "4"))
CLIP_TTL_DAYS = int(os.getenv("CLIP_TTL_DAYS", "30"))
WORK_DIR = Path(os.getenv("WORK_DIR", "/tmp/clipfeed"))

# Clip splitting parameters
MIN_CLIP_SECONDS = 15
MAX_CLIP_SECONDS = 90
TARGET_CLIP_SECONDS = 45
SILENCE_NOISE_DB = -30
SILENCE_MIN_DURATION = 0.5

# Retry parameters
RETRY_BASE_DELAY = 30  # seconds; doubles each attempt (30s, 60s, 120s, …)
JOB_STALE_MINUTES = int(os.getenv("JOB_STALE_MINUTES", "120"))

shutdown = False


def signal_handler(sig, frame):
    global shutdown
    log.info("Shutdown signal received, finishing current jobs...")
    shutdown = True


signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)


def _detect_device() -> tuple[str, str]:
    """Pick CUDA when an NVIDIA GPU is reachable, otherwise fall back to CPU."""
    try:
        import ctranslate2
        if "cuda" in ctranslate2.get_supported_compute_types("cuda"):
            log.info("CUDA device detected — Whisper will use GPU")
            return "cuda", "float16"
    except Exception:
        pass
    log.info("No CUDA device found — Whisper will use CPU")
    return "cpu", "int8"


def open_db():
    """Open a SQLite connection with WAL mode and row factory."""
    db = sqlite3.connect(DB_PATH, isolation_level=None, check_same_thread=False)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    db.execute("PRAGMA foreign_keys=ON")
    db.execute("PRAGMA synchronous=NORMAL")
    db.row_factory = sqlite3.Row
    return db


class Worker:
    def __init__(self):
        # Main-thread connection used only for job popping
        self.db = open_db()
        self.minio = Minio(
            MINIO_ENDPOINT,
            access_key=MINIO_ACCESS,
            secret_key=MINIO_SECRET,
            secure=MINIO_SSL,
        )
        WORK_DIR.mkdir(parents=True, exist_ok=True)

        if not self.minio.bucket_exists(MINIO_BUCKET):
            self.minio.make_bucket(MINIO_BUCKET)

        device, compute_type = _detect_device()
        whisper_kwargs = dict(device=device, compute_type=compute_type)
        if device == "cpu":
            whisper_kwargs["cpu_threads"] = WHISPER_THREADS
        self.whisper = WhisperModel(WHISPER_MODEL, **whisper_kwargs)
        self.kw_model = KeyBERT(model='all-MiniLM-L6-v2')
        self.text_embedder = SentenceTransformer('all-MiniLM-L6-v2')

        self._clip_model = None
        self._clip_preprocess = None
        self._clip_tokenizer = None

        self._backfill_topic_graph()

    @staticmethod
    def _slugify(name: str) -> str:
        slug = name.lower().strip()
        slug = re.sub(r'[^a-z0-9\s-]', '', slug)
        slug = re.sub(r'[\s-]+', '-', slug)
        return slug.strip('-') or 'topic'

    def _resolve_or_create_topic(self, db, name: str) -> str:
        """Find or create a topic node within the current transaction, returning its ID."""
        slug = self._slugify(name)
        row = db.execute(
            "SELECT id FROM topics WHERE slug = ? OR LOWER(name) = LOWER(?)",
            (slug, name)
        ).fetchone()
        if row:
            return row["id"]

        topic_id = str(uuid.uuid4())
        db.execute(
            "INSERT OR IGNORE INTO topics (id, name, slug, path, depth) VALUES (?, ?, ?, ?, 0)",
            (topic_id, name, slug, slug)
        )
        row = db.execute("SELECT id FROM topics WHERE slug = ?", (slug,)).fetchone()
        if row:
            return row["id"]
        return topic_id

    def _backfill_topic_graph(self):
        """One-time migration: seed topics table from existing clips.topics JSON arrays."""
        db = open_db()
        try:
            existing = db.execute("SELECT COUNT(*) FROM topics").fetchone()[0]
            if existing > 0:
                log.info(f"Topic graph already has {existing} nodes, skipping backfill")
                return

            rows = db.execute(
                "SELECT id, topics FROM clips WHERE status = 'ready' AND topics != '[]'"
            ).fetchall()

            if not rows:
                log.info("No clips to backfill topics from")
                return

            topic_ids = {}
            clip_topic_pairs = []

            for row in rows:
                clip_id = row["id"]
                try:
                    topics = json.loads(row["topics"])
                except (json.JSONDecodeError, TypeError):
                    continue
                for name in topics:
                    if not name:
                        continue
                    slug = self._slugify(name)
                    if slug not in topic_ids:
                        topic_ids[slug] = (str(uuid.uuid4()), name, slug)
                    clip_topic_pairs.append((clip_id, topic_ids[slug][0]))

            if not topic_ids:
                return

            db.execute("BEGIN IMMEDIATE")
            for slug, (tid, name, slug) in topic_ids.items():
                db.execute(
                    "INSERT OR IGNORE INTO topics (id, name, slug, path, depth) VALUES (?, ?, ?, ?, 0)",
                    (tid, name, slug, slug)
                )
            for clip_id, topic_id in clip_topic_pairs:
                db.execute(
                    "INSERT OR IGNORE INTO clip_topics (clip_id, topic_id, confidence, source) VALUES (?, ?, 1.0, 'backfill')",
                    (clip_id, topic_id)
                )
            db.execute("COMMIT")

            log.info(f"Backfilled topic graph: {len(topic_ids)} topics, {len(clip_topic_pairs)} clip-topic links")
        except Exception as e:
            log.error(f"Topic backfill failed: {e}")
            try:
                db.execute("ROLLBACK")
            except Exception:
                pass
        finally:
            db.close()

    def _pop_job(self):
        """Atomically claim one pending job. Returns sqlite3.Row or None."""
        try:
            self.db.execute("BEGIN IMMEDIATE")
            row = self.db.execute("""
                UPDATE jobs
                SET status = 'running',
                    started_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
                    attempts = attempts + 1
                WHERE id = (
                    SELECT id FROM jobs
                    WHERE status = 'queued'
                      AND (run_after IS NULL
                           OR run_after <= strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
                    ORDER BY priority DESC, created_at ASC
                    LIMIT 1
                )
                RETURNING id, payload
            """).fetchone()
            self.db.execute("COMMIT")
            return row
        except Exception as e:
            try:
                self.db.execute("ROLLBACK")
            except Exception:
                pass
            raise

    def run(self):
        log.info(f"Worker started (max_concurrent={MAX_CONCURRENT})")
        inflight = set()
        last_reclaim_at = 0.0

        with ThreadPoolExecutor(max_workers=MAX_CONCURRENT) as pool:
            while not shutdown:
                try:
                    done = [f for f in list(inflight) if f.done()]
                    for fut in done:
                        inflight.discard(fut)
                        try:
                            fut.result()
                        except Exception as e:
                            log.error(f"Background job crashed: {e}")

                    now = time.time()
                    if now-last_reclaim_at >= 60:
                        requeued, failed = self._reclaim_stale_running_jobs()
                        if requeued or failed:
                            log.warning(
                                f"Recovered stale running jobs (>{JOB_STALE_MINUTES}m): "
                                f"requeued={requeued}, failed={failed}"
                            )
                        last_reclaim_at = now

                    if len(inflight) >= MAX_CONCURRENT:
                        time.sleep(1)
                        continue

                    row = self._pop_job()
                    if row is None:
                        time.sleep(2)
                        continue
                    job_id = row["id"]
                    payload = json.loads(row["payload"])
                    log.info(f"Claimed job {job_id}")
                    fut = pool.submit(self.process_job, job_id, payload)
                    inflight.add(fut)
                except Exception as e:
                    log.error(f"Job pop failed: {e}")
                    time.sleep(5)

        log.info("Worker shut down")

    def _reclaim_stale_running_jobs(self) -> tuple[int, int]:
        """
        Reclaim jobs stuck in 'running' beyond JOB_STALE_MINUTES.
        - jobs with attempts < max_attempts are re-queued
        - jobs at max attempts are marked failed
        Returns: (requeued_count, failed_count)
        """
        cutoff = f"-{JOB_STALE_MINUTES} minutes"
        stale_msg = f"stale watchdog: recovered running job older than {JOB_STALE_MINUTES}m"
        try:
            self.db.execute("BEGIN IMMEDIATE")

            requeued = self.db.execute(
                """
                UPDATE jobs
                SET status = 'queued',
                    run_after = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
                    error = CASE
                        WHEN error IS NULL OR error = '' THEN ?
                        ELSE error || ' | ' || ?
                    END
                WHERE status = 'running'
                  AND started_at IS NOT NULL
                  AND datetime(started_at) <= datetime('now', ?)
                  AND attempts < max_attempts
                RETURNING id, source_id
                """,
                (stale_msg, stale_msg, cutoff),
            ).fetchall()

            failed = self.db.execute(
                """
                UPDATE jobs
                SET status = 'failed',
                    completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'),
                    error = CASE
                        WHEN error IS NULL OR error = '' THEN ?
                        ELSE error || ' | ' || ?
                    END
                WHERE status = 'running'
                  AND started_at IS NOT NULL
                  AND datetime(started_at) <= datetime('now', ?)
                  AND attempts >= max_attempts
                RETURNING id, source_id
                """,
                (stale_msg, stale_msg, cutoff),
            ).fetchall()

            for row in requeued:
                source_id = row["source_id"]
                if source_id:
                    self.db.execute(
                        "UPDATE sources SET status = 'pending' WHERE id = ?",
                        (source_id,),
                    )

            for row in failed:
                source_id = row["source_id"]
                if source_id:
                    self.db.execute(
                        "UPDATE sources SET status = 'failed' WHERE id = ?",
                        (source_id,),
                    )

            self.db.execute("COMMIT")
            return len(requeued), len(failed)
        except Exception:
            try:
                self.db.execute("ROLLBACK")
            except Exception:
                pass
            return 0, 0

    def process_job(self, job_id: str, payload: dict):
        """Each thread gets its own DB connection."""
        db = open_db()
        try:
            source_id = payload.get("source_id")
            platform = payload.get("platform", "")
            url = payload.get("url", "")

            db.execute("UPDATE sources SET status = 'downloading' WHERE id = ?", (source_id,))

            work_path = WORK_DIR / job_id
            work_path.mkdir(parents=True, exist_ok=True)

            try:
                # Fetch platform cookie if applicable
                cookie_str = None
                if platform in ("youtube", "tiktok", "instagram", "twitter"):
                    row = db.execute("""
                        SELECT cookie_str FROM platform_cookies
                        WHERE user_id = (SELECT submitted_by FROM sources WHERE id = ?)
                          AND platform = ? AND is_active = 1
                    """, (source_id, platform)).fetchone()
                    if row:
                        cookie_str = row["cookie_str"]

                # Step 0: Fetch source metadata early so failed downloads still have context
                source_metadata = self.fetch_source_metadata(url, work_path, cookie_str=cookie_str)
                if source_metadata:
                    db.execute(
                        """
                        UPDATE sources
                        SET external_id = ?,
                            title = ?,
                            channel_name = ?,
                            thumbnail_url = ?,
                            duration_seconds = ?,
                            metadata = ?
                        WHERE id = ?
                        """,
                        (
                            source_metadata.get("id"),
                            source_metadata.get("title"),
                            source_metadata.get("uploader") or source_metadata.get("channel"),
                            source_metadata.get("thumbnail"),
                            source_metadata.get("duration"),
                            json.dumps(source_metadata),
                            source_id,
                        ),
                    )

                # Step 1: Download
                source_file = self.download(url, work_path, cookie_str=cookie_str)
                db.execute("UPDATE sources SET status = 'processing' WHERE id = ?", (source_id,))

                # Step 2: Extract metadata
                media_metadata = self.extract_metadata(source_file)
                merged_metadata = dict(source_metadata) if source_metadata else {}
                if media_metadata:
                    merged_metadata["media_probe"] = media_metadata
                db.execute(
                    "UPDATE sources SET title = ?, duration_seconds = ?, metadata = ? WHERE id = ?",
                    (
                        (source_metadata or {}).get("title") or media_metadata.get("title"),
                        (source_metadata or {}).get("duration") or media_metadata.get("duration"),
                        json.dumps(merged_metadata),
                        source_id,
                    ),
                )

                # Step 3: Detect scenes and split
                segments = self.detect_scenes(source_file, media_metadata.get("duration", 0))

                # Step 4: Process each segment
                clip_ids = []
                segment_metadata = dict(media_metadata)
                if source_metadata and source_metadata.get("title"):
                    segment_metadata["title"] = source_metadata.get("title")
                for i, seg in enumerate(segments):
                    clip_id = self.process_segment(
                        db, source_file, source_id, seg, i, work_path, segment_metadata
                    )
                    if clip_id:
                        clip_ids.append(clip_id)

                # Mark source complete
                db.execute("UPDATE sources SET status = 'complete' WHERE id = ?", (source_id,))

                # Mark job complete
                db.execute(
                    "UPDATE jobs SET status = 'complete', completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), result = ? WHERE id = ?",
                    (json.dumps({"clip_ids": clip_ids, "clip_count": len(clip_ids)}), job_id),
                )
                log.info(f"Job {job_id} complete: {len(clip_ids)} clips created")

            except Exception as e:
                job_row = db.execute(
                    "SELECT attempts, max_attempts FROM jobs WHERE id = ?", (job_id,)
                ).fetchone()
                attempts = job_row["attempts"] if job_row else 0
                max_attempts = job_row["max_attempts"] if job_row else 3

                if attempts < max_attempts:
                    delay = RETRY_BASE_DELAY * (2 ** (attempts - 1))
                    run_after = (datetime.utcnow() + timedelta(seconds=delay)).strftime(
                        "%Y-%m-%dT%H:%M:%SZ"
                    )
                    log.warning(
                        f"Job {job_id} attempt {attempts}/{max_attempts} failed, "
                        f"retrying in {delay}s: {e}"
                    )
                    db.execute(
                        "UPDATE jobs SET status = 'queued', error = ?, run_after = ? WHERE id = ?",
                        (str(e), run_after, job_id),
                    )
                    db.execute(
                        "UPDATE sources SET status = 'pending' WHERE id = ?", (source_id,)
                    )
                else:
                    log.error(
                        f"Job {job_id} permanently failed after {attempts} attempts: {e}"
                    )
                    db.execute(
                        "UPDATE jobs SET status = 'failed', error = ? WHERE id = ?",
                        (str(e), job_id),
                    )
                    db.execute(
                        "UPDATE sources SET status = 'failed' WHERE id = ?", (source_id,)
                    )

            finally:
                # Cleanup working directory
                subprocess.run(["rm", "-rf", str(work_path)], check=False)

        except Exception as e:
            log.error(f"Fatal error processing job {job_id}: {e}")
        finally:
            db.close()

    def download(self, url: str, work_path: Path, cookie_str: str = None) -> Path:
        """Download video using yt-dlp."""
        output_template = str(work_path / "source.%(ext)s")

        cmd = [
            "yt-dlp",
            "--no-playlist",
            "--js-runtimes", "node",
            "--format", "bestvideo[height<=1080]+bestaudio/best[height<=1080]",
            "--merge-output-format", "mp4",
            "--output", output_template,
            "--no-overwrites",
            "--socket-timeout", "30",
        ]

        if cookie_str:
            cookie_file = work_path / "cookies.txt"
            cookie_file.write_text(cookie_str)
            cmd += ["--cookies", str(cookie_file)]

        cmd.append(url)

        log.info(f"Downloading: {url}")
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=600)

        if result.returncode != 0:
            raise RuntimeError(f"yt-dlp failed: {result.stderr[:500]}")

        # Find the downloaded file
        for f in work_path.glob("source.*"):
            if f.suffix in (".mp4", ".mkv", ".webm"):
                return f

        raise RuntimeError("Download completed but no video file found")

    def fetch_source_metadata(self, url: str, work_path: Path, cookie_str: str = None) -> dict:
        """Fetch source metadata with yt-dlp without downloading media."""
        cmd = [
            "yt-dlp",
            "--no-playlist",
            "--dump-single-json",
            "--skip-download",
            "--socket-timeout", "30",
            url,
        ]

        if cookie_str:
            cookie_file = work_path / "cookies_metadata.txt"
            cookie_file.write_text(cookie_str)
            cmd += ["--cookies", str(cookie_file)]

        result = subprocess.run(cmd, capture_output=True, text=True, timeout=90)
        if result.returncode != 0:
            log.warning(f"yt-dlp metadata fetch failed for {url}: {result.stderr[:300]}")
            return {}

        try:
            data = json.loads(result.stdout)
            if isinstance(data, dict):
                return data
        except Exception as e:
            log.warning(f"Failed parsing yt-dlp metadata for {url}: {e}")
        return {}

    def extract_metadata(self, video_path: Path) -> dict:
        """Extract video metadata using ffprobe."""
        cmd = [
            "ffprobe", "-v", "quiet",
            "-print_format", "json",
            "-show_format", "-show_streams",
            str(video_path),
        ]

        result = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        if result.returncode != 0:
            return {}

        probe = json.loads(result.stdout)
        fmt = probe.get("format", {})

        video_stream = next(
            (s for s in probe.get("streams", []) if s.get("codec_type") == "video"),
            {},
        )

        return {
            "title": fmt.get("tags", {}).get("title", video_path.stem),
            "duration": float(fmt.get("duration", 0)),
            "width": int(video_stream.get("width", 0)),
            "height": int(video_stream.get("height", 0)),
            "codec": video_stream.get("codec_name"),
            "bitrate": int(fmt.get("bit_rate", 0)),
        }

    def detect_scenes(self, video_path: Path, total_duration: float) -> list:
        """
        Find natural split points using audio silence detection.
        Falls back to fixed-interval splitting if no silence gaps found.
        """
        if total_duration <= MAX_CLIP_SECONDS:
            return [{"start": 0, "end": total_duration}]

        try:
            cmd = [
                "ffmpeg", "-threads", FFMPEG_THREADS,
                "-i", str(video_path),
                "-af", f"silencedetect=noise={SILENCE_NOISE_DB}dB:d={SILENCE_MIN_DURATION}",
                "-f", "null", "-",
            ]

            result = subprocess.run(
                cmd, capture_output=True, text=True, timeout=120
            )

            silence_midpoints = []
            silence_start = None
            for line in result.stderr.split("\n"):
                if "silence_start:" in line:
                    try:
                        silence_start = float(line.split("silence_start:")[1].strip().split()[0])
                    except (ValueError, IndexError):
                        silence_start = None
                elif "silence_end:" in line and silence_start is not None:
                    try:
                        silence_end = float(line.split("silence_end:")[1].strip().split()[0])
                        midpoint = (silence_start + silence_end) / 2
                        silence_midpoints.append(midpoint)
                    except (ValueError, IndexError):
                        pass
                    silence_start = None

            if silence_midpoints:
                split_points = [0.0] + silence_midpoints + [total_duration]
                split_points = sorted(set(split_points))
                segments = self._merge_scenes(split_points, total_duration)
                if segments:
                    return segments

        except Exception as e:
            log.warning(f"Silence detection failed, using fixed intervals: {e}")

        return self._fixed_split(total_duration)

    def _merge_scenes(self, scene_times: list, total_duration: float) -> list:
        """Merge scene boundaries into clips between MIN and MAX duration."""
        segments = []
        start = 0.0

        for i in range(1, len(scene_times)):
            duration = scene_times[i] - start

            if duration >= TARGET_CLIP_SECONDS:
                # This segment is long enough
                end = scene_times[i]
                if duration > MAX_CLIP_SECONDS:
                    # Too long, split at target duration
                    while start + TARGET_CLIP_SECONDS < end:
                        segments.append({
                            "start": round(start, 2),
                            "end": round(start + TARGET_CLIP_SECONDS, 2),
                        })
                        start += TARGET_CLIP_SECONDS
                    if end - start >= MIN_CLIP_SECONDS:
                        segments.append({"start": round(start, 2), "end": round(end, 2)})
                else:
                    segments.append({"start": round(start, 2), "end": round(end, 2)})
                start = end

        # Handle remaining content
        if total_duration - start >= MIN_CLIP_SECONDS:
            segments.append({"start": round(start, 2), "end": round(total_duration, 2)})

        return segments

    def _fixed_split(self, total_duration: float) -> list:
        """Split into fixed-length segments."""
        segments = []
        pos = 0.0
        while pos < total_duration:
            end = min(pos + TARGET_CLIP_SECONDS, total_duration)
            if end - pos >= MIN_CLIP_SECONDS:
                segments.append({"start": round(pos, 2), "end": round(end, 2)})
            pos = end
        return segments

    def _generate_text_embedding(self, text: str) -> bytes:
        """Generate a 384-dim text embedding, returned as raw float32 bytes."""
        if not text or not text.strip():
            return None
        vec = self.text_embedder.encode(text[:2000], normalize_embeddings=True)
        return np.array(vec, dtype=np.float32).tobytes()

    def _ensure_clip_model(self):
        """Lazy-load CLIP ViT-B-32 on first use."""
        if self._clip_model is not None:
            return
        try:
            import open_clip
            model, _, preprocess = open_clip.create_model_and_transforms(
                'ViT-B-32', pretrained='laion2b_s34b_b79k'
            )
            model.eval()
            self._clip_model = model
            self._clip_preprocess = preprocess
            self._clip_tokenizer = open_clip.get_tokenizer('ViT-B-32')
            log.info("CLIP ViT-B-32 model loaded")
        except Exception as e:
            log.warning(f"CLIP model load failed (visual embeddings disabled): {e}")

    def _extract_keyframes(self, clip_path: Path, n: int = 3) -> list:
        """Extract n keyframes from a clip at evenly-spaced timestamps."""
        from PIL import Image
        import io

        meta = self.extract_metadata(clip_path)
        duration = meta.get("duration", 0)
        if duration <= 0:
            return []

        frames = []
        positions = [duration * (i + 1) / (n + 1) for i in range(n)]
        for ts in positions:
            try:
                cmd = [
                    "ffmpeg", "-y", "-ss", str(ts),
                    "-i", str(clip_path),
                    "-frames:v", "1", "-f", "image2pipe",
                    "-vcodec", "png", "pipe:1",
                ]
                result = subprocess.run(cmd, capture_output=True, timeout=15)
                if result.returncode == 0 and result.stdout:
                    img = Image.open(io.BytesIO(result.stdout)).convert("RGB")
                    frames.append(img)
            except Exception:
                continue
        return frames

    def _generate_visual_embedding(self, clip_path: Path) -> bytes:
        """Generate a 512-dim CLIP visual embedding by averaging keyframe embeddings."""
        self._ensure_clip_model()
        if self._clip_model is None:
            return None

        import torch

        frames = self._extract_keyframes(clip_path, n=3)
        if not frames:
            return None

        try:
            images = torch.stack([self._clip_preprocess(f) for f in frames])
            with torch.no_grad():
                feats = self._clip_model.encode_image(images)
                feats = feats / feats.norm(dim=-1, keepdim=True)
            avg = feats.mean(dim=0)
            avg = avg / avg.norm()
            return avg.cpu().numpy().astype(np.float32).tobytes()
        except Exception as e:
            log.warning(f"Visual embedding generation failed: {e}")
            return None

    def _extract_topics(self, transcript: str, source_title: str = "") -> list:
        """Extract key topics from transcript using KeyBERT."""
        if not transcript or len(transcript.split()) < 10:
            return []
        text = f"{source_title}\n{transcript}".strip()
        try:
            keywords = self.kw_model.extract_keywords(
                text, keyphrase_ngram_range=(1, 2), stop_words='english',
                top_n=5, diversity=0.5, use_mmr=True,
            )
            return [kw for kw, score in keywords if score > 0.25][:5]
        except Exception as e:
            log.warning(f"Topic extraction failed: {e}")
            return []

    def _index_clip_fts(self, db, clip_id, title, transcript, platform, channel_name):
        """Insert into FTS5 table (replaces Meilisearch)."""
        try:
            db.execute(
                "INSERT INTO clips_fts(clip_id, title, transcript, platform, channel_name) VALUES (?, ?, ?, ?, ?)",
                (clip_id, title or '', (transcript or '')[:2000], platform or '', channel_name or ''),
            )
        except Exception as e:
            log.warning(f"FTS index failed for {clip_id}: {e}")

    def process_segment(
        self, db, source_file: Path, source_id: str,
        segment: dict, index: int, work_path: Path, metadata: dict
    ) -> str:
        """Process a single clip segment: transcode, thumbnail, transcribe, upload."""
        clip_id = str(uuid.uuid4())
        start = segment["start"]
        end = segment["end"]
        duration = end - start

        clip_filename = f"clip_{index:04d}.mp4"
        clip_path = work_path / clip_filename
        thumb_path = work_path / f"thumb_{index:04d}.jpg"

        try:
            # Transcode segment to vertical-friendly format
            self._transcode_clip(source_file, clip_path, start, duration, metadata)

            # Generate thumbnail
            self._generate_thumbnail(clip_path, thumb_path)

            # Transcribe audio
            transcript = self._transcribe(clip_path)

            # Extract topics
            topics = self._extract_topics(transcript, metadata.get("title", ""))
            topics = self._refine_topics_llm(transcript, topics)

            # Generate embeddings
            title_for_embed = self._generate_clip_title(transcript, metadata.get("title", ""), index)
            text_emb = self._generate_text_embedding(f"{title_for_embed} {transcript}")
            visual_emb = self._generate_visual_embedding(clip_path)

            clip_key = f"clips/{clip_id}/{clip_filename}"
            thumb_key = f"clips/{clip_id}/thumbnail.jpg"

            file_size = clip_path.stat().st_size

            self.minio.fput_object(MINIO_BUCKET, clip_key, str(clip_path), content_type="video/mp4")

            if thumb_path.exists():
                self.minio.fput_object(MINIO_BUCKET, thumb_key, str(thumb_path), content_type="image/jpeg")

            # Probe the output clip for dimensions
            clip_meta = self.extract_metadata(clip_path)

            expires_at = (datetime.utcnow() + timedelta(days=CLIP_TTL_DAYS)).strftime('%Y-%m-%dT%H:%M:%SZ')

            # Generate a title from the transcript or source
            title = self._generate_clip_title(transcript, metadata.get("title", ""), index)

            row = db.execute("SELECT platform, channel_name FROM sources WHERE id = ?", (source_id,)).fetchone()
            platform = row["platform"] if row else ""
            channel_name = row["channel_name"] if row else ""

            content_score = 0.5

            db.execute("BEGIN IMMEDIATE")
            db.execute("""
                INSERT INTO clips (
                    id, source_id, title, duration_seconds, start_time, end_time,
                    storage_key, thumbnail_key, width, height, file_size_bytes,
                    transcript, topics, content_score, expires_at, status
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')
            """, (
                clip_id, source_id, title, duration, start, end,
                clip_key, thumb_key,
                clip_meta.get("width", 0), clip_meta.get("height", 0),
                file_size, transcript, json.dumps(topics), content_score, expires_at,
            ))

            for topic_name in topics:
                try:
                    topic_id = self._resolve_or_create_topic(db, topic_name)
                    db.execute(
                        "INSERT OR IGNORE INTO clip_topics (clip_id, topic_id, confidence, source) VALUES (?, ?, 1.0, 'keybert')",
                        (clip_id, topic_id)
                    )
                except Exception as e:
                    log.warning(f"Failed to link clip {clip_id} to topic {topic_name}: {e}")

            self._index_clip_fts(db, clip_id, title, transcript, platform, channel_name)

            if text_emb or visual_emb:
                db.execute(
                    "INSERT OR REPLACE INTO clip_embeddings (clip_id, text_embedding, visual_embedding, model_version) VALUES (?, ?, ?, ?)",
                    (clip_id, text_emb, visual_emb, "minilm-v2+clip-vit-b32"),
                )

            db.execute("COMMIT")

            log.info(f"Clip {clip_id} created ({duration:.1f}s, topics={topics})")
            return clip_id

        except Exception as e:
            try:
                db.execute("ROLLBACK")
            except Exception:
                pass
            log.error(f"Failed to process segment {index}: {e}")
            return None

    def _transcode_clip(
        self, source: Path, output: Path,
        start: float, duration: float, metadata: dict
    ):
        """Transcode a segment, optimized for mobile viewing."""
        # Keep aspect ratio, target 720p max
        scale_filter = "scale='min(720,iw)':'min(1280,ih)':force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2"

        cmd = [
            "ffmpeg", "-y",
            "-threads", FFMPEG_THREADS,
            "-ss", str(start),
            "-i", str(source),
            "-t", str(duration),
            "-vf", scale_filter,
            "-c:v", "libx264",
            "-preset", "fast",
            "-crf", "23",
            "-threads", FFMPEG_THREADS,
            "-c:a", "aac",
            "-b:a", "128k",
            "-movflags", "+faststart",
            "-avoid_negative_ts", "make_zero",
            str(output),
        ]

        result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        if result.returncode != 0:
            raise RuntimeError(f"Transcode failed: {result.stderr[-500:]}")

    def _generate_thumbnail(self, clip_path: Path, thumb_path: Path):
        """Generate a thumbnail from the middle of the clip."""
        cmd = [
            "ffmpeg", "-y",
            "-threads", FFMPEG_THREADS,
            "-i", str(clip_path),
            "-vf", "thumbnail,scale=480:-1",
            "-frames:v", "1",
            str(thumb_path),
        ]
        subprocess.run(cmd, capture_output=True, timeout=60)

    def _transcribe(self, clip_path: Path) -> str:
        """Transcribe audio using faster-whisper."""
        try:
            segments, _ = self.whisper.transcribe(str(clip_path), language="en")
            return " ".join(seg.text.strip() for seg in segments)
        except Exception as e:
            log.warning(f"Transcription failed: {e}")
            return ""

    def _generate_clip_title(self, transcript: str, source_title: str, index: int) -> str:
        """Generate a title via LLM if available, otherwise fall back to heuristics."""
        try:
            from llm_client import generate_title
            llm_title = generate_title(transcript, source_title)
            if llm_title and len(llm_title) > 3:
                return llm_title
        except Exception:
            pass

        if transcript:
            words = transcript.split()[:10]
            if len(words) >= 3:
                return " ".join(words) + "..."

        if source_title:
            return f"{source_title} (Part {index + 1})"

        return f"Clip {index + 1}"

    def _refine_topics_llm(self, transcript: str, topics: list) -> list:
        """Optionally refine topics via LLM. Returns original on failure."""
        if not topics:
            return topics
        try:
            from llm_client import refine_topics
            refined = refine_topics(transcript, topics)
            if refined and isinstance(refined, list):
                return refined
        except Exception:
            pass
        return topics


if __name__ == "__main__":
    worker = Worker()
    worker.run()
