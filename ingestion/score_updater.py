#!/usr/bin/env python3
import os
import time
import sqlite3
import logging

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("score_updater")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
INTERVAL = int(os.getenv("SCORE_UPDATE_INTERVAL", "900"))


def main():
    db = sqlite3.connect(DB_PATH)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    log.info(f"Score updater started (interval={INTERVAL}s)")

    while True:
        try:
            db.execute("BEGIN IMMEDIATE")
            db.execute("""
                UPDATE clips
                SET content_score = (
                    SELECT MAX(0.0, MIN(1.0,
                        COALESCE(AVG(CASE WHEN action='view' THEN watch_percentage END), 0.5) * 0.35
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='like'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.25
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='save'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.20
                        + COALESCE(
                            CAST(SUM(CASE WHEN action='watch_full' THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.15
                        - COALESCE(
                            CAST(SUM(CASE WHEN action='skip'       THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.30
                        - COALESCE(
                            CAST(SUM(CASE WHEN action='dislike'    THEN 1.0 ELSE 0 END) AS REAL)
                            / NULLIF(SUM(CASE WHEN action='view'   THEN 1   ELSE 0 END), 0), 0) * 0.15
                    ))
                    FROM interactions
                    WHERE interactions.clip_id = clips.id
                    HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
                )
                WHERE status = 'ready'
                  AND id IN (
                    SELECT clip_id FROM interactions
                    GROUP BY clip_id
                    HAVING SUM(CASE WHEN action='view' THEN 1 ELSE 0 END) >= 5
                  )
            """)
            count = db.execute("SELECT changes()").fetchone()[0]
            db.execute("COMMIT")
            log.info(f"Updated scores for {count} clips")
        except Exception as e:
            log.error(f"Score update failed: {e}")
            try:
                db.execute("ROLLBACK")
            except Exception:
                pass
            try:
                db = sqlite3.connect(DB_PATH)
                db.execute("PRAGMA journal_mode=WAL")
                db.execute("PRAGMA busy_timeout=5000")
            except Exception:
                pass
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
