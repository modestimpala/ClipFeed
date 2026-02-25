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
import hashlib
import base64
from pathlib import Path
from datetime import datetime, timedelta
from concurrent.futures import ThreadPoolExecutor
try:
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
except ImportError:
    AESGCM = None

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
JWT_SECRET = os.getenv("JWT_SECRET", "supersecretkey")

# HTTP worker mode (set WORKER_MODE=http to use the API instead of direct SQLite)
WORKER_MODE = os.getenv("WORKER_MODE", "direct")  # 'direct' or 'http'
WORKER_API_URL = os.getenv("WORKER_API_URL", "http://api:8080")
WORKER_SECRET = os.getenv("WORKER_SECRET", "")

# Clip splitting parameters
MIN_CLIP_SECONDS = int(os.getenv("MIN_CLIP_SECONDS", "15"))
MAX_CLIP_SECONDS = int(os.getenv("MAX_CLIP_SECONDS", "90"))
TARGET_CLIP_SECONDS = int(os.getenv("TARGET_CLIP_SECONDS", "45"))
MAX_VIDEO_DURATION = int(os.getenv("MAX_VIDEO_DURATION", "3600"))
MAX_DOWNLOAD_SIZE_MB = int(os.getenv("MAX_DOWNLOAD_SIZE_MB", "2048"))
PROCESSING_MODE = os.getenv("PROCESSING_MODE", "transcode")
SILENCE_NOISE_DB = -30
SILENCE_MIN_DURATION = 0.5

# Retry parameters
RETRY_BASE_DELAY = 30  # seconds; doubles each attempt (30s, 60s, 120s, …)
JOB_STALE_MINUTES = int(os.getenv("JOB_STALE_MINUTES", "15"))

shutdown = False


class JobCancelled(Exception):
    """Raised when a job has been cancelled by the user."""
    pass


class VideoRejected(Exception):
    """Raised for validation rejections (not transient errors) — skips retries."""
    pass


def signal_handler(sig, frame):
    global shutdown
    log.info("Shutdown signal received, finishing current jobs...")
    shutdown = True


signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)


