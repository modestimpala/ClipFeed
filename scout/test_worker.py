"""Unit tests for pure / near-pure functions in scout/worker.py.

Functions tested
----------------
* _tokenize
* _duration_fit
* _heuristic_rank_score
* _pick_with_caps
* auto_approve
"""

import json
import sqlite3
import sys
import types
import unittest
from collections import defaultdict
from pathlib import Path
from unittest.mock import MagicMock

# worker.py does ``import llm_client`` at module level, but llm_client.py lives
# in ingestion/ and is copied into the Docker image at build time.  For unit
# tests we inject a lightweight stub before importing worker so the import
# succeeds without requiring the real module to be on sys.path.
if "llm_client" not in sys.modules:
    _stub = types.ModuleType("llm_client")
    _stub.generate_summary = MagicMock(return_value=("", "stub-model", None))
    sys.modules["llm_client"] = _stub

# Allow ``import worker`` regardless of how the test is invoked.
sys.path.insert(0, str(Path(__file__).parent))

from worker import (
    _duration_fit,
    _heuristic_rank_score,
    _pick_with_caps,
    _tokenize,
    auto_approve,
    SCOUT_MAX_LLM_PER_SOURCE,
    SCOUT_MAX_LLM_PER_CHANNEL,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Minimal schema for auto_approve tests.  Only the tables / columns touched by
# that function are included so the schema stays compact and test-focused.
_AUTO_APPROVE_SCHEMA = """
CREATE TABLE IF NOT EXISTS sources (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    platform TEXT NOT NULL,
    external_id TEXT,
    title TEXT,
    channel_name TEXT,
    duration_seconds REAL,
    status TEXT DEFAULT 'pending'
);

CREATE TABLE IF NOT EXISTS jobs (
    id TEXT PRIMARY KEY,
    source_id TEXT NOT NULL REFERENCES sources(id),
    job_type TEXT NOT NULL,
    status TEXT NOT NULL,
    payload TEXT
);

CREATE TABLE IF NOT EXISTS scout_sources (
    id TEXT PRIMARY KEY,
    user_id TEXT,
    source_type TEXT,
    platform TEXT,
    identifier TEXT,
    is_active INTEGER DEFAULT 1,
    force_check INTEGER DEFAULT 0,
    check_interval_hours INTEGER DEFAULT 6
);

CREATE TABLE IF NOT EXISTS scout_candidates (
    id TEXT PRIMARY KEY,
    scout_source_id TEXT NOT NULL REFERENCES scout_sources(id),
    url TEXT NOT NULL,
    platform TEXT,
    external_id TEXT,
    title TEXT,
    channel_name TEXT,
    duration_seconds REAL,
    status TEXT DEFAULT 'pending'
);

CREATE TABLE IF NOT EXISTS user_preferences (
    user_id TEXT PRIMARY KEY,
    scout_auto_ingest INTEGER DEFAULT 1
);
"""


def _make_db() -> sqlite3.Connection:
    db = sqlite3.connect(":memory:", isolation_level=None)
    db.execute("PRAGMA foreign_keys=ON")
    db.row_factory = sqlite3.Row
    db.executescript(_AUTO_APPROVE_SCHEMA)
    return db


def _seed_scout_source(db, source_id="ss1", user_id=None, platform="youtube"):
    db.execute(
        "INSERT OR IGNORE INTO scout_sources (id, user_id, platform) VALUES (?, ?, ?)",
        (source_id, user_id, platform),
    )


def _seed_candidate(db, cand_id, scout_source_id="ss1", status="approved", **kwargs):
    db.execute(
        """INSERT OR IGNORE INTO scout_candidates
           (id, scout_source_id, url, platform, external_id, title, channel_name, duration_seconds, status)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)""",
        (
            cand_id,
            scout_source_id,
            kwargs.get("url", f"https://yt.test/{cand_id}"),
            kwargs.get("platform", "youtube"),
            kwargs.get("external_id", cand_id),
            kwargs.get("title", f"Title {cand_id}"),
            kwargs.get("channel_name", "Channel"),
            kwargs.get("duration_seconds", 60.0),
            status,
        ),
    )


class _MockRow:
    """Minimal sqlite3.Row-alike backed by a dict."""

    def __init__(self, **kwargs):
        self._d = kwargs

    def __getitem__(self, key):
        return self._d[key]


# ---------------------------------------------------------------------------
# _tokenize
# ---------------------------------------------------------------------------

class TestTokenize(unittest.TestCase):

    def test_empty_string(self):
        self.assertEqual(_tokenize(""), set())

    def test_none(self):
        self.assertEqual(_tokenize(None), set())

    def test_single_word(self):
        self.assertIn("hello", _tokenize("Hello"))

    def test_short_words_excluded(self):
        # Words shorter than 3 characters are excluded.
        result = _tokenize("go is ok but fine")
        self.assertNotIn("go", result)
        self.assertNotIn("is", result)
        self.assertNotIn("ok", result)
        self.assertIn("but", result)
        self.assertIn("fine", result)

    def test_numeric_tokens(self):
        self.assertIn("123", _tokenize("test 123 run"))

    def test_punctuation_stripped(self):
        result = _tokenize("foo, bar! baz.")
        self.assertIn("foo", result)
        self.assertIn("bar", result)
        self.assertIn("baz", result)

    def test_case_insensitive(self):
        result = _tokenize("Python PYTHON python")
        # Should all collapse to the same token "python".
        self.assertEqual(result, {"python"})

    def test_deduplication(self):
        result = _tokenize("cat cat cat")
        self.assertEqual(result, {"cat"})


# ---------------------------------------------------------------------------
# _duration_fit
# ---------------------------------------------------------------------------

class TestDurationFit(unittest.TestCase):

    def test_none_returns_neutral(self):
        self.assertAlmostEqual(_duration_fit(None), 0.5)

    def test_very_short_low_score(self):
        # ≤15 s → 0.2
        self.assertAlmostEqual(_duration_fit(10), 0.2)
        self.assertAlmostEqual(_duration_fit(15), 0.2)

    def test_medium_range_high_score(self):
        # 60 s is on the boundary where the formula yields 1.0
        self.assertAlmostEqual(_duration_fit(60), 1.0)

    def test_moderate_length_near_one(self):
        # 15 < d ≤ 60 -- score increases from 0.6 towards 1.0
        score = _duration_fit(37.5)  # midpoint of [15, 60]
        self.assertGreater(score, 0.6)
        self.assertLessEqual(score, 1.0)

    def test_long_video_decay(self):
        # 60 < d ≤ 180 -- score decreases from 1.0 towards 0.8
        score_at_120 = _duration_fit(120)
        self.assertGreater(score_at_120, 0.8)
        self.assertLessEqual(score_at_120, 1.0)

    def test_very_long_low_score(self):
        # > 600 s → 0.1
        self.assertAlmostEqual(_duration_fit(601), 0.1)
        self.assertAlmostEqual(_duration_fit(3600), 0.1)

    def test_scores_in_0_to_1(self):
        for d in [0, 5, 15, 30, 60, 90, 120, 180, 300, 600, 3600]:
            score = _duration_fit(d)
            self.assertGreaterEqual(score, 0.0, msg=f"d={d}")
            self.assertLessEqual(score, 1.0, msg=f"d={d}")


# ---------------------------------------------------------------------------
# _heuristic_rank_score
# ---------------------------------------------------------------------------

class TestHeuristicRankScore(unittest.TestCase):

    def _row(self, title="test video", channel="channel", duration=60.0):
        return _MockRow(title=title, channel_name=channel, duration_seconds=duration)

    def test_returns_float_in_0_to_1(self):
        row = self._row()
        score = _heuristic_rank_score(row, {"test", "video"}, 0)
        self.assertGreaterEqual(score, 0.0)
        self.assertLessEqual(score, 1.0)

    def test_empty_topic_tokens_no_overlap(self):
        row = self._row(title="python tutorial")
        # Empty topic_tokens → overlap=0 → topic_score=0
        score = _heuristic_rank_score(row, set(), 0)
        # Score should still be in [0, 1]
        self.assertGreaterEqual(score, 0.0)
        self.assertLessEqual(score, 1.0)

    def test_full_overlap_beats_zero_overlap(self):
        row = self._row(title="python tutorial")
        high = _heuristic_rank_score(row, {"python", "tutorial"}, 0)
        low = _heuristic_rank_score(row, {"cooking", "baking"}, 0)
        self.assertGreater(high, low)

    def test_novel_channel_beats_seen_channel(self):
        row = self._row()
        fresh = _heuristic_rank_score(row, set(), channel_seen_count=0)
        repeat = _heuristic_rank_score(row, set(), channel_seen_count=5)
        self.assertGreater(fresh, repeat)

    def test_good_duration_raises_score(self):
        # 60 s is the optimal duration (score=1.0); very long videos score lower.
        optimal = _heuristic_rank_score(self._row(duration=60.0), set(), 0)
        bad = _heuristic_rank_score(self._row(duration=3600.0), set(), 0)
        self.assertGreater(optimal, bad)


# ---------------------------------------------------------------------------
# _pick_with_caps
# ---------------------------------------------------------------------------

class TestPickWithCaps(unittest.TestCase):

    def _row(self, id_, source_id="s1", channel="ch1"):
        return _MockRow(id=id_, scout_source_id=source_id, channel_name=channel)

    def test_picks_up_to_limit(self):
        rows = [self._row(f"v{i}") for i in range(10)]
        picked = _pick_with_caps(rows, 3, defaultdict(int), defaultdict(int), set())
        self.assertEqual(len(picked), 3)

    def test_respects_source_cap(self):
        # Each row has a unique channel so the channel cap does not interfere,
        # meaning only the source cap should limit the result.
        rows = [self._row(f"v{i}", source_id="s1", channel=f"ch{i}")
                for i in range(SCOUT_MAX_LLM_PER_SOURCE + 2)]
        source_counts = defaultdict(int)
        picked = _pick_with_caps(rows, 100, source_counts, defaultdict(int), set())
        self.assertEqual(len(picked), SCOUT_MAX_LLM_PER_SOURCE)

    def test_respects_channel_cap(self):
        # SCOUT_MAX_LLM_PER_CHANNEL + 1 rows from the same channel.
        rows = [self._row(f"v{i}", source_id=f"s{i}", channel="same-ch")
                for i in range(SCOUT_MAX_LLM_PER_CHANNEL + 2)]
        channel_counts = defaultdict(int)
        picked = _pick_with_caps(rows, 100, defaultdict(int), channel_counts, set())
        self.assertEqual(len(picked), SCOUT_MAX_LLM_PER_CHANNEL)

    def test_already_selected_ids_skipped(self):
        rows = [self._row("v1"), self._row("v2"), self._row("v3")]
        already = {"v1", "v2"}
        picked = _pick_with_caps(rows, 10, defaultdict(int), defaultdict(int), already)
        ids = [r["id"] for r in picked]
        self.assertNotIn("v1", ids)
        self.assertNotIn("v2", ids)
        self.assertIn("v3", ids)

    def test_updates_selected_ids(self):
        rows = [self._row("v1"), self._row("v2")]
        selected = set()
        _pick_with_caps(rows, 10, defaultdict(int), defaultdict(int), selected)
        self.assertIn("v1", selected)
        self.assertIn("v2", selected)

    def test_empty_input(self):
        picked = _pick_with_caps([], 10, defaultdict(int), defaultdict(int), set())
        self.assertEqual(picked, [])


# ---------------------------------------------------------------------------
# auto_approve
# ---------------------------------------------------------------------------

class TestAutoApprove(unittest.TestCase):

    def test_approved_candidate_becomes_ingested(self):
        db = _make_db()
        _seed_scout_source(db)
        _seed_candidate(db, "c1", status="approved")

        auto_approve(db)

        status = db.execute(
            "SELECT status FROM scout_candidates WHERE id = 'c1'"
        ).fetchone()["status"]
        self.assertEqual(status, "ingested")

    def test_source_row_inserted(self):
        db = _make_db()
        _seed_scout_source(db)
        _seed_candidate(db, "c2", status="approved",
                        url="https://yt.test/abc", platform="youtube")

        auto_approve(db)

        row = db.execute("SELECT * FROM sources WHERE url = 'https://yt.test/abc'").fetchone()
        self.assertIsNotNone(row, "sources row should be inserted")

    def test_job_row_inserted(self):
        db = _make_db()
        _seed_scout_source(db)
        _seed_candidate(db, "c3", status="approved")

        auto_approve(db)

        count = db.execute("SELECT COUNT(*) FROM jobs").fetchone()[0]
        self.assertEqual(count, 1)

    def test_non_approved_candidates_skipped(self):
        db = _make_db()
        _seed_scout_source(db)
        _seed_candidate(db, "p1", status="pending")
        _seed_candidate(db, "r1", status="rejected")
        _seed_candidate(db, "i1", status="ingested")

        auto_approve(db)

        source_count = db.execute("SELECT COUNT(*) FROM sources").fetchone()[0]
        self.assertEqual(source_count, 0, "non-approved candidates should not be ingested")

    def test_duplicate_url_rolls_back_and_marks_rejected(self):
        """Second candidate with the same URL triggers IntegrityError on sources.url
        UNIQUE constraint, which should roll back (no job created) and mark the
        candidate rejected -- not ingested."""
        db = _make_db()
        # Add a UNIQUE constraint on sources.url so duplicates fail.
        db.execute("CREATE UNIQUE INDEX sources_url_unique ON sources (url)")
        _seed_scout_source(db)

        _seed_candidate(db, "c-first",  status="approved",
                        url="https://yt.test/dup", external_id="dup1")
        _seed_candidate(db, "c-second", status="approved",
                        url="https://yt.test/dup", external_id="dup2")

        auto_approve(db)

        # First should be ingested, second should be rejected.
        statuses = {
            r["id"]: r["status"]
            for r in db.execute("SELECT id, status FROM scout_candidates").fetchall()
        }
        self.assertEqual(statuses["c-first"], "ingested")
        self.assertEqual(statuses["c-second"], "rejected")

        # Only one source and one job should exist.
        self.assertEqual(db.execute("SELECT COUNT(*) FROM sources").fetchone()[0], 1)
        self.assertEqual(db.execute("SELECT COUNT(*) FROM jobs").fetchone()[0], 1)

    def test_auto_ingest_disabled_leaves_approved(self):
        db = _make_db()
        _seed_scout_source(db, source_id="ss1", user_id="u1")
        db.execute(
            "INSERT INTO user_preferences (user_id, scout_auto_ingest) VALUES ('u1', 0)"
        )
        _seed_candidate(db, "c1", scout_source_id="ss1", status="approved")

        auto_approve(db)

        status = db.execute(
            "SELECT status FROM scout_candidates WHERE id = 'c1'"
        ).fetchone()["status"]
        self.assertEqual(status, "approved",
                         "candidate should remain 'approved' when auto-ingest is disabled")
        self.assertEqual(db.execute("SELECT COUNT(*) FROM sources").fetchone()[0], 0)
        self.assertEqual(db.execute("SELECT COUNT(*) FROM jobs").fetchone()[0], 0)

    def test_multiple_candidates_all_ingested(self):
        db = _make_db()
        _seed_scout_source(db)
        for i in range(5):
            _seed_candidate(db, f"c{i}", status="approved",
                            url=f"https://yt.test/v{i}", external_id=f"v{i}")

        auto_approve(db)

        count = db.execute(
            "SELECT COUNT(*) FROM scout_candidates WHERE status = 'ingested'"
        ).fetchone()[0]
        self.assertEqual(count, 5)
        self.assertEqual(db.execute("SELECT COUNT(*) FROM sources").fetchone()[0], 5)
        self.assertEqual(db.execute("SELECT COUNT(*) FROM jobs").fetchone()[0], 5)


if __name__ == "__main__":
    unittest.main()
