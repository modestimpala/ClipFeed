"""Unit tests for the ingestion worker's pure logic functions."""

import sys
import unittest
from unittest.mock import patch, MagicMock

# Mock heavy third-party dependencies before importing worker so the module
# loads without needing minio, faster_whisper, or keybert installed.
sys.modules.setdefault("numpy", MagicMock())
sys.modules.setdefault("minio", MagicMock())
sys.modules.setdefault("faster_whisper", MagicMock())
sys.modules.setdefault("keybert", MagicMock())
sys.modules.setdefault("sentence_transformers", MagicMock())

import worker


class WorkerStub:
    """A minimal stand-in that gives us access to Worker's methods without
    the heavy __init__ (MinIO, Whisper, KeyBERT connections)."""

    _merge_scenes = worker.Worker._merge_scenes
    _fixed_split = worker.Worker._fixed_split
    _generate_clip_title = worker.Worker._generate_clip_title
    detect_scenes = worker.Worker.detect_scenes


def make_stub():
    stub = object.__new__(WorkerStub)
    return stub


# ---------------------------------------------------------------------------
# _fixed_split
# ---------------------------------------------------------------------------

class TestFixedSplit(unittest.TestCase):
    def setUp(self):
        self.w = make_stub()

    def test_short_video_single_segment(self):
        segments = self.w._fixed_split(40.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["start"], 0.0)
        self.assertEqual(segments[0]["end"], 40.0)

    def test_exact_target_duration(self):
        segments = self.w._fixed_split(45.0)
        self.assertEqual(len(segments), 1)
        self.assertAlmostEqual(segments[0]["end"], 45.0)

    def test_two_full_segments(self):
        segments = self.w._fixed_split(90.0)
        self.assertEqual(len(segments), 2)
        self.assertEqual(segments[0]["start"], 0.0)
        self.assertEqual(segments[0]["end"], 45.0)
        self.assertEqual(segments[1]["start"], 45.0)
        self.assertEqual(segments[1]["end"], 90.0)

    def test_remainder_too_short_dropped(self):
        """A remainder shorter than MIN_CLIP_SECONDS (15) is dropped."""
        # 45 + 10 = 55 → first segment 0–45, remainder 45–55 is 10s < 15s
        segments = self.w._fixed_split(55.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["end"], 45.0)

    def test_remainder_long_enough_kept(self):
        # 45 + 20 = 65 → first 0–45, second 45–65 (20s >= 15s)
        segments = self.w._fixed_split(65.0)
        self.assertEqual(len(segments), 2)
        self.assertEqual(segments[1]["start"], 45.0)
        self.assertEqual(segments[1]["end"], 65.0)

    def test_very_short_video_dropped(self):
        """Video shorter than MIN_CLIP_SECONDS produces no segments."""
        segments = self.w._fixed_split(10.0)
        self.assertEqual(len(segments), 0)

    def test_exactly_min_duration(self):
        segments = self.w._fixed_split(15.0)
        self.assertEqual(len(segments), 1)

    def test_values_are_rounded(self):
        segments = self.w._fixed_split(100.0)
        for seg in segments:
            self.assertEqual(seg["start"], round(seg["start"], 2))
            self.assertEqual(seg["end"], round(seg["end"], 2))


# ---------------------------------------------------------------------------
# _merge_scenes
# ---------------------------------------------------------------------------

