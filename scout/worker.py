#!/usr/bin/env python3
"""
ClipFeed Content Scout Worker
Discovers, monitors, and evaluates video content for ingestion.
"""

import json
import logging
import os
import signal
import sqlite3
import subprocess
import time
import uuid

import ollama_client

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger("scout")

# Environment variables
DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
SCOUT_INTERVAL = int(os.getenv("SCOUT_INTERVAL", "21600"))
LLM_THRESHOLD = float(os.getenv("LLM_THRESHOLD", "6"))

shutdown = False


def signal_handler(sig, frame):
    global shutdown
    log.info("Shutdown signal received, finishing current run...")
    shutdown = True


signal.signal(signal.SIGINT, signal_handler)
signal.signal(signal.SIGTERM, signal_handler)


def open_db():
    """Open SQLite with WAL mode, busy_timeout, foreign_keys, row_factory."""
    db = sqlite3.connect(DB_PATH, isolation_level=None, check_same_thread=False)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA busy_timeout=5000")
    db.execute("PRAGMA foreign_keys=ON")
    db.row_factory = sqlite3.Row
    return db


def check_sources(db: sqlite3.Connection, source_ids: list[str] | None = None) -> None:
    """Query active scout sources, run yt-dlp, insert new candidates.
    If source_ids is provided, only check those sources (bypass interval check).
    """
    if source_ids:
        placeholders = ",".join("?" for _ in source_ids)
        cur = db.execute(
            f"SELECT id, source_type, platform, identifier, check_interval_hours "
            f"FROM scout_sources WHERE id IN ({placeholders})",
            source_ids,
        )
    else:
        cur = db.execute("""
            SELECT id, source_type, platform, identifier, check_interval_hours
            FROM scout_sources
            WHERE is_active = 1
              AND (last_checked IS NULL
                   OR last_checked < datetime('now', '-' || check_interval_hours || ' hours'))
        """)
    sources = cur.fetchall()

    for row in sources:
        if shutdown:
            return
        source_id = row["id"]
        source_type = row["source_type"]
        platform = row["platform"]
        identifier = row["identifier"]
        check_interval_hours = row["check_interval_hours"] or 24

        if source_type == "channel":
            cmd = [
                "yt-dlp",
                "--flat-playlist",
                "--dump-single-json",
                "--playlist-end", "20",
                identifier,
            ]
        elif source_type == "hashtag":
            cmd = [
                "yt-dlp",
                "--flat-playlist",
                "--dump-single-json",
                f"ytsearch20:{identifier}",
            ]
        elif source_type == "playlist":
            cmd = [
                "yt-dlp",
                "--flat-playlist",
                "--dump-single-json",
                "--playlist-end", "20",
                identifier,
            ]
        else:
            log.warning("Unknown source_type %r for source %s", source_type, source_id)
            continue

        try:
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                timeout=90,
            )
            if result.returncode != 0:
                log.warning("yt-dlp failed for %s: %s", source_id, result.stderr[:300])
                continue

            data = json.loads(result.stdout)
            entries = data.get("entries") if isinstance(data, dict) else []
            if not entries:
                entries = [data] if isinstance(data, dict) and data.get("id") else []

        except subprocess.TimeoutExpired:
            log.warning("yt-dlp timed out for source %s", source_id)
            continue
        except json.JSONDecodeError as e:
            log.warning("yt-dlp output parse error for %s: %s", source_id, e)
            continue

        inserted = 0
        for entry in entries:
            if shutdown:
                return
            if not isinstance(entry, dict):
                continue
            vid_id = entry.get("id")
            if not vid_id:
                continue

            external_id = str(vid_id)
            url = entry.get("url") or f"https://www.youtube.com/watch?v={vid_id}"
            title = entry.get("title") or ""
            channel_name = entry.get("uploader") or entry.get("channel") or ""
            duration_seconds = entry.get("duration")
            if duration_seconds is not None:
                try:
                    duration_seconds = float(duration_seconds)
                except (TypeError, ValueError):
                    duration_seconds = None

            exists = db.execute(
                """
                SELECT 1 FROM sources
                WHERE platform = ? AND external_id = ?
                UNION
                SELECT 1 FROM scout_candidates
                WHERE platform = ? AND external_id = ?
                """,
                (platform, external_id, platform, external_id),
            ).fetchone()

            if exists:
                continue

            try:
                db.execute(
                    """
                    INSERT INTO scout_candidates
                    (id, scout_source_id, url, platform, external_id, title, channel_name, duration_seconds, status)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending')
                    """,
                    (
                        str(uuid.uuid4()),
                        source_id,
                        url,
                        platform,
                        external_id,
                        title,
                        channel_name,
                        duration_seconds,
                    ),
                )
                inserted += 1
            except sqlite3.IntegrityError:
                continue

        db.execute(
            "UPDATE scout_sources SET last_checked = datetime('now') WHERE id = ?",
            (source_id,),
        )
        log.info("Scout source %s: discovered %d new candidates", source_id, inserted)


