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
# Retry / exponential-backoff logic (via mocked HTTP API)
# ---------------------------------------------------------------------------

from datetime import datetime, timedelta


def _make_api_worker():
    """Create a Worker stub with a mocked API client."""
    w = object.__new__(worker.Worker)
    w.api = MagicMock()
    return w


class TestRetryOnFailure(unittest.TestCase):
    """process_job re-queues with backoff when attempts < max_attempts."""

    def test_first_failure_requeues_with_backoff(self):
        w = _make_api_worker()
        w.api.get_job.return_value = {"attempts": 1, "max_attempts": 3}
        w.api.get_cookie.return_value = None

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 429")):
            w.process_job("j1", {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

        # Should requeue with backoff
        w.api.update_job.assert_called_once()
        call_args = w.api.update_job.call_args
        self.assertEqual(call_args[0][0], "j1")
        self.assertEqual(call_args[0][1], "queued")
        self.assertIn("429", call_args[1]["error"])
        self.assertIsNotNone(call_args[1]["run_after"])

        w.api.update_source.assert_any_call("s1", status="pending")

    def test_final_attempt_marks_failed(self):
        w = _make_api_worker()
        w.api.get_job.return_value = {"attempts": 3, "max_attempts": 3}
        w.api.get_cookie.return_value = None

        with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("HTTP 429")):
            w.process_job("j1", {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

        w.api.update_job.assert_called_once()
        call_args = w.api.update_job.call_args
        self.assertEqual(call_args[0][1], "failed")
        self.assertIn("429", call_args[1]["error"])

        w.api.update_source.assert_any_call("s1", status="failed")

    def test_backoff_delay_doubles_each_attempt(self):
        """delay = BASE * 2^(attempts-1): 30s, 60s, 120s, …"""
        for attempt, expected_delay in [(1, 30), (2, 60)]:
            w = _make_api_worker()
            w.api.get_job.return_value = {"attempts": attempt, "max_attempts": 3}
            w.api.get_cookie.return_value = None

            with patch.object(w, "fetch_source_metadata", side_effect=RuntimeError("rate limit")):
                w.process_job(f"j{attempt}", {"source_id": "s1", "url": "http://example.com/v", "platform": "youtube"})

            call_args = w.api.update_job.call_args
            run_after = datetime.strptime(call_args[1]["run_after"], "%Y-%m-%dT%H:%M:%SZ")
            expected_min = datetime.utcnow() + timedelta(seconds=expected_delay - 5)
            expected_max = datetime.utcnow() + timedelta(seconds=expected_delay + 5)
            self.assertGreaterEqual(run_after, expected_min, f"attempt {attempt}")
            self.assertLessEqual(run_after, expected_max, f"attempt {attempt}")


class TestPopJob(unittest.TestCase):
    """_pop_job delegates to API client."""

    def test_returns_none_when_no_jobs(self):
        w = _make_api_worker()
        w.api.claim_job.return_value = None
        self.assertIsNone(w._pop_job())

    def test_returns_dict_with_id_and_payload(self):
        w = _make_api_worker()
        w.api.claim_job.return_value = {
            "id": "j1",
            "payload": {"source_id": "s1", "url": "http://example.com/v"},
        }
        row = w._pop_job()
        self.assertEqual(row["id"], "j1")
        self.assertIn("source_id", row["payload"])


class TestReclaimStaleRunningJobs(unittest.TestCase):
    """_reclaim_stale_running_jobs delegates to API client."""

    def test_delegates_to_api(self):
        w = _make_api_worker()
        w.api.reclaim_stale_jobs.return_value = (2, 1)

        requeued, failed = w._reclaim_stale_running_jobs()
        self.assertEqual(requeued, 2)
        self.assertEqual(failed, 1)
        w.api.reclaim_stale_jobs.assert_called_once_with(worker.JOB_STALE_MINUTES)


if __name__ == "__main__":
    unittest.main()