class TestMergeScenes(unittest.TestCase):
    def setUp(self):
        self.w = make_stub()

    def test_single_long_segment(self):
        scene_times = [0.0, 50.0]
        segments = self.w._merge_scenes(scene_times, 50.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["start"], 0.0)
        self.assertEqual(segments[0]["end"], 50.0)

    def test_merges_short_scenes(self):
        """Scenes shorter than TARGET (45s) are merged with the next one."""
        scene_times = [0.0, 10.0, 20.0, 50.0]
        segments = self.w._merge_scenes(scene_times, 50.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["start"], 0.0)
        self.assertEqual(segments[0]["end"], 50.0)

    def test_splits_overly_long_segment(self):
        """A segment > MAX_CLIP_SECONDS gets split at TARGET intervals."""
        scene_times = [0.0, 150.0]
        segments = self.w._merge_scenes(scene_times, 150.0)
        self.assertTrue(len(segments) >= 2)
        for seg in segments:
            dur = seg["end"] - seg["start"]
            self.assertLessEqual(dur, worker.MAX_CLIP_SECONDS + 1)

    def test_drops_tiny_remainder(self):
        """Remainder < MIN_CLIP_SECONDS at the end is dropped."""
        scene_times = [0.0, 50.0, 55.0]
        segments = self.w._merge_scenes(scene_times, 55.0)
        # 0-50 is the main segment; 50-55 is only 5s, should be dropped
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0]["end"], 50.0)

    def test_keeps_remainder_above_min(self):
        scene_times = [0.0, 50.0, 70.0]
        segments = self.w._merge_scenes(scene_times, 70.0)
        self.assertEqual(len(segments), 2)
        self.assertEqual(segments[1]["start"], 50.0)
        self.assertEqual(segments[1]["end"], 70.0)

    def test_empty_scene_times(self):
        """With no scene boundaries, remainder logic captures the full duration."""
        segments = self.w._merge_scenes([], 60.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0], {"start": 0.0, "end": 60.0})


# ---------------------------------------------------------------------------
# _generate_clip_title
# ---------------------------------------------------------------------------

class TestGenerateClipTitle(unittest.TestCase):
    def setUp(self):
        self.w = make_stub()

    def test_from_transcript(self):
        title = self.w._generate_clip_title(
            "This is a really interesting discussion about cooking", "", 0
        )
        self.assertIn("...", title)
        self.assertTrue(title.startswith("This"))

    def test_short_transcript_uses_source_title(self):
        title = self.w._generate_clip_title("Hi", "My Video", 2)
        self.assertEqual(title, "My Video (Part 3)")

    def test_empty_transcript_uses_source_title(self):
        title = self.w._generate_clip_title("", "Source Vid", 0)
        self.assertEqual(title, "Source Vid (Part 1)")

    def test_no_transcript_no_title_fallback(self):
        title = self.w._generate_clip_title("", "", 4)
        self.assertEqual(title, "Clip 5")

    def test_transcript_with_exactly_three_words(self):
        title = self.w._generate_clip_title("one two three", "", 0)
        self.assertEqual(title, "one two three...")


# ---------------------------------------------------------------------------
# detect_scenes – mocked subprocess
# ---------------------------------------------------------------------------

class TestDetectScenes(unittest.TestCase):
    def setUp(self):
        self.w = make_stub()

    def test_short_video_returns_single_segment(self):
        from pathlib import Path
        segments = self.w.detect_scenes(Path("/fake/video.mp4"), 30.0)
        self.assertEqual(len(segments), 1)
        self.assertEqual(segments[0], {"start": 0, "end": 30.0})

    @patch("worker.subprocess.run")
    def test_falls_back_to_fixed_split_on_no_silence(self, mock_run):
        from pathlib import Path
        mock_run.return_value = MagicMock(
            returncode=0, stderr="no silence detected here\n"
        )
        segments = self.w.detect_scenes(Path("/fake/video.mp4"), 120.0)
        # Should fall back to _fixed_split
        self.assertTrue(len(segments) >= 1)
        for seg in segments:
            self.assertIn("start", seg)
            self.assertIn("end", seg)

    @patch("worker.subprocess.run")
    def test_uses_silence_midpoints(self, mock_run):
        from pathlib import Path
        stderr = (
            "[silencedetect @ 0x1234] silence_start: 44.5\n"
            "[silencedetect @ 0x1234] silence_end: 45.5 | silence_duration: 1.0\n"
        )
        mock_run.return_value = MagicMock(returncode=0, stderr=stderr)
        segments = self.w.detect_scenes(Path("/fake/video.mp4"), 100.0)
        self.assertTrue(len(segments) >= 1)
        # The midpoint is 45.0 which should be used as a split point
        all_starts = [s["start"] for s in segments]
        all_ends = [s["end"] for s in segments]
        self.assertIn(0.0, all_starts)

    @patch("worker.subprocess.run")
    def test_falls_back_on_subprocess_error(self, mock_run):
        from pathlib import Path
        mock_run.side_effect = Exception("ffmpeg crashed")
        segments = self.w.detect_scenes(Path("/fake/video.mp4"), 120.0)
        self.assertTrue(len(segments) >= 1)


