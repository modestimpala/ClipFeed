#!/usr/bin/env python3
import os
import time
import logging
import psycopg2

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("score_updater")

DB_URL = os.getenv("DATABASE_URL", "postgres://clipfeed:changeme@localhost:5432/clipfeed")
INTERVAL = int(os.getenv("SCORE_UPDATE_INTERVAL", "900"))


def main():
    db = psycopg2.connect(DB_URL)
    log.info(f"Score updater started (interval={INTERVAL}s)")
    while True:
        try:
            cur = db.cursor()
            cur.execute("SELECT update_content_scores()")
            count = cur.fetchone()[0]
            db.commit()
            cur.close()
            log.info(f"Updated scores for {count} clips")
        except Exception as e:
            log.error(f"Score update failed: {e}")
            try:
                db = psycopg2.connect(DB_URL)
            except Exception:
                pass
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