def decrypt_cookie(encoded: str, secret: str) -> str | None:
    """Decrypt a cookie encrypted by the Go API (AES-256-GCM, nonce-prepended, base64).
    Returns None on any failure so the job can proceed without cookies."""
    if AESGCM is None:
        log.warning("cryptography package not installed — cannot decrypt cookies")
        return None
    try:
        key = hashlib.sha256(secret.encode()).digest()
        data = base64.b64decode(encoded)
        nonce_size = 12  # AES-GCM standard nonce length
        if len(data) < nonce_size:
            return None
        nonce, ciphertext = data[:nonce_size], data[nonce_size:]
        aesgcm = AESGCM(key)
        plaintext = aesgcm.decrypt(nonce, ciphertext, None)
        return plaintext.decode()
    except Exception as e:
        log.warning("Cookie decryption failed: %s", e)
        return None


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
    # Defaults so object.__new__(Worker) used by tests gets sane values
    http_mode = False
    api = None
    db = None

    def __init__(self):
        self.http_mode = WORKER_MODE.lower() == "http"

        if self.http_mode:
            from api_client import WorkerAPIClient
            if not WORKER_SECRET:
                raise ValueError("WORKER_SECRET is required when WORKER_MODE=http")
            self.api = WorkerAPIClient(WORKER_API_URL, WORKER_SECRET)
            log.info("Worker HTTP mode: connecting to %s", WORKER_API_URL)
            self.api.wait_for_api()
            self.db = None
        else:
            # Main-thread connection used only for job popping
            self.db = open_db()
            self.api = None

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

        if not self.http_mode:
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
            for slug, (tid, name, _) in topic_ids.items():
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
        """Atomically claim one pending job. Returns dict-like object or None."""
        if self.http_mode:
            job = self.api.claim_job()
            if job is None:
                return None
            # Return a dict that behaves like sqlite3.Row
            return {"id": job["id"], "payload": json.dumps(job["payload"]) if isinstance(job["payload"], dict) else job["payload"]}
        # Direct SQLite mode
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
                Path("/tmp/health").touch(exist_ok=True)
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
        if self.http_mode:
            return self.api.reclaim_stale_jobs(JOB_STALE_MINUTES)

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

    def _check_cancelled(self, db, job_id: str):
        """Check if a job has been cancelled by the user. Raises JobCancelled if so."""
        if self.http_mode:
            info = self.api.get_job(job_id)
            if info and info.get("status") == "cancelled":
                raise JobCancelled(f"Job {job_id} cancelled by user")
        else:
            row = db.execute("SELECT status FROM jobs WHERE id = ?", (job_id,)).fetchone()
            if row and row["status"] == "cancelled":
                raise JobCancelled(f"Job {job_id} cancelled by user")

    def process_job(self, job_id: str, payload: dict):
        """Process a job. Uses HTTP API or direct SQLite depending on mode."""
        db = None if self.http_mode else open_db()
        try:
            source_id = payload.get("source_id")
            platform = payload.get("platform", "")
            url = payload.get("url", "")

            self._update_source(db, source_id, status="downloading")

            work_path = WORK_DIR / job_id
            work_path.mkdir(parents=True, exist_ok=True)

            try:
                # Fetch platform cookie if applicable
                cookie_str = None
                if platform in ("youtube", "tiktok", "instagram", "twitter"):
                    cookie_str = self._get_cookie(db, source_id, platform)
                    if cookie_str:
                        log.info("Job %s: using platform cookie for %s", job_id[:8], platform)

                # Step 0: Fetch source metadata early so failed downloads still have context
                log.info("Job %s: [step 0/4] fetching source metadata for %s", job_id[:8], url[:80])
                source_metadata = self.fetch_source_metadata(url, work_path, cookie_str=cookie_str)
                if source_metadata:
                    duration = source_metadata.get("duration", 0)
                    if MAX_VIDEO_DURATION > 0 and duration > MAX_VIDEO_DURATION:
                        raise VideoRejected(f"Video too long ({duration}s, max {MAX_VIDEO_DURATION}s)")

                    try:
                        self._update_source(db, source_id,
                            external_id=source_metadata.get("id"),
                            title=source_metadata.get("title"),
                            channel_name=source_metadata.get("uploader") or source_metadata.get("channel"),
                            thumbnail_url=source_metadata.get("thumbnail"),
                            duration_seconds=source_metadata.get("duration"),
                            metadata=json.dumps(source_metadata),
                        )
                    except Exception as e:
                        err_str = str(e).lower()
                        if "duplicate" in err_str or "unique constraint" in err_str:
                            # Another source already has this external_id — skip it and continue
                            log.warning("Job %s: external_id %s already exists for platform %s, skipping external_id update",
                                        job_id[:8], source_metadata.get("id"), platform)
                            self._update_source(db, source_id,
                                title=source_metadata.get("title"),
                                channel_name=source_metadata.get("uploader") or source_metadata.get("channel"),
                                thumbnail_url=source_metadata.get("thumbnail"),
                                duration_seconds=source_metadata.get("duration"),
                                metadata=json.dumps(source_metadata),
                            )
                        else:
                            raise

                # Step 1: Download
                self._check_cancelled(db, job_id)
                log.info("Job %s: [step 1/4] downloading video", job_id[:8])
                dl_start = time.time()
                source_file = self.download(url, work_path, cookie_str=cookie_str)
                log.info("Job %s: download complete in %.1fs — %s", job_id[:8], time.time() - dl_start, source_file.name)
                self._update_source(db, source_id, status="processing")

                # Step 2: Extract metadata
                self._check_cancelled(db, job_id)
                log.info("Job %s: [step 2/4] extracting media metadata", job_id[:8])
                media_metadata = self.extract_metadata(source_file)
                merged_metadata = dict(source_metadata) if source_metadata else {}
                if media_metadata:
                    merged_metadata["media_probe"] = media_metadata
                self._update_source(db, source_id,
                    title=(source_metadata or {}).get("title") or media_metadata.get("title"),
                    duration_seconds=(source_metadata or {}).get("duration") or media_metadata.get("duration"),
                    metadata=json.dumps(merged_metadata),
                )

                # Step 3: Detect scenes and split
                self._check_cancelled(db, job_id)
                log.info("Job %s: [step 3/4] detecting scenes (duration=%.1fs)", job_id[:8], media_metadata.get("duration", 0))
                segments = self.detect_scenes(source_file, media_metadata.get("duration", 0))
                log.info("Job %s: detected %d segments", job_id[:8], len(segments))

                # Step 4: Process each segment
                self._check_cancelled(db, job_id)
                log.info("Job %s: [step 4/4] processing %d segments (transcode, transcribe, embed, upload)", job_id[:8], len(segments))
                clip_ids = []
                segment_metadata = dict(media_metadata)
                if source_metadata and source_metadata.get("title"):
                    segment_metadata["title"] = source_metadata.get("title")
                # Pass platform info for HTTP mode (avoids extra DB query in process_segment)
                segment_metadata["_platform"] = platform
                segment_metadata["_channel_name"] = (source_metadata or {}).get("uploader") or (source_metadata or {}).get("channel") or ""
                for i, seg in enumerate(segments):
                    clip_id = self.process_segment(
                        db, source_file, source_id, seg, i, work_path, segment_metadata
                    )
                    if clip_id:
                        clip_ids.append(clip_id)

                # Mark source complete
                self._update_source(db, source_id, status="complete")

                # Mark job complete
                self._complete_job(db, job_id, clip_ids)
                log.info("Job %s complete: %d clips created from %s", job_id[:8], len(clip_ids), url[:80])

            except VideoRejected as e:
                log.info("Job %s rejected: %s", job_id[:8], e)
                self._fail_or_reject_job(db, job_id, source_id, str(e), rejected=True)

            except JobCancelled:
                log.info("Job %s cancelled by user", job_id[:8])
                # Job status already set to 'cancelled' by the API; just clean up

            except Exception as e:
                self._handle_job_error(db, job_id, source_id, e)

            finally:
                # Cleanup working directory
                subprocess.run(["rm", "-rf", str(work_path)], check=False)

        except Exception as e:
            log.error(f"Fatal error processing job {job_id}: {e}")
        finally:
            if db:
                db.close()

    # --- DB/API abstraction helpers ---

    _ALLOWED_SOURCE_COLUMNS = frozenset({
        'status', 'title', 'channel_name', 'platform', 'duration_seconds',
        'error', 'thumbnail_url', 'last_checked_at', 'check_interval_hours',
        'force_check', 'external_id', 'metadata',
    })

    def _update_source(self, db, source_id, **fields):
        """Update source via direct DB or HTTP API."""
        if self.http_mode:
            self.api.update_source(source_id, **fields)
        else:
            sets, vals = [], []
            for k, v in fields.items():
                if k not in self._ALLOWED_SOURCE_COLUMNS:
                    raise ValueError(f"disallowed column in _update_source: {k}")
                sets.append(f"{k} = ?")
                vals.append(v)
            vals.append(source_id)
            db.execute(f"UPDATE sources SET {', '.join(sets)} WHERE id = ?", vals)

    def _get_cookie(self, db, source_id, platform):
        """Get decrypted platform cookie."""
        if self.http_mode:
            return self.api.get_cookie(source_id, platform)
        row = db.execute("""
            SELECT cookie_str FROM platform_cookies
            WHERE user_id = (SELECT submitted_by FROM sources WHERE id = ?)
              AND platform = ? AND is_active = 1
        """, (source_id, platform)).fetchone()
        if row:
            return decrypt_cookie(row["cookie_str"], JWT_SECRET)
        return None

    def _complete_job(self, db, job_id, clip_ids):
        """Mark a job as complete."""
        if self.http_mode:
            self.api.update_job(job_id, "complete",
                result={"clip_ids": clip_ids, "clip_count": len(clip_ids)})
        else:
            db.execute(
                "UPDATE jobs SET status = 'complete', completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), result = ? WHERE id = ?",
                (json.dumps({"clip_ids": clip_ids, "clip_count": len(clip_ids)}), job_id),
            )

    def _fail_or_reject_job(self, db, job_id, source_id, error_msg, rejected=False):
        """Mark a job as rejected or failed (terminal)."""
        status = "rejected" if rejected else "failed"
        if self.http_mode:
            self.api.update_job(job_id, status, error=error_msg)
            self.api.update_source(source_id, status=status)
        else:
            db.execute(
                "UPDATE jobs SET status = ?, error = ?, completed_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?",
                (status, error_msg, job_id),
            )
            db.execute(
                "UPDATE sources SET status = ? WHERE id = ?", (status, source_id)
            )

    def _handle_job_error(self, db, job_id, source_id, error):
        """Handle a transient job error: retry or permanently fail."""
        if self.http_mode:
            job_info = self.api.get_job(job_id)
            attempts = job_info.get("attempts", 0)
            max_attempts = job_info.get("max_attempts", 3)
        else:
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
                f"retrying in {delay}s: {error}"
            )
            if self.http_mode:
                self.api.update_job(job_id, "queued", error=str(error), run_after=run_after)
                self.api.update_source(source_id, status="pending")
            else:
                db.execute(
                    "UPDATE jobs SET status = 'queued', error = ?, run_after = ? WHERE id = ?",
                    (str(error), run_after, job_id),
                )
                db.execute(
                    "UPDATE sources SET status = 'pending' WHERE id = ?", (source_id,)
                )
        else:
            log.error(
                f"Job {job_id} permanently failed after {attempts} attempts: {error}"
            )
            if self.http_mode:
                self.api.update_job(job_id, "failed", error=str(error))
                self.api.update_source(source_id, status="failed")
            else:
                db.execute(
                    "UPDATE jobs SET status = 'failed', error = ? WHERE id = ?",
                    (str(error), job_id),
                )
                db.execute(
                    "UPDATE sources SET status = 'failed' WHERE id = ?", (source_id,)
                )

    def download(self, url: str, work_path: Path, cookie_str: str = None) -> Path:
        """Download video using yt-dlp."""
        output_template = str(work_path / "source.%(ext)s")

        cmd = [
            "yt-dlp",
            "--no-playlist",
            "--js-runtimes", "node",
            "--format",
            "bestvideo[height<=1080]+bestaudio/best[height<=1080]"
            "/bestvideo+bestaudio/best",
            "--merge-output-format", "mp4",
            "--max-filesize", f"{MAX_DOWNLOAD_SIZE_MB}M",
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
            log.info("Segment %d: transcoding %.1fs-%.1fs (%.1fs)", index, start, end, duration)
            self._transcode_clip(source_file, clip_path, start, duration, metadata)

            # Generate thumbnail
            self._generate_thumbnail(clip_path, thumb_path)

            # Transcribe audio
            log.info("Segment %d: transcribing audio", index)
            transcript = self._transcribe(clip_path)
            log.info("Segment %d: transcript length=%d words", index, len(transcript.split()) if transcript else 0)

            # Generate a title from the transcript or source (reused for embedding context below)
            title = self._generate_clip_title(transcript, metadata.get("title", ""), index)

            # Extract topics
            log.info("Segment %d: extracting topics via KeyBERT", index)
            topics = self._extract_topics(transcript, metadata.get("title", ""))
            log.info("Segment %d: KeyBERT topics=%s", index, topics)
            topics = self._refine_topics_llm(transcript, topics)

            # Generate embeddings
            log.info("Segment %d: generating embeddings (text + visual)", index)
            text_emb = self._generate_text_embedding(f"{title} {transcript}")
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

            # Get platform/channel from metadata (passed from process_job) or DB
            platform = metadata.get("_platform", "")
            channel_name = metadata.get("_channel_name", "")
            if not platform and db:
                row = db.execute("SELECT platform, channel_name FROM sources WHERE id = ?", (source_id,)).fetchone()
                platform = row["platform"] if row else ""
                channel_name = row["channel_name"] if row else ""

            content_score = 0.5

            if self.http_mode:
                # Single API call creates clip + topics + embeddings + FTS
                self.api.create_clip(
                    clip_id=clip_id,
                    source_id=source_id,
                    title=title,
                    duration_seconds=duration,
                    start_time=start,
                    end_time=end,
                    storage_key=clip_key,
                    thumbnail_key=thumb_key,
                    width=clip_meta.get("width", 0),
                    height=clip_meta.get("height", 0),
                    file_size_bytes=file_size,
                    transcript=transcript,
                    topics=topics,
                    content_score=content_score,
                    expires_at=expires_at,
                    platform=platform,
                    channel_name=channel_name,
                    text_embedding=text_emb,
                    visual_embedding=visual_emb,
                    model_version="minilm-v2+clip-vit-b32",
                )
            else:
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
            if db:
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
        """Transcode or copy a segment, optimized for mobile viewing or speed."""
        if PROCESSING_MODE == "copy":
            cmd = [
                "ffmpeg", "-y",
                "-ss", str(start),
                "-i", str(source),
                "-t", str(duration),
                "-c", "copy",
                "-movflags", "+faststart",
                str(output),
            ]
        else:
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
            log.info("[LLM] Generating title for segment %d via LLM (source=%r, transcript_len=%d)",
                     index, source_title[:60] if source_title else "", len(transcript or ""))
            llm_title = generate_title(transcript, source_title)
            if llm_title and len(llm_title) > 3:
                log.info("[LLM] Title generated for segment %d: %r", index, llm_title)
                return llm_title
            log.info("[LLM] Title generation returned empty/short for segment %d — falling back to heuristic", index)
        except Exception as e:
            log.warning("[LLM] Title generation failed for segment %d: %s — falling back to heuristic", index, e)

        if transcript:
            words = transcript.split()[:10]
            if len(words) >= 3:
                fallback = " ".join(words) + "..."
                log.debug("Title fallback (transcript excerpt) for segment %d: %r", index, fallback)
                return fallback

        if source_title:
            fallback = f"{source_title} (Part {index + 1})"
            log.debug("Title fallback (source title) for segment %d: %r", index, fallback)
            return fallback

        return f"Clip {index + 1}"

    def _refine_topics_llm(self, transcript: str, topics: list) -> list:
        """Optionally refine topics via LLM. Returns original on failure."""
        if not topics:
            return topics
        try:
            from llm_client import refine_topics
            log.info("[LLM] Refining topics via LLM: input=%s", topics)
            refined = refine_topics(transcript, topics)
            if refined and isinstance(refined, list):
                log.info("[LLM] Topics refined: %s -> %s", topics, refined)
                return refined
            log.info("[LLM] Topic refinement returned empty — keeping originals: %s", topics)
        except Exception as e:
            log.warning("[LLM] Topic refinement failed: %s — keeping originals: %s", e, topics)
        return topics


if __name__ == "__main__":
    worker = Worker()
    worker.run()