def evaluate_candidates(db: sqlite3.Connection) -> None:
    """Score pending candidates via Ollama, set approved/rejected."""
    if not ollama_client.is_available():
        log.info("Ollama unavailable, skipping candidate evaluation")
        return

    cur = db.execute(
        "SELECT id, url, platform, external_id, title, channel_name FROM scout_candidates WHERE status = 'pending'"
    )
    candidates = cur.fetchall()

    topic_rows = db.execute(
        "SELECT name FROM topics ORDER BY clip_count DESC LIMIT 10"
    ).fetchall()
    top_topics = [r["name"] for r in topic_rows]

    for row in candidates:
        if shutdown:
            return

        cand_id = row["id"]
        title = row["title"] or ""
        channel = row["channel_name"] or ""

        score = ollama_client.evaluate_candidate(title, channel, top_topics)
        db.execute(
            "UPDATE scout_candidates SET llm_score = ? WHERE id = ?",
            (score, cand_id),
        )

        if score >= LLM_THRESHOLD:
            db.execute(
                "UPDATE scout_candidates SET status = 'approved' WHERE id = ?",
                (cand_id,),
            )
            log.info("Candidate %s approved (score=%.1f)", cand_id[:8], score)
        else:
            db.execute(
                "UPDATE scout_candidates SET status = 'rejected' WHERE id = ?",
                (cand_id,),
            )
            log.debug("Candidate %s rejected (score=%.1f)", cand_id[:8], score)


def auto_approve(db: sqlite3.Connection) -> None:
    """Insert approved candidates into sources and jobs, mark ingested."""
    cur = db.execute(
        "SELECT id, url, platform, external_id, title, channel_name, duration_seconds FROM scout_candidates WHERE status = 'approved'"
    )
    candidates = cur.fetchall()

    for row in candidates:
        if shutdown:
            return

        cand_id = row["id"]
        url = row["url"]
        platform = row["platform"]
        external_id = row["external_id"]
        title = row["title"]
        channel_name = row["channel_name"]
        duration_seconds = row["duration_seconds"]

        source_id = str(uuid.uuid4())
        job_id = str(uuid.uuid4())

        try:
            db.execute(
                """
                INSERT INTO sources (id, url, platform, external_id, title, channel_name, duration_seconds, status)
                VALUES (?, ?, ?, ?, ?, ?, ?, 'pending')
                """,
                (source_id, url, platform, external_id, title, channel_name, duration_seconds),
            )
            db.execute(
                """
                INSERT INTO jobs (id, source_id, job_type, status, payload)
                VALUES (?, ?, 'download', 'queued', ?)
                """,
                (
                    job_id,
                    source_id,
                    json.dumps({"url": url, "source_id": source_id, "platform": platform}),
                ),
            )
            db.execute(
                "UPDATE scout_candidates SET status = 'ingested' WHERE id = ?",
                (cand_id,),
            )
            log.info("Ingested candidate %s -> source %s", cand_id[:8], source_id[:8])
        except sqlite3.IntegrityError as e:
            log.warning("Failed to ingest candidate %s (duplicate?): %s", cand_id[:8], e)
            db.execute(
                "UPDATE scout_candidates SET status = 'rejected' WHERE id = ?",
                (cand_id,),
            )


TRIGGER_POLL_INTERVAL = 10  # seconds between checks for force_check sources


def process_triggers(db: sqlite3.Connection) -> bool:
    """Check for force_check sources, process them, return True if any found."""
    cur = db.execute(
        "SELECT id FROM scout_sources WHERE force_check = 1"
    )
    triggered = [row["id"] for row in cur.fetchall()]
    if not triggered:
        return False

    log.info("Processing %d triggered source(s): %s", len(triggered),
             ", ".join(t[:8] for t in triggered))

    check_sources(db, source_ids=triggered)
    if not shutdown:
        evaluate_candidates(db)
    if not shutdown:
        auto_approve(db)

    db.execute(
        "UPDATE scout_sources SET force_check = 0 WHERE force_check = 1"
    )
    return True


def main():
    log.info(
        "Scout worker started (interval=%ds, threshold=%.1f, trigger_poll=%ds)",
        SCOUT_INTERVAL,
        LLM_THRESHOLD,
        TRIGGER_POLL_INTERVAL,
    )

    db = open_db()
    try:
        elapsed = 0
        while not shutdown:
            # Fast-poll: check for manually triggered sources
            try:
                process_triggers(db)
            except Exception as e:
                log.error("Trigger processing error: %s", e)

            # Full cycle when interval elapses
            if elapsed >= SCOUT_INTERVAL:
                elapsed = 0
                try:
                    log.info("Starting full scout cycle")
                    check_sources(db)
                    if shutdown:
                        break
                    evaluate_candidates(db)
                    if shutdown:
                        break
                    auto_approve(db)
                    log.info("Full scout cycle complete")
                except Exception as e:
                    log.error("Scout run error: %s", e)

            if shutdown:
                break

            time.sleep(TRIGGER_POLL_INTERVAL)
            elapsed += TRIGGER_POLL_INTERVAL

    finally:
        db.close()
        log.info("Scout worker shut down")


if __name__ == "__main__":
    main()
