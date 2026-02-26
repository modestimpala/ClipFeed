"""Integration tests for the ingestion worker using mocked HTTP API client.

These tests exercise the worker's job processing, retry, and error handling
logic end-to-end with a mocked WorkerAPIClient.
"""

import json
import os
import sys
import tempfile
import unittest
from datetime import datetime, timedelta
from pathlib import Path
from unittest.mock import MagicMock, patch, call

# Mock heavy ML dependencies before importing worker
sys.modules.setdefault("numpy", MagicMock())
sys.modules.setdefault("minio", MagicMock())
sys.modules.setdefault("faster_whisper", MagicMock())
sys.modules.setdefault("keybert", MagicMock())
sys.modules.setdefault("sentence_transformers", MagicMock())

import worker


def _make_worker():
    """Create a Worker stub with a mocked API client."""
    w = object.__new__(worker.Worker)
    w.api = MagicMock()
    return w


# ---------------------------------------------------------------------------
# Job claiming tests
# ---------------------------------------------------------------------------

class TestPopJob(unittest.TestCase):
    """Test _pop_job via API client."""

    def test_claims_queued_job(self):
        w = _make_worker()
        w.api.claim_job.return_value = {
            "id": "j1",
            "payload": {"source_id": "s1", "url": "http://youtube.com/watch?v=abc"},
        }
        row = w._pop_job()
        self.assertIsNotNone(row)
        self.assertEqual(row["id"], "j1")
        w.api.claim_job.assert_called_once()

    def test_no_queued_jobs_returns_none(self):
        w = _make_worker()
        w.api.claim_job.return_value = None
        row = w._pop_job()
        self.assertIsNone(row)

    def test_payload_serialized_to_json_string(self):
        w = _make_worker()
        w.api.claim_job.return_value = {
            "id": "j1",
            "payload": {"source_id": "s1", "url": "http://example.com"},
        }
        row = w._pop_job()
        # payload should be a JSON string (matches old sqlite3.Row behavior)
        parsed = json.loads(row["payload"])
        self.assertEqual(parsed["source_id"], "s1")


# ---------------------------------------------------------------------------
# Stale job reclamation tests
# ---------------------------------------------------------------------------

class TestReclaimStale(unittest.TestCase):
    """Test _reclaim_stale_running_jobs delegates to API."""

    def test_returns_api_counts(self):
        w = _make_worker()
        w.api.reclaim_stale_jobs.return_value = (2, 1)

        requeued, failed = w._reclaim_stale_running_jobs()
        self.assertEqual(requeued, 2)
        self.assertEqual(failed, 1)
        w.api.reclaim_stale_jobs.assert_called_once_with(worker.JOB_STALE_MINUTES)

    def test_zero_stale_jobs(self):
        w = _make_worker()
        w.api.reclaim_stale_jobs.return_value = (0, 0)

        requeued, failed = w._reclaim_stale_running_jobs()
        self.assertEqual(requeued, 0)
        self.assertEqual(failed, 0)


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
# Clip title generation tests
# ---------------------------------------------------------------------------

class TestClipTitleIntegration(unittest.TestCase):
    """Additional clip title edge cases using real WorkerStub."""

    def setUp(self):
        self.w = object.__new__(worker.Worker)

    def test_long_transcript_truncated(self):
        words = " ".join(["word"] * 100)
        title = self.w._generate_clip_title(words, "", 0)
        self.assertTrue(title.endswith("..."))
        self.assertLessEqual(len(title), 80)

    def test_unicode_transcript(self):
        title = self.w._generate_clip_title("こんにちは世界", "Japanese Video", 0)
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
# Process job retry integration (end-to-end with mocked API)
# ---------------------------------------------------------------------------

class TestProcessJobRetry(unittest.TestCase):
    """End-to-end tests for process_job failure/retry flows with mocked API."""

    def test_transient_error_requeues_job(self):
        """A transient failure should re-queue the job with backoff."""
        w = _make_worker()
        w.api.get_job.return_value = {"attempts": 1, "max_attempts": 3}
        w.api.get_cookie.return_value = None

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("Connection timeout")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        w.api.update_job.assert_called_once()
        call_args = w.api.update_job.call_args
        self.assertEqual(call_args[0][1], "queued")
        self.assertIn("timeout", call_args[1]["error"])

    def test_permanent_rejection_fails_job(self):
        """A VideoRejected exception should mark the job rejected (no retry)."""
        w = _make_worker()
        w.api.get_cookie.return_value = None

        with patch.object(w, "fetch_source_metadata", side_effect=worker.VideoRejected("Too short")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        w.api.update_job.assert_called_once()
        call_args = w.api.update_job.call_args
        self.assertEqual(call_args[0][1], "rejected")
        self.assertIn("Too short", call_args[1]["error"])

        w.api.update_source.assert_any_call("s1", status="rejected")

    def test_max_attempts_exhausted(self):
        """At max attempts, a transient error should permanently fail the job."""
        w = _make_worker()
        w.api.get_job.return_value = {"attempts": 3, "max_attempts": 3}
        w.api.get_cookie.return_value = None

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 500")):
            w.process_job("j1", {
                "source_id": "s1",
                "url": "http://youtube.com/watch?v=abc",
                "platform": "youtube",
            })

        w.api.update_job.assert_called_once()
        call_args = w.api.update_job.call_args
        self.assertEqual(call_args[0][1], "failed")

        w.api.update_source.assert_any_call("s1", status="failed")


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


# ---------------------------------------------------------------------------
# Heartbeat tests
# ---------------------------------------------------------------------------

class TestHeartbeat(unittest.TestCase):
    """heartbeat_job delegates to the API client without raising."""

    def test_heartbeat_calls_api(self):
        w = _make_worker()
        w.api.heartbeat_job.return_value = True
        result = w.api.heartbeat_job("job-123")
        self.assertTrue(result)
        w.api.heartbeat_job.assert_called_once_with("job-123")

    def test_heartbeat_returns_false_on_failure(self):
        w = _make_worker()
        w.api.heartbeat_job.return_value = False
        result = w.api.heartbeat_job("job-999")
        self.assertFalse(result)


if __name__ == "__main__":
    unittest.main()
