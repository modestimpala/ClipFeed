"""Integration tests for the ingestion worker using a real SQLite database.

These tests exercise the worker's DB logic end-to-end with a real temp-file
SQLite database, complementing the unit tests that rely on mocks.
"""

import json
import os
import re
import sqlite3
import sys
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import MagicMock, patch

# Mock heavy ML dependencies before importing worker
sys.modules.setdefault("numpy", MagicMock())
sys.modules.setdefault("minio", MagicMock())
sys.modules.setdefault("faster_whisper", MagicMock())
sys.modules.setdefault("keybert", MagicMock())
sys.modules.setdefault("sentence_transformers", MagicMock())

import worker

# ---------------------------------------------------------------------------
# Schema -- matches the real SQLite migration (001_init.sql subset needed)
# ---------------------------------------------------------------------------

SCHEMA = """
CREATE TABLE users (
    id TEXT PRIMARY KEY,
    username TEXT UNIQUE NOT NULL,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE sources (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    platform TEXT NOT NULL,
    status TEXT DEFAULT 'pending',
    title TEXT,
    channel_name TEXT,
    thumbnail_url TEXT,
    external_id TEXT,
    duration_seconds REAL,
    metadata TEXT,
    submitted_by TEXT REFERENCES users(id),
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE clips (
    id TEXT PRIMARY KEY,
    source_id TEXT REFERENCES sources(id),
    title TEXT,
    duration_seconds REAL NOT NULL,
    start_time REAL DEFAULT 0,
    end_time REAL DEFAULT 0,
    storage_key TEXT NOT NULL,
    thumbnail_key TEXT,
    width INTEGER,
    height INTEGER,
    file_size_bytes INTEGER,
    transcript TEXT,
    topics TEXT DEFAULT '[]',
    content_score REAL DEFAULT 0.5,
    is_protected INTEGER DEFAULT 0,
    status TEXT DEFAULT 'processing',
    expires_at TEXT,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE topics (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    path TEXT,
    parent_id TEXT REFERENCES topics(id),
    depth INTEGER DEFAULT 0,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE clip_topics (
    clip_id TEXT NOT NULL REFERENCES clips(id),
    topic_id TEXT NOT NULL REFERENCES topics(id),
    confidence REAL DEFAULT 1.0,
    source TEXT DEFAULT 'keybert',
    PRIMARY KEY (clip_id, topic_id)
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


class IntegrationTestBase(unittest.TestCase):
    """Base class providing a real temp-file SQLite database."""

    def setUp(self):
        self._tmp = tempfile.NamedTemporaryFile(suffix=".db", delete=False)
        self.db_path = self._tmp.name
        self._tmp.close()

        self.db = self._open(self.db_path)
        self.db.executescript(SCHEMA)
        self.db.execute(
            "INSERT INTO users VALUES ('u1','tester','t@t.com','hash', 'Tester', datetime('now'))"
        )
        self.db.execute(
            "INSERT INTO sources VALUES ('s1','http://youtube.com/watch?v=abc','youtube','pending','Video Title','Channel','http://thumb.jpg','abc',120.0,'{}','u1',datetime('now'))"
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
        os.unlink(self.db_path)

    def _make_worker(self):
        """Create a Worker stub with a real DB connection."""
        w = object.__new__(worker.Worker)
        w.db = self._open(self.db_path)
        w.http_mode = False
        w.api = None
        return w

    def _insert_job(self, job_id="j1", source_id="s1", attempts=0, max_attempts=3,
                    status="queued", priority=5, run_after=None, payload=None):
        if payload is None:
            payload = json.dumps({
                "source_id": source_id,
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })
        self.db.execute(
            """INSERT INTO jobs (id, source_id, job_type, status, priority, payload,
                                attempts, max_attempts, run_after)
               VALUES (?, ?, 'ingest', ?, ?, ?, ?, ?, ?)""",
            (job_id, source_id, status, priority, payload, attempts, max_attempts, run_after),
        )


# ---------------------------------------------------------------------------
# Job claiming integration tests
# ---------------------------------------------------------------------------

class TestPopJobIntegration(IntegrationTestBase):
    """Test _pop_job with a real SQLite database."""

    def test_claims_queued_job(self):
        self._insert_job("j1")

        w = self._make_worker()
        row = w._pop_job()
        self.assertIsNotNone(row)
        self.assertEqual(row["id"], "j1")

        # Verify it's now running
        status = self.db.execute("SELECT status FROM jobs WHERE id='j1'").fetchone()["status"]
        self.assertEqual(status, "running")

    def test_attempts_incremented(self):
        self._insert_job("j1", attempts=2)

        w = self._make_worker()
        w._pop_job()

        attempts = self.db.execute("SELECT attempts FROM jobs WHERE id='j1'").fetchone()["attempts"]
        self.assertEqual(attempts, 3)

    def test_started_at_set(self):
        self._insert_job("j1")

        w = self._make_worker()
        w._pop_job()

        started = self.db.execute("SELECT started_at FROM jobs WHERE id='j1'").fetchone()["started_at"]
        self.assertIsNotNone(started)

    def test_no_queued_jobs_returns_none(self):
        w = self._make_worker()
        row = w._pop_job()
        self.assertIsNone(row)

    def test_skips_running_jobs(self):
        self._insert_job("j1", status="running")

        w = self._make_worker()
        row = w._pop_job()
        self.assertIsNone(row)

    def test_skips_future_run_after(self):
        future = (datetime.utcnow() + timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self._insert_job("j1", run_after=future)

        w = self._make_worker()
        row = w._pop_job()
        self.assertIsNone(row)

    def test_claims_past_run_after(self):
        past = (datetime.utcnow() - timedelta(minutes=5)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self._insert_job("j1", run_after=past)

        w = self._make_worker()
        row = w._pop_job()
        self.assertIsNotNone(row)
        self.assertEqual(row["id"], "j1")

    def test_priority_ordering(self):
        self._insert_job("j-low", priority=1)
        self._insert_job("j-high", priority=10)

        w = self._make_worker()
        row = w._pop_job()
        self.assertEqual(row["id"], "j-high")

    def test_fifo_within_same_priority(self):
        """Jobs with the same priority should be claimed in creation order."""
        self.db.execute(
            """INSERT INTO jobs (id, source_id, job_type, status, priority, payload, attempts, max_attempts, created_at)
               VALUES ('j-first', 's1', 'ingest', 'queued', 5, '{}', 0, 3, '2020-01-01T00:00:00Z')"""
        )
        self.db.execute(
            """INSERT INTO jobs (id, source_id, job_type, status, priority, payload, attempts, max_attempts, created_at)
               VALUES ('j-second', 's1', 'ingest', 'queued', 5, '{}', 0, 3, '2025-01-01T00:00:00Z')"""
        )

        w = self._make_worker()
        row = w._pop_job()
        self.assertEqual(row["id"], "j-first")

    def test_concurrent_claim_safety(self):
        """Two pop_job calls should not return the same job."""
        self._insert_job("j1")

        w1 = self._make_worker()
        w2 = self._make_worker()

        row1 = w1._pop_job()
        row2 = w2._pop_job()

        self.assertIsNotNone(row1)
        self.assertIsNone(row2, "Second pop_job should return None -- job already claimed")


# ---------------------------------------------------------------------------
# Stale job reclamation integration tests
# ---------------------------------------------------------------------------

class TestReclaimStaleIntegration(IntegrationTestBase):
    """Test _reclaim_stale_running_jobs with a real SQLite database."""

    def test_requeues_stale_job(self):
        self._insert_job("j1", attempts=1, max_attempts=3, status="running")
        self.db.execute(
            "UPDATE jobs SET started_at = datetime('now', '-3 hours') WHERE id = 'j1'"
        )

        w = self._make_worker()
        requeued, failed = w._reclaim_stale_running_jobs()

        self.assertEqual(requeued, 1)
        self.assertEqual(failed, 0)

        row = self.db.execute("SELECT status, error, run_after FROM jobs WHERE id='j1'").fetchone()
        self.assertEqual(row["status"], "queued")
        self.assertIn("stale watchdog", row["error"])
        self.assertIsNotNone(row["run_after"])

    def test_fails_exhausted_stale_job(self):
        self._insert_job("j1", attempts=3, max_attempts=3, status="running")
        self.db.execute(
            "UPDATE jobs SET started_at = datetime('now', '-3 hours') WHERE id = 'j1'"
        )

        w = self._make_worker()
        requeued, failed = w._reclaim_stale_running_jobs()

        self.assertEqual(requeued, 0)
        self.assertEqual(failed, 1)

        row = self.db.execute("SELECT status, completed_at FROM jobs WHERE id='j1'").fetchone()
        self.assertEqual(row["status"], "failed")
        self.assertIsNotNone(row["completed_at"])

    def test_does_not_touch_fresh_running_jobs(self):
        self._insert_job("j1", attempts=1, max_attempts=3, status="running")
        self.db.execute(
            "UPDATE jobs SET started_at = datetime('now', '-1 minute') WHERE id = 'j1'"
        )

        w = self._make_worker()
        requeued, failed = w._reclaim_stale_running_jobs()

        self.assertEqual(requeued, 0)
        self.assertEqual(failed, 0)

        status = self.db.execute("SELECT status FROM jobs WHERE id='j1'").fetchone()["status"]
        self.assertEqual(status, "running")

    def test_resets_source_status_on_requeue(self):
        self.db.execute("UPDATE sources SET status = 'processing' WHERE id = 's1'")
        self._insert_job("j1", attempts=1, max_attempts=3, status="running")
        self.db.execute(
            "UPDATE jobs SET started_at = datetime('now', '-3 hours') WHERE id = 'j1'"
        )

        w = self._make_worker()
        w._reclaim_stale_running_jobs()

        src = self.db.execute("SELECT status FROM sources WHERE id='s1'").fetchone()
        self.assertEqual(src["status"], "pending")

    def test_mixed_stale_jobs(self):
        """Multiple stale jobs: one retryable, one exhausted."""
        self._insert_job("j-retry", attempts=1, max_attempts=3, status="running")
        self._insert_job("j-fail", attempts=3, max_attempts=3, status="running")
        self.db.execute(
            "UPDATE jobs SET started_at = datetime('now', '-3 hours') WHERE id IN ('j-retry', 'j-fail')"
        )

        w = self._make_worker()
        requeued, failed = w._reclaim_stale_running_jobs()

        self.assertEqual(requeued, 1)
        self.assertEqual(failed, 1)


# ---------------------------------------------------------------------------
# Topic resolution integration tests
# ---------------------------------------------------------------------------

class TestTopicResolutionIntegration(IntegrationTestBase):
    """Test _resolve_or_create_topic and _slugify with real DB."""

    def test_creates_new_topic(self):
        w = self._make_worker()
        topic_id = w._resolve_or_create_topic(self.db, "Machine Learning")

        self.assertIsNotNone(topic_id)
        row = self.db.execute("SELECT name, slug FROM topics WHERE id = ?", (topic_id,)).fetchone()
        self.assertEqual(row["name"], "Machine Learning")
        self.assertEqual(row["slug"], "machine-learning")

    def test_resolves_existing_topic_by_name(self):
        self.db.execute(
            "INSERT INTO topics (id, name, slug, path, depth) VALUES ('t1', 'cooking', 'cooking', 'cooking', 0)"
        )

        w = self._make_worker()
        topic_id = w._resolve_or_create_topic(self.db, "cooking")
        self.assertEqual(topic_id, "t1")

    def test_resolves_existing_topic_case_insensitive(self):
        self.db.execute(
            "INSERT INTO topics (id, name, slug, path, depth) VALUES ('t1', 'Cooking', 'cooking', 'cooking', 0)"
        )

        w = self._make_worker()
        topic_id = w._resolve_or_create_topic(self.db, "COOKING")
        self.assertEqual(topic_id, "t1")

    def test_resolves_by_slug(self):
        self.db.execute(
            "INSERT INTO topics (id, name, slug, path, depth) VALUES ('t1', 'Machine Learning', 'machine-learning', 'machine-learning', 0)"
        )

        w = self._make_worker()
        topic_id = w._resolve_or_create_topic(self.db, "Machine Learning")
        self.assertEqual(topic_id, "t1")

    def test_idempotent_creation(self):
        """Creating the same topic twice returns the same ID."""
        w = self._make_worker()
        id1 = w._resolve_or_create_topic(self.db, "cooking")
        id2 = w._resolve_or_create_topic(self.db, "cooking")
        self.assertEqual(id1, id2)

        count = self.db.execute("SELECT COUNT(*) FROM topics WHERE slug='cooking'").fetchone()[0]
        self.assertEqual(count, 1)


# ---------------------------------------------------------------------------
# Slugify tests
# ---------------------------------------------------------------------------

class TestSlugify(unittest.TestCase):
    """Test the Worker._slugify static method."""

    def test_basic(self):
        self.assertEqual(worker.Worker._slugify("Machine Learning"), "machine-learning")

    def test_special_chars_removed(self):
        self.assertEqual(worker.Worker._slugify("C++ Programming!"), "c-programming")

    def test_multiple_spaces(self):
        self.assertEqual(worker.Worker._slugify("  lots   of   spaces  "), "lots-of-spaces")

    def test_already_slugified(self):
        self.assertEqual(worker.Worker._slugify("already-slugified"), "already-slugified")

    def test_empty_string(self):
        self.assertEqual(worker.Worker._slugify(""), "topic")

    def test_only_special_chars(self):
        self.assertEqual(worker.Worker._slugify("@#$%"), "topic")

    def test_unicode_removed(self):
        self.assertEqual(worker.Worker._slugify("café latte"), "caf-latte")

    def test_numbers_preserved(self):
        self.assertEqual(worker.Worker._slugify("Web3 Development"), "web3-development")


# ---------------------------------------------------------------------------
# Clip title generation integration tests
# ---------------------------------------------------------------------------

class TestClipTitleIntegration(unittest.TestCase):
    """Additional clip title edge cases using real WorkerStub."""

    def setUp(self):
        self.w = object.__new__(worker.Worker)

    def test_long_transcript_truncated(self):
        # Transcript with many words should be truncated and end with "..."
        words = " ".join(["word"] * 100)
        title = self.w._generate_clip_title(words, "", 0)
        self.assertTrue(title.endswith("..."))
        self.assertLessEqual(len(title), 80)  # reasonable title length

    def test_unicode_transcript(self):
        title = self.w._generate_clip_title("こんにちは世界", "Japanese Video", 0)
        # Should not crash, even if the result falls back to source title
        self.assertIsInstance(title, str)
        self.assertTrue(len(title) > 0)

    def test_clip_index_zero_based(self):
        """Clip index 0 → Part 1 in the title."""
        title = self.w._generate_clip_title("", "Source", 0)
        self.assertIn("Part 1", title)

    def test_clip_index_increments(self):
        title = self.w._generate_clip_title("", "Source", 9)
        self.assertIn("Part 10", title)


# ---------------------------------------------------------------------------
# Process job retry integration (end-to-end with real DB)
# ---------------------------------------------------------------------------

class TestProcessJobRetryIntegration(IntegrationTestBase):
    """End-to-end tests for process_job failure/retry flows with real SQLite."""

    def _insert_source(self, source_id="s1"):
        """Insert a minimal source row so _update_source doesn't fail."""
        self.db.execute(
            "INSERT OR IGNORE INTO sources (id, url, status) VALUES (?, 'http://example.com', 'pending')",
            (source_id,),
        )
        self.db.commit()

    @patch("worker.WORK_DIR")
    @patch("worker.open_db")
    def test_transient_error_requeues_job(self, mock_open_db, mock_work_dir):
        """A transient failure should re-queue the job with backoff."""
        mock_open_db.return_value = self._open(self.db_path)
        mock_work_dir.__truediv__ = lambda self_wd, key: Path(tempfile.mkdtemp()) / key
        self._insert_job("j1", attempts=1, max_attempts=3, status="running")
        self._insert_source("s1")

        w = self._make_worker()
        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("Connection timeout")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        row = self.db.execute("SELECT status, error FROM jobs WHERE id='j1'").fetchone()
        self.assertEqual(row["status"], "queued")
        self.assertIn("timeout", row["error"])

    @patch("worker.WORK_DIR")
    @patch("worker.open_db")
    def test_permanent_rejection_fails_job(self, mock_open_db, mock_work_dir):
        """A VideoRejected exception should mark the job rejected (no retry)."""
        mock_open_db.return_value = self._open(self.db_path)
        mock_work_dir.__truediv__ = lambda self_wd, key: Path(tempfile.mkdtemp()) / key
        self._insert_job("j1", attempts=1, max_attempts=3, status="running")
        self._insert_source("s1")

        w = self._make_worker()
        with patch.object(w, "fetch_source_metadata", side_effect=worker.VideoRejected("Too short")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        row = self.db.execute("SELECT status, error FROM jobs WHERE id='j1'").fetchone()
        self.assertEqual(row["status"], "rejected")
        self.assertIn("Too short", row["error"])

    @patch("worker.WORK_DIR")
    @patch("worker.open_db")
    def test_max_attempts_exhausted(self, mock_open_db, mock_work_dir):
        """At max attempts, a transient error should permanently fail the job."""
        mock_open_db.return_value = self._open(self.db_path)
        mock_work_dir.__truediv__ = lambda self_wd, key: Path(tempfile.mkdtemp()) / key
        self._insert_job("j1", attempts=3, max_attempts=3, status="running")
        self._insert_source("s1")

        w = self._make_worker()
        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 500")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        row = self.db.execute("SELECT status FROM jobs WHERE id='j1'").fetchone()
        self.assertEqual(row["status"], "failed")

        src = self.db.execute("SELECT status FROM sources WHERE id='s1'").fetchone()
        self.assertEqual(src["status"], "failed")


# ---------------------------------------------------------------------------
# Cookie decryption integration test
# ---------------------------------------------------------------------------

class TestDecryptCookieIntegration(unittest.TestCase):
    """Test cookie decryption with real crypto (if cryptography is installed)."""

    def test_decrypt_round_trip(self):
        """Encrypt then decrypt should return the original value."""
        try:
            from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        except ImportError:
            self.skipTest("cryptography not installed")

        import hashlib
        import base64

        secret = "test-secret-key"
        plaintext = "session_id=abc123; domain=.youtube.com"

        # Encrypt (same algorithm as Go API's encryptCookie)
        key = hashlib.sha256(secret.encode()).digest()
        aesgcm = AESGCM(key)
        nonce = os.urandom(12)
        ciphertext = aesgcm.encrypt(nonce, plaintext.encode(), None)
        encoded = base64.b64encode(nonce + ciphertext).decode()

        # Decrypt using worker's function
        result = worker.decrypt_cookie(encoded, secret)
        self.assertEqual(result, plaintext)

    def test_decrypt_invalid_base64(self):
        result = worker.decrypt_cookie("not-valid-base64!!!", "secret")
        self.assertIsNone(result)

    def test_decrypt_wrong_key(self):
        """Decrypting with the wrong key should return None (not crash)."""
        try:
            from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        except ImportError:
            self.skipTest("cryptography not installed")

        import hashlib
        import base64

        secret = "correct-key"
        key = hashlib.sha256(secret.encode()).digest()
        aesgcm = AESGCM(key)
        nonce = os.urandom(12)
        ciphertext = aesgcm.encrypt(nonce, b"secret data", None)
        encoded = base64.b64encode(nonce + ciphertext).decode()

        result = worker.decrypt_cookie(encoded, "wrong-key")
        self.assertIsNone(result)


if __name__ == "__main__":
    unittest.main()
