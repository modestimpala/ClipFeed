"""
Feature extraction for learning-to-rank training.
Extracts interaction-based features from the ClipFeed SQLite database.
"""

import logging
import sqlite3
from datetime import datetime, timezone
from collections import defaultdict

import numpy as np

log = logging.getLogger(__name__)

FEATURE_NAMES = [
    "content_score",
    "duration_seconds",
    "topic_count",
    "transcript_length",
    "age_hours",
    "file_size_bytes",
    "topic_overlap",
    "channel_affinity",
    "user_total_views",
    "user_avg_watch_percentage",
    "user_like_rate",
    "user_save_rate",
    "hours_since_last_session",
]


def _check_tables(conn: sqlite3.Connection) -> tuple[bool, str]:
    """Verify required tables exist. Returns (ok, error_message)."""
    required = {"interactions", "clips", "users"}
    cursor = conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table' AND name IN (?, ?, ?)",
        tuple(required),
    )
    found = {r[0] for r in cursor.fetchall()}
    missing = required - found
    if missing:
        return False, f"Missing tables: {missing}"
    return True, ""


def _parse_ts(ts: str | None) -> datetime | None:
    """Parse ISO timestamp to datetime. Returns None if invalid."""
    if not ts:
        return None
    try:
        return datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except (ValueError, TypeError):
        return None