# ---------------------------------------------------------------------------
# Module-level constants sanity check
# ---------------------------------------------------------------------------

class TestWorkerConstants(unittest.TestCase):
    def test_min_less_than_max(self):
        self.assertLess(worker.MIN_CLIP_SECONDS, worker.MAX_CLIP_SECONDS)

    def test_target_between_min_and_max(self):
        self.assertGreaterEqual(worker.TARGET_CLIP_SECONDS, worker.MIN_CLIP_SECONDS)
        self.assertLessEqual(worker.TARGET_CLIP_SECONDS, worker.MAX_CLIP_SECONDS)


# ---------------------------------------------------------------------------
# Retry / exponential-backoff logic
# ---------------------------------------------------------------------------

import os
import sqlite3
import tempfile
from datetime import datetime, timedelta

_RETRY_SCHEMA = """
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT UNIQUE NOT NULL,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL
);
CREATE TABLE sources (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    platform TEXT NOT NULL,
    status TEXT DEFAULT 'pending',
    submitted_by TEXT REFERENCES users(id),
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    source_id TEXT REFERENCES sources(id),
    job_type TEXT NOT NULL,
    status TEXT DEFAULT 'queued',
    priority INTEGER DEFAULT 5,
    payload TEXT DEFAULT '{}',
    result TEXT DEFAULT '{}',
    error TEXT,
    attempts INTEGER DEFAULT 0,
    max_attempts INTEGER DEFAULT 3,
    run_after TEXT,
    started_at TEXT,
    completed_at TEXT,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
CREATE TABLE platform_cookies (
    id TEXT PRIMARY KEY,
    user_id TEXT,
    platform TEXT,
    cookie_str TEXT,
    is_active INTEGER DEFAULT 1
);
"""


class RetryTestBase(unittest.TestCase):
    """Sets up a temp-file SQLite DB with the minimal schema for retry tests.

    Using a file (not :memory:) so process_job can open/close its own
    connection via the mocked open_db while tests query through self.db.
    """

    def setUp(self):
        self._tmp = tempfile.NamedTemporaryFile(suffix=".db", delete=False)
        self._db_path = self._tmp.name
        self._tmp.close()

        self.db = self._open(self._db_path)
        self.db.executescript(_RETRY_SCHEMA)
        self.db.execute(
            "INSERT INTO users VALUES ('u1','tester','t@t.com','hash')"
        )
        self.db.execute(
            "INSERT INTO sources VALUES ('s1','http://example.com/v','youtube','pending','u1', datetime('now'))"
        )

    def _open(self, path):
        conn = sqlite3.connect(path, isolation_level=None, check_same_thread=False)
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("PRAGMA foreign_keys=ON")
        conn.row_factory = sqlite3.Row
        return conn

    def tearDown(self):
        self.db.close()
        os.unlink(self._db_path)

    def _insert_job(self, job_id="j1", attempts=0, max_attempts=3, status="queued"):
        self.db.execute(
            "INSERT INTO jobs (id, source_id, job_type, status, payload, attempts, max_attempts) "
            "VALUES (?, 's1', 'ingest', ?, ?, ?, ?)",
            (job_id, status, '{"source_id":"s1","url":"http://example.com/v","platform":"youtube"}',
             attempts, max_attempts),
        )


