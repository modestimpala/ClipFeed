"""Unit tests for the storage lifecycle manager.

Uses a temporary SQLite file and mocks only MinIO calls.
"""

import os
import sys
import sqlite3
import tempfile
import unittest
from unittest.mock import MagicMock, patch
from datetime import datetime, timedelta

sys.modules.setdefault("minio", MagicMock())

import lifecycle


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
    thumbnail_key TEXT,
    file_size_bytes INTEGER,
    is_protected INTEGER DEFAULT 0,
    status TEXT DEFAULT 'processing',
    expires_at TEXT,
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE saved_clips (
    user_id TEXT NOT NULL REFERENCES users(id),
    clip_id TEXT NOT NULL REFERENCES clips(id),
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (user_id, clip_id)
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
    created_at TEXT DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
"""


class LifecycleTestBase(unittest.TestCase):
    """Base class that sets up a temp SQLite DB file for each test."""

    def setUp(self):
        self.db_fd, self.db_path = tempfile.mkstemp(suffix=".db")
        db = sqlite3.connect(self.db_path)
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("PRAGMA busy_timeout=5000")
        db.executescript(SCHEMA)
        db.close()
        self.mock_minio = MagicMock()

    def tearDown(self):
        os.close(self.db_fd)
        os.unlink(self.db_path)

    def _db(self):
        db = sqlite3.connect(self.db_path)
        db.row_factory = sqlite3.Row
        return db

    def insert_clip(self, clip_id, storage_key="clips/x/clip.mp4",
                    thumbnail_key="clips/x/thumb.jpg", file_size_bytes=1_000_000,
                    is_protected=0, status="ready", expires_at=None, created_at=None):
        db = self._db()
        db.execute(
            "INSERT INTO sources (id, url, platform) VALUES (?, 'http://x.com', 'direct')",
            (f"src-{clip_id}",),
        )
        db.execute("""
            INSERT INTO clips (id, source_id, storage_key, thumbnail_key, duration_seconds,
                               file_size_bytes, is_protected, status, expires_at, created_at)
            VALUES (?, ?, ?, ?, 30.0, ?, ?, ?, ?, COALESCE(?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')))
        """, (clip_id, f"src-{clip_id}", storage_key, thumbnail_key, file_size_bytes,
              is_protected, status, expires_at, created_at))
        db.commit()
        db.close()

    def run_lifecycle(self, storage_limit_gb=50.0):
        with patch.object(lifecycle, "DB_PATH", self.db_path), \
             patch.object(lifecycle, "Minio", return_value=self.mock_minio), \
             patch.object(lifecycle, "STORAGE_LIMIT_GB", storage_limit_gb):
            lifecycle.main()

    def get_status(self, clip_id):
        db = self._db()
        row = db.execute("SELECT status FROM clips WHERE id = ?", (clip_id,)).fetchone()
        db.close()
        return row[0] if row else None


class TestLifecycleExpiredClips(LifecycleTestBase):
    """Phase 1: Delete expired, unprotected clips."""

    def test_expired_unprotected_clips_are_marked_expired(self):
        past = (datetime.utcnow() - timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("c1", expires_at=past)

        self.run_lifecycle()

        self.assertEqual(self.get_status("c1"), "expired")
        self.mock_minio.remove_object.assert_called()

    def test_protected_clips_not_deleted(self):
        past = (datetime.utcnow() - timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("c2", expires_at=past, is_protected=1)

        self.run_lifecycle()

        self.assertEqual(self.get_status("c2"), "ready")

    def test_non_expired_clips_not_deleted(self):
        future = (datetime.utcnow() + timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("c3", expires_at=future)

        self.run_lifecycle()

        self.assertEqual(self.get_status("c3"), "ready")


class TestLifecycleStorageEviction(LifecycleTestBase):
    """Phase 2: Evict oldest clips when over storage limit."""

    def test_evicts_oldest_when_over_limit(self):
        future = (datetime.utcnow() + timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("old", file_size_bytes=1_000_000, expires_at=future,
                         created_at="2020-01-01T00:00:00Z")
        self.insert_clip("new", file_size_bytes=1_000_000, expires_at=future,
                         created_at="2025-01-01T00:00:00Z")

        self.run_lifecycle(storage_limit_gb=0.0001)

        self.assertEqual(self.get_status("old"), "evicted")

    def test_no_eviction_under_limit(self):
        future = (datetime.utcnow() + timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("fine", file_size_bytes=1_000_000, expires_at=future)

        self.run_lifecycle(storage_limit_gb=100.0)

        self.assertEqual(self.get_status("fine"), "ready")

    def test_protected_clips_not_evicted(self):
        future = (datetime.utcnow() + timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.insert_clip("prot", file_size_bytes=1_000_000, expires_at=future,
                         is_protected=1)

        self.run_lifecycle(storage_limit_gb=0.0001)

        self.assertEqual(self.get_status("prot"), "ready")


class TestLifecycleJobCleanup(LifecycleTestBase):
    """Phase 3: Clean up old failed/complete jobs."""

    def test_old_failed_jobs_deleted(self):
        db = self._db()
        db.execute("""
            INSERT INTO jobs (id, job_type, status, created_at)
            VALUES ('j1', 'download', 'failed', datetime('now', '-10 days'))
        """)
        db.execute("""
            INSERT INTO jobs (id, job_type, status, created_at)
            VALUES ('j2', 'download', 'complete', datetime('now', '-10 days'))
        """)
        db.execute("""
            INSERT INTO jobs (id, job_type, status, created_at)
            VALUES ('j3', 'download', 'queued', datetime('now', '-10 days'))
        """)
        db.commit()
        db.close()

        self.run_lifecycle()

        db = self._db()
        remaining = db.execute("SELECT id FROM jobs").fetchall()
        remaining_ids = [r[0] for r in remaining]
        db.close()

        self.assertNotIn("j1", remaining_ids)
        self.assertNotIn("j2", remaining_ids)
        self.assertIn("j3", remaining_ids)

    def test_recent_failed_jobs_kept(self):
        db = self._db()
        db.execute("""
            INSERT INTO jobs (id, job_type, status, created_at)
            VALUES ('j4', 'download', 'failed', datetime('now', '-1 day'))
        """)
        db.commit()
        db.close()

        self.run_lifecycle()

        db = self._db()
        row = db.execute("SELECT id FROM jobs WHERE id = 'j4'").fetchone()
        db.close()
        self.assertIsNotNone(row)


if __name__ == "__main__":
    unittest.main()
