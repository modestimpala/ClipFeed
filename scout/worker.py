#!/usr/bin/env python3
"""
ClipFeed Content Scout Worker
Discovers, monitors, and evaluates video content for ingestion.
"""

import json
import logging
import os
import random
import re
import signal
import sqlite3
import subprocess
import time
import uuid
from pathlib import Path
from collections import defaultdict

import llm_client

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger("scout")

# Environment variables
DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
SCOUT_INTERVAL = int(os.getenv("SCOUT_INTERVAL", "21600"))
LLM_THRESHOLD = float(os.getenv("LLM_THRESHOLD", "6"))
SCOUT_LLM_AUTO_PULL = (
    os.getenv("SCOUT_LLM_AUTO_PULL", "1")
).lower() not in ("0", "false", "no")
SCOUT_MAX_LLM_PER_CYCLE = int(os.getenv("SCOUT_MAX_LLM_PER_CYCLE", "40"))
SCOUT_MAX_LLM_PER_SOURCE = int(os.getenv("SCOUT_MAX_LLM_PER_SOURCE", "5"))
SCOUT_MAX_LLM_PER_CHANNEL = int(os.getenv("SCOUT_MAX_LLM_PER_CHANNEL", "3"))
SCOUT_EXPLORATION_RATIO = float(os.getenv("SCOUT_EXPLORATION_RATIO", "0.2"))

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


def _tokenize(text: str) -> set[str]:
    return {t for t in re.findall(r"[a-z0-9]+", (text or "").lower()) if len(t) >= 3}


def _duration_fit(duration_seconds: float | None) -> float:
    if duration_seconds is None:
        return 0.5
    d = float(duration_seconds)
    if d <= 15:
        return 0.2
    if d <= 60:
        return 0.6 + (d - 15) / 45 * 0.4
    if d <= 180:
        return 1.0 - (d - 60) / 120 * 0.2
    if d <= 600:
        return 0.8 - (d - 180) / 420 * 0.6
    return 0.1


def _heuristic_rank_score(row: sqlite3.Row, topic_tokens: set[str], channel_seen_count: int) -> float:
    title = row["title"] or ""
    channel = row["channel_name"] or ""
    text_tokens = _tokenize(f"{title} {channel}")
    overlap = len(text_tokens & topic_tokens) / max(1, len(topic_tokens))
    topic_score = min(1.0, overlap * 2.5)
    duration_score = _duration_fit(row["duration_seconds"])
    novelty_score = 1.0 / (1.0 + max(0, channel_seen_count))
    return 0.55 * topic_score + 0.25 * duration_score + 0.20 * novelty_score


def _pick_with_caps(
    rows: list[sqlite3.Row],
    limit: int,
    source_counts: defaultdict[str, int],
    channel_counts: defaultdict[str, int],
    selected_ids: set[str],
) -> list[sqlite3.Row]:
    picked: list[sqlite3.Row] = []
    for row in rows:
        if len(picked) >= limit:
            break
        cid = row["id"]
        if cid in selected_ids:
            continue
        source_id = row["scout_source_id"] or ""
        channel_key = (row["channel_name"] or "").strip().lower() or f"__nochannel__:{cid}"
        if source_counts[source_id] >= SCOUT_MAX_LLM_PER_SOURCE:
            continue
        if channel_counts[channel_key] >= SCOUT_MAX_LLM_PER_CHANNEL:
            continue
        picked.append(row)
        selected_ids.add(cid)
        source_counts[source_id] += 1
        channel_counts[channel_key] += 1
    return picked


