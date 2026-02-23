"""Unit tests for the content score updater.

Tests the SQL scoring formula against known interaction patterns
using an in-memory SQLite database.
"""

import sqlite3
import unittest


SCHEMA = """
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT UNIQUE NOT NULL,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT,
    avatar_url TEXT,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE sources (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    platform TEXT NOT NULL,
    status TEXT DEFAULT 'pending',
    submitted_by TEXT REFERENCES users(id),
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE clips (
    id TEXT PRIMARY KEY,
    source_id TEXT REFERENCES sources(id),
    title TEXT,
    duration_seconds REAL NOT NULL,
    storage_key TEXT NOT NULL,
    content_score REAL DEFAULT 0.5,
    status TEXT DEFAULT 'processing',
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE interactions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    clip_id TEXT NOT NULL REFERENCES clips(id),
    action TEXT NOT NULL,
    watch_duration_seconds REAL,
    watch_percentage REAL,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
"""

SCORE_UPDATE_SQL = """
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
"""


def make_db():
    db = sqlite3.connect(":memory:", isolation_level=None)
    db.execute("PRAGMA journal_mode=WAL")
    db.execute("PRAGMA foreign_keys=ON")
    db.executescript(SCHEMA)
    return db


def seed_user(db, user_id="u1"):
    db.execute(
        "INSERT OR IGNORE INTO users (id, username, email, password_hash) VALUES (?, ?, ?, ?)",
        (user_id, f"user_{user_id}", f"{user_id}@test.com", "hash"),
    )


def seed_clip(db, clip_id="clip1", score=0.5):
    db.execute(
        "INSERT OR IGNORE INTO sources (id, url, platform) VALUES (?, 'http://x.com', 'direct')",
        (f"src-{clip_id}",),
    )
    db.execute(
        "INSERT OR IGNORE INTO clips (id, source_id, duration_seconds, storage_key, content_score, status) "
        "VALUES (?, ?, 30.0, 'key', ?, 'ready')",
        (clip_id, f"src-{clip_id}", score),
    )


def add_interaction(db, clip_id, user_id, action, watch_pct=None, interaction_id=None):
    iid = interaction_id or f"{clip_id}-{user_id}-{action}-{id(action)}"
    db.execute(
        "INSERT INTO interactions (id, user_id, clip_id, action, watch_percentage) VALUES (?, ?, ?, ?, ?)",
        (iid, user_id, clip_id, action, watch_pct),
    )


def run_score_update(db):
    db.execute("BEGIN IMMEDIATE")
    db.execute(SCORE_UPDATE_SQL)
    count = db.execute("SELECT changes()").fetchone()[0]
    db.execute("COMMIT")
    return count


def get_score(db, clip_id):
    return db.execute("SELECT content_score FROM clips WHERE id = ?", (clip_id,)).fetchone()[0]


class TestScoreUpdater(unittest.TestCase):

    def test_needs_minimum_5_views(self):
        """Clips with fewer than 5 views are not updated."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c1", score=0.5)
        for i in range(4):
            add_interaction(db, "c1", "u1", "view", watch_pct=1.0, interaction_id=f"v{i}")

        count = run_score_update(db)
        self.assertEqual(count, 0)
        self.assertAlmostEqual(get_score(db, "c1"), 0.5)

    def test_all_positive_engagement(self):
        """High engagement: 100% watch, all likes, saves, watch_full → high score."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c2", score=0.5)

        for i in range(5):
            add_interaction(db, "c2", "u1", "view", watch_pct=1.0, interaction_id=f"v{i}")
            add_interaction(db, "c2", "u1", "like", interaction_id=f"l{i}")
            add_interaction(db, "c2", "u1", "save", interaction_id=f"s{i}")
            add_interaction(db, "c2", "u1", "watch_full", interaction_id=f"w{i}")

        run_score_update(db)
        score = get_score(db, "c2")
        # 0.35*1.0 + 0.25*1.0 + 0.20*1.0 + 0.15*1.0 = 0.95
        self.assertAlmostEqual(score, 0.95, places=2)

    def test_all_negative_engagement(self):
        """Low engagement: 0% watch, all skips and dislikes → score clamped to 0."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c3", score=0.5)

        for i in range(5):
            add_interaction(db, "c3", "u1", "view", watch_pct=0.0, interaction_id=f"v{i}")
            add_interaction(db, "c3", "u1", "skip", interaction_id=f"sk{i}")
            add_interaction(db, "c3", "u1", "dislike", interaction_id=f"d{i}")

        run_score_update(db)
        score = get_score(db, "c3")
        # 0.35*0.0 + 0 + 0 + 0 - 0.30*1.0 - 0.15*1.0 = -0.45, clamped to 0
        self.assertAlmostEqual(score, 0.0, places=2)

    def test_mixed_engagement(self):
        """Mixed signals: partial watch, some likes, some skips."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c4", score=0.5)

        for i in range(5):
            add_interaction(db, "c4", "u1", "view", watch_pct=0.6, interaction_id=f"v{i}")

        # 2 out of 5 liked, 1 skipped
        add_interaction(db, "c4", "u1", "like", interaction_id="l0")
        add_interaction(db, "c4", "u1", "like", interaction_id="l1")
        add_interaction(db, "c4", "u1", "skip", interaction_id="sk0")

        run_score_update(db)
        score = get_score(db, "c4")
        # 0.35*0.6 + 0.25*(2/5) + 0.20*0 + 0.15*0 - 0.30*(1/5) - 0.15*0
        # = 0.21 + 0.10 + 0 + 0 - 0.06 - 0 = 0.25
        self.assertAlmostEqual(score, 0.25, places=2)

    def test_only_ready_clips_updated(self):
        """Clips not in 'ready' status are skipped."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c5", score=0.5)
        db.execute("UPDATE clips SET status = 'processing' WHERE id = 'c5'")

        for i in range(5):
            add_interaction(db, "c5", "u1", "view", watch_pct=1.0, interaction_id=f"v{i}")
            add_interaction(db, "c5", "u1", "like", interaction_id=f"l{i}")

        count = run_score_update(db)
        self.assertEqual(count, 0)
        self.assertAlmostEqual(get_score(db, "c5"), 0.5)

    def test_score_clamped_to_one(self):
        """Score cannot exceed 1.0."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "c6", score=0.5)

        for i in range(10):
            add_interaction(db, "c6", "u1", "view", watch_pct=1.0, interaction_id=f"v{i}")
            add_interaction(db, "c6", "u1", "like", interaction_id=f"l{i}")
            add_interaction(db, "c6", "u1", "save", interaction_id=f"s{i}")
            add_interaction(db, "c6", "u1", "watch_full", interaction_id=f"w{i}")

        run_score_update(db)
        score = get_score(db, "c6")
        self.assertLessEqual(score, 1.0)

    def test_multiple_clips_updated_independently(self):
        """Each clip's score is calculated from its own interactions."""
        db = make_db()
        seed_user(db)
        seed_clip(db, "good")
        seed_clip(db, "bad")

        for i in range(5):
            add_interaction(db, "good", "u1", "view", watch_pct=1.0, interaction_id=f"gv{i}")
            add_interaction(db, "good", "u1", "like", interaction_id=f"gl{i}")
            add_interaction(db, "bad", "u1", "view", watch_pct=0.0, interaction_id=f"bv{i}")
            add_interaction(db, "bad", "u1", "skip", interaction_id=f"bs{i}")

        run_score_update(db)
        good_score = get_score(db, "good")
        bad_score = get_score(db, "bad")
        self.assertGreater(good_score, bad_score)


if __name__ == "__main__":
    unittest.main()
