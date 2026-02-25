#!/usr/bin/env python3
"""
ClipFeed Score Updater
Periodically updates content scores, generates co-occurrence topic edges,
and maintains user profile embeddings.
"""
import math
import os
import signal
import struct
import time
import sqlite3
import logging
import threading
from collections import defaultdict
from pathlib import Path

import numpy as np

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
log = logging.getLogger("score_updater")

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
INTERVAL = int(os.getenv("SCORE_UPDATE_INTERVAL", "900"))
CO_OCCURRENCE_MIN_CLIPS = 3


def open_db():
    db = sqlite3.connect(DB_PATH, isolation_level=None)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    db.execute("PRAGMA foreign_keys=ON")
    db.row_factory = sqlite3.Row
    return db


def update_content_scores(db):
    """Update clip content_score from aggregate interaction signals."""
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


def generate_co_occurrence_edges(db):
    """
    Find topic pairs that appear together on the same clip >= CO_OCCURRENCE_MIN_CLIPS times.
    Upsert into topic_edges with relation='co_occurs' and PMI-proportional weight.
    """
    rows = db.execute("""
        SELECT a.topic_id AS t1, b.topic_id AS t2, COUNT(*) AS co_count
        FROM clip_topics a
        JOIN clip_topics b ON a.clip_id = b.clip_id AND a.topic_id < b.topic_id
        GROUP BY a.topic_id, b.topic_id
        HAVING co_count >= ?
    """, (CO_OCCURRENCE_MIN_CLIPS,)).fetchall()

    if not rows:
        log.info("No co-occurrence edges to generate")
        return

    total_clips = db.execute("SELECT COUNT(*) FROM clips WHERE status='ready'").fetchone()[0]
    if total_clips == 0:
        return

    topic_clip_counts = {}
    for r in db.execute("SELECT topic_id, COUNT(*) AS cnt FROM clip_topics GROUP BY topic_id").fetchall():
        topic_clip_counts[r["topic_id"]] = r["cnt"]

    edge_count = 0
    db.execute("BEGIN IMMEDIATE")
    for r in rows:
        t1, t2, co_count = r["t1"], r["t2"], r["co_count"]
        p_t1 = topic_clip_counts.get(t1, 1) / total_clips
        p_t2 = topic_clip_counts.get(t2, 1) / total_clips
        p_joint = co_count / total_clips
        if p_t1 > 0 and p_t2 > 0 and p_joint > 0:
            pmi = math.log(p_joint / (p_t1 * p_t2))
            weight = max(0.1, min(1.0, (pmi + 2) / 4))
        else:
            weight = 0.5

        db.execute("""
            INSERT INTO topic_edges (source_id, target_id, relation, weight)
            VALUES (?, ?, 'co_occurs', ?)
            ON CONFLICT(source_id, target_id) DO UPDATE
            SET weight = excluded.weight
        """, (t1, t2, weight))
        db.execute("""
            INSERT INTO topic_edges (source_id, target_id, relation, weight)
            VALUES (?, ?, 'co_occurs', ?)
            ON CONFLICT(source_id, target_id) DO UPDATE
            SET weight = excluded.weight
        """, (t2, t1, weight))
        edge_count += 1
    db.execute("COMMIT")
    log.info(f"Generated {edge_count} co-occurrence edges")


def update_user_embeddings(db):
    """
    Maintain a running average text embedding per user from their positively-interacted clips.
    Reads clip text_embedding BLOBs and averages them (float32 vectors).
    """
    users = db.execute("""
        SELECT DISTINCT i.user_id
        FROM interactions i
        WHERE i.action IN ('like', 'save', 'watch_full')
    """).fetchall()

    updated = 0
    for row in users:
        uid = row["user_id"]
        emb_rows = db.execute("""
            SELECT e.text_embedding
            FROM interactions i
            JOIN clip_embeddings e ON e.clip_id = i.clip_id
            WHERE i.user_id = ?
              AND i.action IN ('like', 'save', 'watch_full')
              AND e.text_embedding IS NOT NULL
        """, (uid,)).fetchall()

        if not emb_rows:
            continue

        vecs = []
        for er in emb_rows:
            blob = er["text_embedding"]
            if blob and len(blob) > 0 and len(blob) % 4 == 0:
                arr = np.frombuffer(blob, dtype=np.float32).copy()
                vecs.append(arr)

        if not vecs:
            continue

        avg = np.mean(vecs, axis=0).astype(np.float32)
        norm = np.linalg.norm(avg)
        if norm > 0:
            avg = avg / norm
        avg_blob = avg.tobytes()

        db.execute("""
            INSERT INTO user_embeddings (user_id, text_embedding, interaction_count, updated_at)
            VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
            ON CONFLICT(user_id) DO UPDATE
            SET text_embedding = excluded.text_embedding,
                interaction_count = excluded.interaction_count,
                updated_at = excluded.updated_at
        """, (uid, avg_blob, len(vecs)))
        updated += 1

    log.info(f"Updated user embeddings for {updated} users")


def main():
    db = open_db()
    log.info(f"Score updater started (interval={INTERVAL}s)")

    shutdown = threading.Event()

    def handle_signal(signum, frame):
        log.info("Received shutdown signal, finishing current cycle...")
        shutdown.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)

    elapsed = INTERVAL
    while not shutdown.is_set():
        Path("/tmp/health").touch(exist_ok=True)
        
        if elapsed >= INTERVAL:
            elapsed = 0
            try:
                update_content_scores(db)
            except Exception as e:
                log.error(f"Score update failed: {e}")
                try:
                    db.execute("ROLLBACK")
                except Exception:
                    pass

            try:
                generate_co_occurrence_edges(db)
            except Exception as e:
                log.error(f"Co-occurrence edge generation failed: {e}")
                try:
                    db.execute("ROLLBACK")
                except Exception:
                    pass

            try:
                update_user_embeddings(db)
            except Exception as e:
                log.error(f"User embedding update failed: {e}")

            # Reconnect on persistent errors
            try:
                db.execute("SELECT 1")
            except Exception:
                log.warning("DB connection lost, reconnecting")
                try:
                    db = open_db()
                except Exception:
                    pass

        shutdown.wait(10)
        elapsed += 10

    db.close()
    log.info("Score updater shut down cleanly")


if __name__ == "__main__":
    main()