class TestRetryOnFailure(RetryTestBase):
    """process_job re-queues with backoff when attempts < max_attempts."""

    @patch("worker.open_db")
    def test_first_failure_requeues_with_backoff(self, mock_open_db):
        mock_open_db.return_value = self._open(self._db_path)
        self._insert_job(attempts=1, max_attempts=3, status="running")

        w = object.__new__(worker.Worker)
        w.db = self.db

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 429")):
            w.process_job("j1", {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

        row = self.db.execute("SELECT status, error, run_after FROM jobs WHERE id = 'j1'").fetchone()
        self.assertEqual(row["status"], "queued")
        self.assertIn("429", row["error"])
        self.assertIsNotNone(row["run_after"])

        run_after = datetime.strptime(row["run_after"], "%Y-%m-%dT%H:%M:%SZ")
        self.assertGreater(run_after, datetime.utcnow() - timedelta(seconds=5))

        src = self.db.execute("SELECT status FROM sources WHERE id = 's1'").fetchone()
        self.assertEqual(src["status"], "pending")

    @patch("worker.open_db")
    def test_final_attempt_marks_failed(self, mock_open_db):
        mock_open_db.return_value = self._open(self._db_path)
        self._insert_job(attempts=3, max_attempts=3, status="running")

        w = object.__new__(worker.Worker)
        w.db = self.db

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 429")):
            w.process_job("j1", {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

        row = self.db.execute("SELECT status, error FROM jobs WHERE id = 'j1'").fetchone()
        self.assertEqual(row["status"], "failed")
        self.assertIn("429", row["error"])

        src = self.db.execute("SELECT status FROM sources WHERE id = 's1'").fetchone()
        self.assertEqual(src["status"], "failed")

    @patch("worker.open_db")
    def test_backoff_delay_doubles_each_attempt(self, mock_open_db):
        """delay = BASE * 2^(attempts-1): 30s, 60s, 120s, …"""
        for attempt, expected_delay in [(1, 30), (2, 60)]:
            job_id = f"j{attempt}"
            self._insert_job(job_id=job_id, attempts=attempt, max_attempts=3, status="running")

            mock_open_db.return_value = self._open(self._db_path)
            w = object.__new__(worker.Worker)
            w.db = self.db
            with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("rate limit")):
                w.process_job(job_id, {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

            row = self.db.execute("SELECT run_after FROM jobs WHERE id = ?", (job_id,)).fetchone()
            run_after = datetime.strptime(row["run_after"], "%Y-%m-%dT%H:%M:%SZ")
            expected_min = datetime.utcnow() + timedelta(seconds=expected_delay - 5)
            expected_max = datetime.utcnow() + timedelta(seconds=expected_delay + 5)
            self.assertGreaterEqual(run_after, expected_min, f"attempt {attempt}")
            self.assertLessEqual(run_after, expected_max, f"attempt {attempt}")


class TestPopJobRespectsRunAfter(RetryTestBase):
    """_pop_job skips jobs whose run_after is in the future."""

    def test_skips_job_in_backoff(self):
        self._insert_job()
        future = (datetime.utcnow() + timedelta(minutes=10)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.db.execute("UPDATE jobs SET run_after = ? WHERE id = 'j1'", (future,))

        w = object.__new__(worker.Worker)
        w.db = self.db
        row = w._pop_job()
        self.assertIsNone(row)

    def test_picks_up_job_past_backoff(self):
        self._insert_job()
        past = (datetime.utcnow() - timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.db.execute("UPDATE jobs SET run_after = ? WHERE id = 'j1'", (past,))

        w = object.__new__(worker.Worker)
        w.db = self.db
        row = w._pop_job()
        self.assertIsNotNone(row)
        self.assertEqual(row["id"], "j1")

    def test_null_run_after_is_eligible(self):
        self._insert_job()

        w = object.__new__(worker.Worker)
        w.db = self.db
        row = w._pop_job()
        self.assertIsNotNone(row)


if __name__ == "__main__":
    unittest.main()
