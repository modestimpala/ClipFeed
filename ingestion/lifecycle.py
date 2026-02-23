#!/usr/bin/env python3
"""
Storage lifecycle manager.
Run via cron or systemd timer to clean up expired clips.
"""

import os
import json
import logging
import psycopg2
import psycopg2.extras
from minio import Minio

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("lifecycle")

DB_URL = os.getenv("DATABASE_URL", "postgres://clipfeed:changeme@localhost:5432/clipfeed")
MINIO_ENDPOINT = os.getenv("MINIO_ENDPOINT", "localhost:9000")
MINIO_ACCESS = os.getenv("MINIO_ACCESS_KEY", "clipfeed")
MINIO_SECRET = os.getenv("MINIO_SECRET_KEY", "changeme123")
MINIO_BUCKET = os.getenv("MINIO_BUCKET", "clips")
MINIO_SSL = os.getenv("MINIO_USE_SSL", "false") == "true"
STORAGE_LIMIT_GB = float(os.getenv("STORAGE_LIMIT_GB", "50"))


def main():
    db = psycopg2.connect(DB_URL)
    db.autocommit = True
    cur = db.cursor(cursor_factory=psycopg2.extras.RealDictCursor)

    minio_client = Minio(
        MINIO_ENDPOINT,
        access_key=MINIO_ACCESS,
        secret_key=MINIO_SECRET,
        secure=MINIO_SSL,
    )

    # Phase 1: Delete expired clips that aren't protected
    cur.execute("""
        SELECT id, storage_key, thumbnail_key, file_size_bytes
        FROM clips
        WHERE expires_at < now()
          AND NOT is_protected
          AND status = 'ready'
    """)

    expired = cur.fetchall()
    deleted_count = 0
    freed_bytes = 0

    for clip in expired:
        try:
            if clip["storage_key"]:
                minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
            if clip["thumbnail_key"]:
                minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])

            cur.execute("UPDATE clips SET status = 'expired' WHERE id = %s", (clip["id"],))
            deleted_count += 1
            freed_bytes += clip["file_size_bytes"] or 0

        except Exception as e:
            log.error(f"Failed to clean up clip {clip['id']}: {e}")

    log.info(f"Expired {deleted_count} clips, freed {freed_bytes / (1024**3):.2f} GB")

    # Phase 2: Check total storage usage and evict oldest if over limit
    cur.execute("SELECT COALESCE(SUM(file_size_bytes), 0) as total FROM clips WHERE status = 'ready'")
    total_bytes = cur.fetchone()["total"]
    total_gb = total_bytes / (1024 ** 3)

    if total_gb > STORAGE_LIMIT_GB:
        overage_bytes = total_bytes - int(STORAGE_LIMIT_GB * (1024 ** 3))
        log.info(f"Storage at {total_gb:.2f} GB (limit {STORAGE_LIMIT_GB} GB), need to free {overage_bytes / (1024**3):.2f} GB")

        # Evict oldest unprotected clips first
        cur.execute("""
            SELECT id, storage_key, thumbnail_key, file_size_bytes
            FROM clips
            WHERE NOT is_protected AND status = 'ready'
            ORDER BY created_at ASC
        """)

        evicted = 0
        for clip in cur.fetchall():
            if overage_bytes <= 0:
                break

            try:
                if clip["storage_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["storage_key"])
                if clip["thumbnail_key"]:
                    minio_client.remove_object(MINIO_BUCKET, clip["thumbnail_key"])

                cur.execute("UPDATE clips SET status = 'evicted' WHERE id = %s", (clip["id"],))
                overage_bytes -= clip["file_size_bytes"] or 0
                evicted += 1

            except Exception as e:
                log.error(f"Failed to evict clip {clip['id']}: {e}")

        log.info(f"Evicted {evicted} clips for storage management")

    # Phase 3: Clean up failed jobs older than 7 days
    cur.execute("""
        DELETE FROM jobs
        WHERE status IN ('failed', 'complete')
          AND created_at < now() - interval '7 days'
    """)

    cur.close()
    db.close()
    log.info("Lifecycle cleanup complete")


if __name__ == "__main__":
    main()
