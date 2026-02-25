#!/usr/bin/env python3
"""
Storage lifecycle manager.
Run via cron or `make lifecycle` to clean up expired clips.
"""

import os
import sqlite3
import logging
from minio import Minio

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("lifecycle")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
MINIO_ENDPOINT = os.getenv("MINIO_ENDPOINT", "localhost:9000")
MINIO_ACCESS = os.getenv("MINIO_ACCESS_KEY", "clipfeed")
MINIO_SECRET = os.getenv("MINIO_SECRET_KEY", "changeme123")
MINIO_BUCKET = os.getenv("MINIO_BUCKET", "clips")
MINIO_SSL = os.getenv("MINIO_USE_SSL", "false") == "true"
STORAGE_LIMIT_GB = float(os.getenv("STORAGE_LIMIT_GB", "50"))


def main():
    db = sqlite3.connect(DB_PATH)
    try:
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("PRAGMA busy_timeout=5000")
        db.row_factory = sqlite3.Row

        minio_client = Minio(
            MINIO_ENDPOINT,
            access_key=MINIO_ACCESS,
            secret_key=MINIO_SECRET,
            secure=MINIO_SSL,
        )

        # Phase 1: Delete expired clips that aren't protected
        expired = db.execute("""
            SELECT id, storage_key, thumbnail_key, file_size_bytes
            FROM clips
            WHERE expires_at < strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
              AND is_protected = 0
              AND status = 'ready'
        """).fetchall()

        deleted_count = 0
        freed_bytes = 0
        for clip in expired:
            try:
                if clip["storage_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
                if clip["thumbnail_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])

                db.execute("UPDATE clips SET status = 'expired' WHERE id = ?", (clip["id"],))
                deleted_count += 1
                freed_bytes += clip["file_size_bytes"] or 0
            except Exception as e:
                log.error(f"Failed to clean up clip {clip['id']}: {e}")

        log.info(f"Expired {deleted_count} clips, freed {freed_bytes / (1024**3):.2f} GB")

        # Phase 2: Check total storage usage and evict oldest if over limit
        total_bytes = db.execute(
            "SELECT COALESCE(SUM(file_size_bytes), 0) FROM clips WHERE status = 'ready'"
        ).fetchone()[0]
        total_gb = total_bytes / (1024 ** 3)

        if total_gb > STORAGE_LIMIT_GB:
            overage_bytes = total_bytes - int(STORAGE_LIMIT_GB * (1024 ** 3))
            log.info(f"Storage at {total_gb:.2f} GB (limit {STORAGE_LIMIT_GB} GB), need to free {overage_bytes / (1024**3):.2f} GB")

            candidates = db.execute("""
                SELECT id, storage_key, thumbnail_key, file_size_bytes
                FROM clips
                WHERE is_protected = 0 AND status = 'ready'
                ORDER BY created_at ASC
            """).fetchall()

            evicted = 0
            for clip in candidates:
                if overage_bytes <= 0:
                    break
                try:
                    if clip["storage_key"]:
                        minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
                    if clip["thumbnail_key"]:
                        minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])

                    db.execute("UPDATE clips SET status = 'evicted' WHERE id = ?", (clip["id"],))
                    overage_bytes -= clip["file_size_bytes"] or 0
                    evicted += 1
                except Exception as e:
                    log.error(f"Failed to evict clip {clip['id']}: {e}")

            log.info(f"Evicted {evicted} clips for storage management")

        # Phase 3: Clean up failed jobs older than 7 days
        db.execute("""
            DELETE FROM jobs
            WHERE status IN ('failed', 'complete')
                AND created_at < datetime('now', '-7 days')
        """)

        db.commit()
        log.info("Lifecycle cleanup complete")
    finally:
        db.close()


if __name__ == "__main__":
    main()
