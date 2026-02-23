#!/usr/bin/env python3
"""
ClipFeed Ingestion Worker
Processes video sources: download -> analyze -> split -> transcode -> transcribe -> upload
"""

import os
import sys
import json
import time
import uuid
import signal
import logging
import subprocess
import tempfile
from pathlib import Path
from datetime import datetime, timedelta
from concurrent.futures import ThreadPoolExecutor

import redis
import psycopg2
import psycopg2.extras
from minio import Minio
from faster_whisper import WhisperModel
from keybert import KeyBERT
import meilisearch

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s"
)
log = logging.getLogger("worker")

# Configuration
DB_URL = os.getenv("DATABASE_URL", "postgres://clipfeed:changeme@localhost:5432/clipfeed")
REDIS_URL = os.getenv("REDIS_URL", "redis://localhost:6379")
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


class Worker:
    def __init__(self):
        self.db = psycopg2.connect(DB_URL)
        self.db.autocommit = True
        self.rdb = redis.from_url(REDIS_URL)
        self.minio = Minio(
            MINIO_ENDPOINT,
            access_key=MINIO_ACCESS,
            secret_key=MINIO_SECRET,
            secure=MINIO_SSL,
        )
        WORK_DIR.mkdir(parents=True, exist_ok=True)

        if not self.minio.bucket_exists(MINIO_BUCKET):
            self.minio.make_bucket(MINIO_BUCKET)

        # Whisper transcription model
        self.whisper = WhisperModel(WHISPER_MODEL, device="cpu", compute_type="int8")

        # Topic extraction model
        self.kw_model = KeyBERT(model='all-MiniLM-L6-v2')

        # Meilisearch client
        self.meili = meilisearch.Client(
            os.getenv("MEILI_URL", "http://meilisearch:7700"),
            os.getenv("MEILI_KEY", "")
        )
        self.meili_index = self.meili.index('clips')

    def run(self):
        log.info(f"Worker started (max_concurrent={MAX_CONCURRENT})")
        with ThreadPoolExecutor(max_workers=MAX_CONCURRENT) as pool:
            while not shutdown:
                job_id = self.rdb.blpop("clipfeed:jobs", timeout=5)
                if job_id is None:
                    continue

                job_id = job_id[1].decode("utf-8")
                log.info(f"Processing job {job_id}")
                pool.submit(self.process_job, job_id)

        log.info("Worker shut down")

    def process_job(self, job_id: str):
        cur = self.db.cursor(cursor_factory=psycopg2.extras.RealDictCursor)
        try:
            cur.execute(
                "UPDATE jobs SET status = 'running', started_at = now(), attempts = attempts + 1 WHERE id = %s RETURNING *",
                (job_id,),
            )
            job = cur.fetchone()
            if not job:
                log.warning(f"Job {job_id} not found")
                return

            payload = job["payload"] if isinstance(job["payload"], dict) else json.loads(job["payload"])
            source_id = payload.get("source_id") or str(job["source_id"])
            platform = payload.get("platform", "")

            cur.execute("UPDATE sources SET status = 'downloading' WHERE id = %s", (source_id,))

            work_path = WORK_DIR / job_id
            work_path.mkdir(parents=True, exist_ok=True)

            try:
                # Fetch platform cookie if applicable
                cookie_str = None
                if platform in ("tiktok", "instagram", "twitter"):
                    cur.execute("""
                        SELECT cookie_str FROM platform_cookies
                        WHERE user_id = (SELECT submitted_by FROM sources WHERE id = %s)
                          AND platform = %s AND is_active = true
                    """, (source_id, platform))
                    row = cur.fetchone()
                    if row:
                        cookie_str = row["cookie_str"]

                # Step 1: Download
                source_file = self.download(payload["url"], work_path, cookie_str=cookie_str)
                cur.execute("UPDATE sources SET status = 'processing' WHERE id = %s", (source_id,))

                # Step 2: Extract metadata
                metadata = self.extract_metadata(source_file)
                cur.execute(
                    "UPDATE sources SET title = %s, duration_seconds = %s, metadata = %s WHERE id = %s",
                    (metadata.get("title"), metadata.get("duration"), json.dumps(metadata), source_id),
                )

                # Step 3: Detect scenes and split
                segments = self.detect_scenes(source_file, metadata.get("duration", 0))

                # Step 4: Process each segment
                clip_ids = []
                for i, seg in enumerate(segments):
                    clip_id = self.process_segment(
                        cur, source_file, source_id, seg, i, work_path, metadata
                    )
                    if clip_id:
                        clip_ids.append(clip_id)

                # Mark source complete
                cur.execute("UPDATE sources SET status = 'complete' WHERE id = %s", (source_id,))

                # Mark job complete
                cur.execute(
                    "UPDATE jobs SET status = 'complete', completed_at = now(), result = %s WHERE id = %s",
                    (json.dumps({"clip_ids": clip_ids, "clip_count": len(clip_ids)}), job_id),
                )
                log.info(f"Job {job_id} complete: {len(clip_ids)} clips created")

            except Exception as e:
                log.error(f"Job {job_id} failed: {e}")
                cur.execute(
                    "UPDATE jobs SET status = 'failed', error = %s WHERE id = %s",
                    (str(e), job_id),
                )
                cur.execute("UPDATE sources SET status = 'failed' WHERE id = %s", (source_id,))

            finally:
                # Cleanup working directory
                subprocess.run(["rm", "-rf", str(work_path)], check=False)

        except Exception as e:
            log.error(f"Fatal error processing job {job_id}: {e}")
        finally:
            cur.close()

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

        # Write Netscape cookie file if cookies provided
        cookie_file = None
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

    def _index_clip(self, clip_id, title, transcript, topics, platform,
                    channel_name, duration, score):
        """Index clip in Meilisearch for full-text search."""
        try:
            self.meili_index.add_documents([{
                'id': clip_id,
                'title': title,
                'transcript': transcript[:2000],
                'topics': topics,
                'platform': platform,
                'channel_name': channel_name or '',
                'duration_seconds': round(duration, 1),
                'content_score': score,
                'created_at': int(time.time()),
            }])
        except Exception as e:
            log.warning(f"Failed to index clip {clip_id}: {e}")

    def process_segment(
        self, cur, source_file: Path, source_id: str,
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

            # Upload to MinIO
            clip_key = f"clips/{clip_id}/{clip_filename}"
            thumb_key = f"clips/{clip_id}/thumbnail.jpg"

            file_size = clip_path.stat().st_size

            self.minio.fput_object(
                MINIO_BUCKET, clip_key, str(clip_path),
                content_type="video/mp4",
            )

            if thumb_path.exists():
                self.minio.fput_object(
                    MINIO_BUCKET, thumb_key, str(thumb_path),
                    content_type="image/jpeg",
                )

            # Probe the output clip for dimensions
            clip_meta = self.extract_metadata(clip_path)

            # Determine expiry
            expires_at = datetime.utcnow() + timedelta(days=CLIP_TTL_DAYS)

            # Generate a title from the transcript or source
            title = self._generate_clip_title(transcript, metadata.get("title", ""), index)

            # Get source info for indexing
            cur.execute("SELECT platform, channel_name FROM sources WHERE id = %s", (source_id,))
            source_row = cur.fetchone()
            platform = source_row["platform"] if source_row else ""
            channel_name = source_row["channel_name"] if source_row else ""

            content_score = 0.5

            # Insert clip record
            cur.execute("""
                INSERT INTO clips (
                    id, source_id, title, duration_seconds, start_time, end_time,
                    storage_key, thumbnail_key, width, height, file_size_bytes,
                    transcript, topics, content_score, expires_at, status
                ) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, 'ready')
            """, (
                clip_id, source_id, title, duration, start, end,
                clip_key, thumb_key,
                clip_meta.get("width", 0), clip_meta.get("height", 0),
                file_size, transcript, topics, content_score, expires_at,
            ))

            # Index in Meilisearch
            self._index_clip(clip_id, title, transcript, topics, platform,
                             channel_name, duration, content_score)

            log.info(f"Clip {clip_id} created ({duration:.1f}s, topics={topics})")
            return clip_id

        except Exception as e:
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