def check_sources(db: sqlite3.Connection, source_ids: list[str] | None = None) -> None:
    """Query active scout sources, run yt-dlp, insert new candidates.
    If source_ids is provided, only check those sources (bypass interval check).

    For hashtag/search sources, uses LLM-generated queries that combine the
    source identifier with the user's interests to discover fresh content.
    """
    if source_ids:
        placeholders = ",".join("?" for _ in source_ids)
        cur = db.execute(
            f"SELECT id, user_id, source_type, platform, identifier, check_interval_hours "
            f"FROM scout_sources WHERE id IN ({placeholders})",
            source_ids,
        )
    else:
        cur = db.execute("""
            SELECT id, user_id, source_type, platform, identifier, check_interval_hours
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
        user_id = row["user_id"]
        source_type = row["source_type"]
        platform = row["platform"]
        identifier = row["identifier"]
        check_interval_hours = row["check_interval_hours"] or 24

        # Build user profile for LLM-powered query generation
        user_profile_summary = None
        if user_id:
            profile = _build_user_profile(db, user_id)
            user_profile_summary = profile.get("profile_summary")

        if source_type == "channel":
            # For channels, fetch latest uploads (more results)
            cmds = [[
                "yt-dlp",
                "--flat-playlist",
                "--dump-single-json",
                "--playlist-end", "30",
                identifier,
            ]]
        elif source_type == "hashtag":
            # Use LLM to generate varied search queries
            # Get previously used queries to avoid repeats
            prev_queries_rows = db.execute(
                """
                SELECT DISTINCT title FROM scout_candidates
                WHERE scout_source_id = ? AND title LIKE '[query:%'
                ORDER BY created_at DESC LIMIT 10
                """,
                (source_id,),
            ).fetchall()
            existing_queries = [
                r["title"].replace("[query:", "").rstrip("]")
                for r in prev_queries_rows
            ]

            queries = llm_client.generate_search_queries(
                identifier=identifier,
                source_type=source_type,
                user_profile=user_profile_summary,
                existing_queries=existing_queries,
                count=4,
            )

            log.info("Scout source %s: using search queries: %s", source_id[:8], queries)

            # Build a yt-dlp command per query, each fetching 10 results
            cmds = []
            for query in queries:
                cmds.append([
                    "yt-dlp",
                    "--flat-playlist",
                    "--dump-single-json",
                    f"ytsearch10:{query}",
                ])

        elif source_type == "playlist":
            cmds = [[
                "yt-dlp",
                "--flat-playlist",
                "--dump-single-json",
                "--playlist-end", "30",
                identifier,
            ]]
        else:
            log.warning("Unknown source_type %r for source %s", source_type, source_id)
            continue

        total_inserted = 0
        seen_external_ids: set[str] = set()

        for cmd in cmds:
            if shutdown:
                return

            try:
                result = subprocess.run(
                    cmd,
                    capture_output=True,
                    text=True,
                    timeout=90,
                )
                if result.returncode != 0:
                    log.warning("yt-dlp failed for %s (cmd=%s): %s",
                                source_id, cmd[-1][:50], result.stderr[:300])
                    continue

                data = json.loads(result.stdout)
                entries = data.get("entries") if isinstance(data, dict) else []
                if not entries:
                    entries = [data] if isinstance(data, dict) and data.get("id") else []

            except subprocess.TimeoutExpired:
                log.warning("yt-dlp timed out for source %s (cmd=%s)", source_id, cmd[-1][:50])
                continue
            except json.JSONDecodeError as e:
                log.warning("yt-dlp output parse error for %s: %s", source_id, e)
                continue

            for entry in entries:
                if shutdown:
                    return
                if not isinstance(entry, dict):
                    continue
                vid_id = entry.get("id")
                if not vid_id:
                    continue

                external_id = str(vid_id)

                # Skip duplicates within this check cycle
                if external_id in seen_external_ids:
                    continue
                seen_external_ids.add(external_id)

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
                    total_inserted += 1
                except sqlite3.IntegrityError:
                    continue

        db.execute(
            "UPDATE scout_sources SET last_checked = datetime('now') WHERE id = ?",
            (source_id,),
        )
        log.info("Scout source %s (%s/%s): discovered %d new candidates across %d queries",
                 source_id[:8], source_type, identifier[:40] if identifier else "",
                 total_inserted, len(cmds))


def _build_user_profile(db: sqlite3.Connection, user_id: str) -> dict:
    """Build a personalized interest profile for a user from their interactions,
    topic weights, and viewing history. Returns a dict with:
      - top_topics: list of topic names ranked by engagement
      - favorite_channels: list of channel names ranked by interaction count
      - explicit_weights: dict of topic_name -> weight from user preferences
      - threshold: per-user scout threshold
      - auto_ingest: whether user wants auto-ingest
      - profile_summary: string summary for LLM consumption
    """
    # Liked/saved topic engagement
    topic_rows = db.execute(
        """
        SELECT t.name, COUNT(*) AS cnt,
               COALESCE(uta.weight, 1.0) AS user_weight
        FROM interactions i
        JOIN clips c ON i.clip_id = c.id
        JOIN clip_topics ct ON ct.clip_id = c.id
        JOIN topics t ON ct.topic_id = t.id
        LEFT JOIN user_topic_affinities uta ON uta.topic_id = t.id AND uta.user_id = i.user_id
        WHERE i.user_id = ?
          AND i.action IN ('like', 'save', 'share')
        GROUP BY t.id
        ORDER BY (cnt * COALESCE(uta.weight, 1.0)) DESC
        LIMIT 15
        """,
        (user_id,),
    ).fetchall()
    top_topics = [r["name"] for r in topic_rows]

    # Watched topic engagement (broader signal — includes view/complete)
    if not top_topics:
        watched_rows = db.execute(
            """
            SELECT t.name, COUNT(*) AS cnt
            FROM interactions i
            JOIN clips c ON i.clip_id = c.id
            JOIN clip_topics ct ON ct.clip_id = c.id
            JOIN topics t ON ct.topic_id = t.id
            WHERE i.user_id = ?
              AND i.action IN ('view', 'complete')
            GROUP BY t.id
            ORDER BY cnt DESC
            LIMIT 10
            """,
            (user_id,),
        ).fetchall()
        top_topics = [r["name"] for r in watched_rows]

    # Favorite channels
    channel_rows = db.execute(
        """
        SELECT s.channel_name, COUNT(*) AS cnt
        FROM interactions i
        JOIN clips c ON i.clip_id = c.id
        JOIN sources s ON c.source_id = s.id
        WHERE i.user_id = ?
          AND i.action IN ('like', 'save', 'share', 'complete')
          AND s.channel_name IS NOT NULL AND TRIM(s.channel_name) <> ''
        GROUP BY s.channel_name
        ORDER BY cnt DESC
        LIMIT 10
        """,
        (user_id,),
    ).fetchall()
    favorite_channels = [r["channel_name"] for r in channel_rows]

    # Explicit topic weights from user preferences
    explicit_weights: dict = {}
    row = db.execute(
        "SELECT COALESCE(topic_weights, '{}') FROM user_preferences WHERE user_id = ?",
        (user_id,),
    ).fetchone()
    if row:
        try:
            explicit_weights = json.loads(row[0])
        except (json.JSONDecodeError, TypeError):
            pass

    # Boosted/hidden topics from explicit weights
    boosted = [name for name, w in explicit_weights.items() if isinstance(w, (int, float)) and w > 1.0]
    hidden = [name for name, w in explicit_weights.items() if isinstance(w, (int, float)) and w <= 0]

    # Per-user threshold and auto-ingest setting
    threshold = LLM_THRESHOLD
    auto_ingest = True
    pref_row = db.execute(
        "SELECT COALESCE(scout_threshold, 6.0), COALESCE(scout_auto_ingest, 1) FROM user_preferences WHERE user_id = ?",
        (user_id,),
    ).fetchone()
    if pref_row:
        threshold = float(pref_row[0])
        auto_ingest = bool(pref_row[1])

    # Build a natural language summary for the LLM
    parts = []
    if top_topics:
        parts.append(f"Favorite topics: {', '.join(top_topics[:10])}")
    if boosted:
        parts.append(f"Explicitly boosted: {', '.join(boosted[:5])}")
    if hidden:
        parts.append(f"Not interested in: {', '.join(hidden[:5])}")
    if favorite_channels:
        parts.append(f"Favorite channels: {', '.join(favorite_channels[:5])}")
    if not parts:
        # Fall back to global popular topics
        fallback_rows = db.execute(
            "SELECT name FROM topics ORDER BY clip_count DESC LIMIT 10"
        ).fetchall()
        fallback_topics = [r["name"] for r in fallback_rows]
        if fallback_topics:
            parts.append(f"Popular topics on this platform: {', '.join(fallback_topics)}")
            top_topics = fallback_topics

    profile_summary = ". ".join(parts) if parts else "No preference data available"

    return {
        "top_topics": top_topics,
        "favorite_channels": favorite_channels,
        "explicit_weights": explicit_weights,
        "boosted": boosted,
        "hidden": hidden,
        "threshold": threshold,
        "auto_ingest": auto_ingest,
        "profile_summary": profile_summary,
    }


def evaluate_candidates(db: sqlite3.Connection) -> None:
    """Score pending candidates via LLM with personalized user profiles and diversity caps."""
    if not llm_client.is_available():
        log.info("[LLM] Provider unavailable — skipping candidate evaluation")
        return
    if not llm_client.ensure_model(auto_pull=SCOUT_LLM_AUTO_PULL):
        log.info("[LLM] Model/config unavailable — skipping candidate evaluation")
        return

    # Get all pending candidates with their owning user
    cur = db.execute(
        """
        SELECT sc.id, sc.scout_source_id, sc.url, sc.platform, sc.external_id,
               sc.title, sc.channel_name, sc.duration_seconds, sc.created_at,
               ss.user_id
        FROM scout_candidates sc
        JOIN scout_sources ss ON sc.scout_source_id = ss.id
        WHERE sc.status = 'pending'
        """
    )
    candidates = cur.fetchall()
    if not candidates:
        return

    # Group candidates by user for personalized evaluation
    user_candidates: dict[str, list] = defaultdict(list)
    for c in candidates:
        uid = c["user_id"] or "__global__"
        user_candidates[uid].append(c)

    channel_seen = {
        r["channel_name"]: r["cnt"]
        for r in db.execute(
            """
            SELECT channel_name, COUNT(*) AS cnt
            FROM sources
            WHERE channel_name IS NOT NULL AND TRIM(channel_name) <> ''
            GROUP BY channel_name
            """
        ).fetchall()
    }

    total_evaluated = 0
    total_approved = 0
    total_rejected = 0
    total_failed = 0

    for user_id, user_cands in user_candidates.items():
        if shutdown:
            return

        # Build personalized profile
        if user_id != "__global__":
            profile = _build_user_profile(db, user_id)
        else:
            # Fallback: global popular topics
            topic_rows = db.execute(
                "SELECT name FROM topics ORDER BY clip_count DESC LIMIT 10"
            ).fetchall()
            profile = {
                "top_topics": [r["name"] for r in topic_rows],
                "favorite_channels": [],
                "explicit_weights": {},
                "boosted": [],
                "hidden": [],
                "threshold": LLM_THRESHOLD,
                "auto_ingest": True,
                "profile_summary": f"Popular topics: {', '.join(r['name'] for r in topic_rows)}" if topic_rows else "No data",
            }

        topic_tokens = _tokenize(" ".join(profile["top_topics"]))
        user_threshold = profile["threshold"]

        log.info("[Scout] User %s: profile=%r threshold=%.1f candidates=%d",
                 user_id[:8] if user_id != "__global__" else "global",
                 profile["profile_summary"][:120], user_threshold, len(user_cands))

        # Rank and select candidates
        ranked = sorted(
            user_cands,
            key=lambda row: _heuristic_rank_score(row, topic_tokens, channel_seen.get(row["channel_name"] or "", 0)),
            reverse=True,
        )

        max_eval = min(max(1, SCOUT_MAX_LLM_PER_CYCLE), len(ranked))
        exploration_ratio = max(0.0, min(0.5, SCOUT_EXPLORATION_RATIO))
        explore_slots = min(max_eval, int(round(max_eval * exploration_ratio)))
        exploit_slots = max_eval - explore_slots

        source_counts: defaultdict[str, int] = defaultdict(int)
        channel_counts: defaultdict[str, int] = defaultdict(int)
        selected_ids: set[str] = set()

        selected = _pick_with_caps(ranked, exploit_slots, source_counts, channel_counts, selected_ids)

        remaining = [r for r in ranked if r["id"] not in selected_ids]
        random.shuffle(remaining)
        selected.extend(_pick_with_caps(remaining, explore_slots, source_counts, channel_counts, selected_ids))

        if not selected:
            log.info("No pending candidates for user %s passed diversity caps",
                     user_id[:8] if user_id != "__global__" else "global")
            continue

        log.info(
            "[LLM] User %s: evaluating %d/%d (exploit=%d, explore=%d)",
            user_id[:8] if user_id != "__global__" else "global",
            len(selected), len(user_cands), exploit_slots, explore_slots,
        )

        for row in selected:
            if shutdown:
                return

            cand_id = row["id"]
            title = row["title"] or ""
            channel = row["channel_name"] or ""

            log.info("[LLM] Scoring candidate %s: title=%r channel=%r", cand_id[:8], title[:80], channel)
            score = llm_client.evaluate_candidate(
                title, channel, profile["top_topics"],
                user_profile=profile["profile_summary"],
            )
            if score is None:
                log.warning("[LLM] Candidate %s: evaluation failed (no score returned)", cand_id[:8])
                total_failed += 1
                continue

            total_evaluated += 1
            db.execute(
                "UPDATE scout_candidates SET llm_score = ? WHERE id = ?",
                (score, cand_id),
            )

            if score >= user_threshold:
                db.execute(
                    "UPDATE scout_candidates SET status = 'approved' WHERE id = ?",
                    (cand_id,),
                )
                total_approved += 1
                log.info("[LLM] Candidate %s APPROVED (score=%.1f >= threshold=%.1f): %r",
                         cand_id[:8], score, user_threshold, title[:80])
            else:
                db.execute(
                    "UPDATE scout_candidates SET status = 'rejected' WHERE id = ?",
                    (cand_id,),
                )
                total_rejected += 1
                log.info("[LLM] Candidate %s rejected (score=%.1f < threshold=%.1f): %r",
                         cand_id[:8], score, user_threshold, title[:80])

    log.info("[LLM] Evaluation cycle complete: evaluated=%d approved=%d rejected=%d failed=%d",
             total_evaluated, total_approved, total_rejected, total_failed)


def auto_approve(db: sqlite3.Connection) -> None:
    """Insert approved candidates into sources and jobs, mark ingested.
    Respects per-user scout_auto_ingest preference — if disabled, leaves
    candidates as 'approved' for manual review.
    """
    cur = db.execute(
        """
        SELECT sc.id, sc.url, sc.platform, sc.external_id, sc.title,
               sc.channel_name, sc.duration_seconds, ss.user_id
        FROM scout_candidates sc
        JOIN scout_sources ss ON sc.scout_source_id = ss.id
        WHERE sc.status = 'approved'
        """
    )
    candidates = cur.fetchall()

    # Cache user auto-ingest preferences
    auto_ingest_cache: dict[str, bool] = {}

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
        user_id = row["user_id"]

        # Check per-user auto-ingest setting
        if user_id and user_id not in auto_ingest_cache:
            pref_row = db.execute(
                "SELECT COALESCE(scout_auto_ingest, 1) FROM user_preferences WHERE user_id = ?",
                (user_id,),
            ).fetchone()
            auto_ingest_cache[user_id] = bool(pref_row[0]) if pref_row else True

        if user_id and not auto_ingest_cache.get(user_id, True):
            log.info("Candidate %s approved but auto-ingest disabled for user %s — awaiting manual review",
                     cand_id[:8], user_id[:8])
            continue

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
        "Scout worker started — interval=%ds threshold=%.1f trigger_poll=%ds "
        "max_llm_per_cycle=%d max_per_source=%d max_per_channel=%d exploration=%.0f%% auto_pull=%s",
        SCOUT_INTERVAL,
        LLM_THRESHOLD,
        TRIGGER_POLL_INTERVAL,
        SCOUT_MAX_LLM_PER_CYCLE,
        SCOUT_MAX_LLM_PER_SOURCE,
        SCOUT_MAX_LLM_PER_CHANNEL,
        SCOUT_EXPLORATION_RATIO * 100,
        SCOUT_LLM_AUTO_PULL,
    )

    db = open_db()
    try:
        elapsed = 0
        while not shutdown:
            Path("/tmp/health").touch(exist_ok=True)
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