def extract_features(db_path: str) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """
    Extract L2R features from the ClipFeed database.

    Connects to SQLite at db_path with WAL mode, queries interactions joined with
    clips, clip_topics, clip_embeddings, and user data. For each (user_id, clip_id)
    interaction, extracts the feature vector and assigns a label.

    Labels: 1.0 (like/save/watch_full), 0.0 (skip/dislike), 0.5 (view with
    watch_percentage < 0.3). Groups are by user_id for LambdaRank.

    Returns:
        features_array: (n_samples, n_features) float array
        labels_array: (n_samples,) float array
        group_sizes_array: (n_groups,) int array, one per user
    """
    conn = sqlite3.connect(db_path, isolation_level=None)
    conn.row_factory = sqlite3.Row
    try:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA busy_timeout=5000")
    except sqlite3.OperationalError as e:
        log.warning("Could not set WAL mode: %s", e)

    try:
        ok, err = _check_tables(conn)
        if not ok:
            log.error(err)
            return np.empty((0, len(FEATURE_NAMES))), np.empty(0), np.empty(0, dtype=int)

        # Check for optional tables
        cursor = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('clip_topics', 'user_topic_affinities', 'sources')"
        )
        optional_tables = {r[0] for r in cursor.fetchall()}
        has_clip_topics = "clip_topics" in optional_tables
        has_user_affinities = "user_topic_affinities" in optional_tables
        has_sources = "sources" in optional_tables

        # Base query: interactions with clip data
        if has_sources:
            rows = conn.execute("""
                SELECT
                    i.id AS interaction_id,
                    i.user_id,
                    i.clip_id,
                    i.action,
                    i.watch_percentage,
                    i.watch_duration_seconds,
                    i.created_at AS interaction_created_at,
                    c.content_score,
                    c.duration_seconds,
                    c.transcript,
                    c.file_size_bytes,
                    c.created_at AS clip_created_at,
                    c.source_id
                FROM interactions i
                JOIN clips c ON c.id = i.clip_id
                LEFT JOIN sources s ON s.id = c.source_id
                ORDER BY i.user_id, i.created_at
            """).fetchall()
        else:
            rows = conn.execute("""
                SELECT
                    i.id AS interaction_id,
                    i.user_id,
                    i.clip_id,
                    i.action,
                    i.watch_percentage,
                    i.watch_duration_seconds,
                    i.created_at AS interaction_created_at,
                    c.content_score,
                    c.duration_seconds,
                    c.transcript,
                    c.file_size_bytes,
                    c.created_at AS clip_created_at,
                    c.source_id
                FROM interactions i
                JOIN clips c ON c.id = i.clip_id
                ORDER BY i.user_id, i.created_at
            """).fetchall()

        if not rows:
            log.warning("No interactions found")
            return np.empty((0, len(FEATURE_NAMES))), np.empty(0), np.empty(0, dtype=int)

        # Precompute per-clip topic count and topic overlap
        clip_topic_count = defaultdict(int)
        clip_topics_map: dict[str, set[str]] = defaultdict(set)
        if has_clip_topics:
            for r in conn.execute("""
                SELECT clip_id, topic_id FROM clip_topics
            """).fetchall():
                clip_topic_count[r[0]] += 1
                clip_topics_map[r[0]].add(r[1])

        user_topics_map: dict[str, set[str]] = defaultdict(set)
        if has_user_affinities:
            for r in conn.execute("""
                SELECT user_id, topic_id FROM user_topic_affinities
            """).fetchall():
                user_topics_map[r[0]].add(r[1])

        # Channel mapping: source_id -> channel key for affinity
        source_channel: dict[str | None, str] = {}
        if has_sources:
            for r in conn.execute("""
                SELECT id, COALESCE(channel_id, channel_name, '') AS ch
                FROM sources
            """).fetchall():
                source_channel[r[0]] = r[1] or ""

        # User past stats (point-in-time): for each interaction, compute from
        # interactions before this one for the same user.
        # Also channel_affinity: past views from same channel.
        # hours_since_last_session: time since previous interaction.
        user_past_views: dict[str, list[float]] = defaultdict(list)  # watch_percentage
        user_past_likes: dict[str, int] = defaultdict(int)
        user_past_saves: dict[str, int] = defaultdict(int)
        user_past_total: dict[str, int] = defaultdict(int)
        user_channel_views: dict[tuple[str, str], int] = defaultdict(int)
        user_last_ts: dict[str, datetime | None] = {}

        samples: list[tuple[list[float], float]] = []
        current_user: str | None = None
        group_sizes: list[int] = []
        current_group = 0

        for row in rows:
            user_id = row["user_id"]
            clip_id = row["clip_id"]
            action = (row["action"] or "").lower()
            watch_pct = row["watch_percentage"]
            if watch_pct is None:
                watch_pct = 0.0
            else:
                watch_pct = float(watch_pct)

            # Label
            if action in ("like", "save", "watch_full", "share"):
                label = 1.0
            elif action in ("skip", "dislike"):
                label = 0.0
            elif action == "view":
                label = 0.5 if watch_pct < 0.3 else 1.0
            else:
                label = 0.5

            # Content features
            content_score = float(row["content_score"] or 0.5)
            duration_seconds = float(row["duration_seconds"] or 0.0)
            transcript = row["transcript"] or ""
            transcript_length = len(transcript)
            file_size_bytes = int(row["file_size_bytes"] or 0)
            source_id = row["source_id"]
            channel_key = source_channel.get(source_id, "")

            # Age
            clip_ts = _parse_ts(row["clip_created_at"])
            interaction_ts = _parse_ts(row["interaction_created_at"])
            if clip_ts and interaction_ts:
                if clip_ts.tzinfo is None:
                    clip_ts = clip_ts.replace(tzinfo=timezone.utc)
                if interaction_ts.tzinfo is None:
                    interaction_ts = interaction_ts.replace(tzinfo=timezone.utc)
                age_hours = (interaction_ts - clip_ts).total_seconds() / 3600.0
            else:
                age_hours = 0.0

            topic_count = clip_topic_count.get(clip_id, 0)
            clip_topic_ids = clip_topics_map.get(clip_id, set())
            user_topic_ids = user_topics_map.get(user_id, set())
            topic_overlap = len(clip_topic_ids & user_topic_ids)

            # User stats (from past interactions only)
            total_views = user_past_total.get(user_id, 0)
            if total_views > 0:
                pct_list = user_past_views.get(user_id, [])
                user_avg_watch = float(np.mean(pct_list)) if pct_list else 0.0
                user_like_rate = user_past_likes.get(user_id, 0) / total_views
                user_save_rate = user_past_saves.get(user_id, 0) / total_views
            else:
                user_avg_watch = 0.0
                user_like_rate = 0.0
                user_save_rate = 0.0

            channel_affinity = user_channel_views.get((user_id, channel_key), 0)

            last_ts = user_last_ts.get(user_id)
            if last_ts and interaction_ts:
                if interaction_ts.tzinfo is None:
                    interaction_ts = interaction_ts.replace(tzinfo=timezone.utc)
                hours_since = (interaction_ts - last_ts).total_seconds() / 3600.0
            else:
                hours_since = 24.0 * 7  # 1 week default for first interaction

            features = [
                content_score,
                duration_seconds,
                float(topic_count),
                float(transcript_length),
                age_hours,
                float(file_size_bytes),
                float(topic_overlap),
                float(channel_affinity),
                float(total_views),
                user_avg_watch,
                user_like_rate,
                user_save_rate,
                hours_since,
            ]

            samples.append((features, label))

            # Group by user
            if user_id != current_user:
                if current_group > 0:
                    group_sizes.append(current_group)
                current_user = user_id
                current_group = 0
            current_group += 1

            # Update rolling stats for next iteration (exclude current from past)
            if action in ("view", "watch_full"):
                user_past_views[user_id].append(watch_pct)
                user_past_total[user_id] = user_past_total.get(user_id, 0) + 1
                if channel_key:
                    user_channel_views[(user_id, channel_key)] = (
                        user_channel_views.get((user_id, channel_key), 0) + 1
                    )
            if action == "like":
                user_past_likes[user_id] = user_past_likes.get(user_id, 0) + 1
            if action == "save":
                user_past_saves[user_id] = user_past_saves.get(user_id, 0) + 1

            user_last_ts[user_id] = interaction_ts

        if current_group > 0:
            group_sizes.append(current_group)

        # Update channel_affinity to EXCLUDE current view (strict point-in-time)
        # We computed it before the current interaction was applied, so we're good.
        # Actually: we add to user_channel_views AFTER appending the sample, so
        # channel_affinity in the sample is the count BEFORE this view. Good.

        # Same for user_total_views, etc. - we use values before updating. Good.

        X = np.array([s[0] for s in samples], dtype=np.float64)
        y = np.array([s[1] for s in samples], dtype=np.float64)
        g = np.array(group_sizes, dtype=np.int32)

        log.info("Extracted %d samples, %d groups", len(samples), len(group_sizes))
        return X, y, g

    finally:
        conn.close()
