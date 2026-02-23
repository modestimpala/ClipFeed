#!/usr/bin/env python3
"""
ClipFeed Ingestion Worker
Processes video sources: download -> analyze -> split -> transcode -> transcribe -> upload
"""

import os
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

from minio import Minio
from faster_whisper import WhisperModel
from keybert import KeyBERT

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
CLIP_TTL_DAYS = int(os.getenv("CLIP_TTL_DAYS", "30"))
WORK_DIR = Path(os.getenv("WORK_DIR", "/tmp/clipfeed"))

# Clip splitting parameters
MIN_CLIP_SECONDS = 15
MAX_CLIP_SECONDS = 90
TARGET_CLIP_SECONDS = 45
SCENE_THRESHOLD = 0.3

shutdown = False


def signal_handler(sig, frame):
    global shutdown
    log.info("Shutdown signal received, finishing current jobs...")
    shutdown = True


signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)


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

        self.whisper = WhisperModel(WHISPER_MODEL, device="cpu", compute_type="int8")
        self.kw_model = KeyBERT(model='all-MiniLM-L6-v2')

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
        with ThreadPoolExecutor(max_workers=MAX_CONCURRENT) as pool:
            while not shutdown:
                try:
                    row = self._pop_job()
                    if row is None:
                        time.sleep(2)
                        continue
                    job_id = row["id"]
                    payload = json.loads(row["payload"])
                    log.info(f"Claimed job {job_id}")
                    pool.submit(self.process_job, job_id, payload)
                except Exception as e:
                    log.error(f"Job pop failed: {e}")
                    time.sleep(5)

        log.info("Worker shut down")

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
                if platform in ("tiktok", "instagram", "twitter"):
                    row = db.execute("""
                        SELECT cookie_str FROM platform_cookies
                        WHERE user_id = (SELECT submitted_by FROM sources WHERE id = ?)
                          AND platform = ? AND is_active = 1
                    """, (source_id, platform)).fetchone()
                    if row:
                        cookie_str = row["cookie_str"]

                # Step 1: Download
                source_file = self.download(url, work_path, cookie_str=cookie_str)
                db.execute("UPDATE sources SET status = 'processing' WHERE id = ?", (source_id,))

                # Step 2: Extract metadata
                metadata = self.extract_metadata(source_file)
                db.execute(
                    "UPDATE sources SET title = ?, duration_seconds = ?, metadata = ? WHERE id = ?",
                    (metadata.get("title"), metadata.get("duration"), json.dumps(metadata), source_id),
                )

                # Step 3: Detect scenes and split
                segments = self.detect_scenes(source_file, metadata.get("duration", 0))

                # Step 4: Process each segment
                clip_ids = []
                for i, seg in enumerate(segments):
                    clip_id = self.process_segment(
                        db, source_file, source_id, seg, i, work_path, metadata
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
                log.error(f"Job {job_id} failed: {e}")
                db.execute(
                    "UPDATE jobs SET status = 'failed', error = ? WHERE id = ?",
                    (str(e), job_id),
                )
                db.execute("UPDATE sources SET status = 'failed' WHERE id = ?", (source_id,))

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
        Detect scene changes to find natural split points.
        Falls back to fixed-interval splitting if scene detection fails.
        """
        if total_duration <= MAX_CLIP_SECONDS:
            return [{"start": 0, "end": total_duration}]

        try:
            # Use ffmpeg scene detection
            cmd = [
                "ffmpeg", "-i", str(video_path),
                "-filter:v", f"select='gt(scene,{SCENE_THRESHOLD})',showinfo",
                "-f", "null", "-",
            ]

            result = subprocess.run(
                cmd, capture_output=True, text=True, timeout=300
            )

            # Parse scene change timestamps
            scene_times = [0.0]
            for line in result.stderr.split("\n"):
                if "pts_time:" in line:
                    try:
                        pts = float(line.split("pts_time:")[1].split()[0])
                        scene_times.append(pts)
                    except (ValueError, IndexError):
                        continue

            scene_times.append(total_duration)
            scene_times = sorted(set(scene_times))

            # Merge scene boundaries into clips of appropriate length
            segments = self._merge_scenes(scene_times, total_duration)
            if segments:
                return segments

        except Exception as e:
            log.warning(f"Scene detection failed, using fixed intervals: {e}")

        # Fallback: fixed interval splitting
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

            self._index_clip_fts(db, clip_id, title, transcript, platform, channel_name)
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
        scale_filter = "scale='min(720,iw)':'min(1280,ih)':force_original_aspect_ratio=decrease"

        cmd = [
            "ffmpeg", "-y",
            "-ss", str(start),
            "-i", str(source),
            "-t", str(duration),
            "-vf", scale_filter,
            "-c:v", "libx264",
            "-preset", "fast",
            "-crf", "23",
            "-c:a", "aac",
            "-b:a", "128k",
            "-movflags", "+faststart",
            "-avoid_negative_ts", "make_zero",
            str(output),
        ]

        result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        if result.returncode != 0:
            raise RuntimeError(f"Transcode failed: {result.stderr[:300]}")

    def _generate_thumbnail(self, clip_path: Path, thumb_path: Path):
        """Generate a thumbnail from the middle of the clip."""
        cmd = [
            "ffmpeg", "-y",
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
        """Generate a reasonable title for the clip."""
        if transcript:
            words = transcript.split()[:10]
            if len(words) >= 3:
                return " ".join(words) + "..."

        if source_title:
            return f"{source_title} (Part {index + 1})"

        return f"Clip {index + 1}"


if __name__ == "__main__":
    worker = Worker()
    worker.run()
