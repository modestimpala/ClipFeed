"""
ClipFeed Worker API Client
HTTP client for the internal worker API.

Requires WORKER_API_URL and WORKER_SECRET environment variables.
"""

import base64
import json
import logging
import time

import threading

import requests

log = logging.getLogger("worker.api_client")


class DuplicateSourceError(Exception):
    """Raised when a source update conflicts with an existing source (same platform + external_id)."""
    pass


class WorkerAPIClient:
    """HTTP client for the ClipFeed internal worker API."""

    def __init__(self, api_url: str, worker_secret: str, timeout: int = 30):
        self.api_url = api_url.rstrip("/")
        self._worker_secret = worker_secret
        self.timeout = timeout
        self._local = threading.local()

    def _session(self) -> requests.Session:
        """Return a per-thread Session, creating and configuring it on first use."""
        if not hasattr(self._local, "session"):
            s = requests.Session()
            s.headers.update({
                "Authorization": f"Bearer {self._worker_secret}",
                "Content-Type": "application/json",
            })
            self._local.session = s
        return self._local.session

    def _url(self, path: str) -> str:
        return f"{self.api_url}/api/internal{path}"

    def _get(self, path: str, **kwargs) -> requests.Response:
        return self._session().get(self._url(path), timeout=self.timeout, **kwargs)

    def _post(self, path: str, data=None, **kwargs) -> requests.Response:
        return self._session().post(self._url(path), json=data, timeout=self.timeout, **kwargs)

    def _put(self, path: str, data=None, **kwargs) -> requests.Response:
        return self._session().put(self._url(path), json=data, timeout=self.timeout, **kwargs)

    # --- Job operations ---

    def claim_job(self) -> dict | None:
        """Atomically claim the next queued job. Returns {id, payload} or None."""
        resp = self._post("/jobs/claim")
        if resp.status_code == 204:
            return None
        resp.raise_for_status()
        data = resp.json()
        # Ensure payload is a dict
        if isinstance(data.get("payload"), str):
            data["payload"] = json.loads(data["payload"])
        return data

    def update_job(
        self,
        job_id: str,
        status: str,
        error: str = None,
        result: dict = None,
        run_after: str = None,
    ):
        """Update a job's status."""
        body = {"status": status}
        if error is not None:
            body["error"] = error
        if result is not None:
            body["result"] = result
        if run_after is not None:
            body["run_after"] = run_after
        resp = self._put(f"/jobs/{job_id}", data=body)
        resp.raise_for_status()

    def get_job(self, job_id: str) -> dict:
        """Get job info (attempts, max_attempts, status)."""
        resp = self._get(f"/jobs/{job_id}")
        resp.raise_for_status()
        return resp.json()

    def reclaim_stale_jobs(self, stale_minutes: int = 120) -> tuple[int, int]:
        """Reclaim stale running jobs. Returns (requeued, failed)."""
        resp = self._post("/jobs/reclaim", data={"stale_minutes": stale_minutes})
        resp.raise_for_status()
        data = resp.json()
        return data.get("requeued", 0), data.get("failed", 0)

    # --- Source operations ---

    def update_source(self, source_id: str, **fields):
        """Update source fields: status, title, channel_name, metadata, etc."""
        resp = self._put(f"/sources/{source_id}", data=fields)
        if resp.status_code == 409:
            raise DuplicateSourceError(resp.json().get("error", "duplicate source"))
        resp.raise_for_status()

    def get_cookie(self, source_id: str, platform: str) -> str | None:
        """Get decrypted platform cookie for a source's user."""
        resp = self._get(f"/sources/{source_id}/cookie", params={"platform": platform})
        resp.raise_for_status()
        return resp.json().get("cookie")

    # --- Clip operations ---

    def create_clip(
        self,
        clip_id: str,
        source_id: str,
        title: str,
        duration_seconds: float,
        start_time: float,
        end_time: float,
        storage_key: str,
        thumbnail_key: str,
        width: int,
        height: int,
        file_size_bytes: int,
        transcript: str,
        topics: list[str],
        content_score: float,
        expires_at: str,
        platform: str = "",
        channel_name: str = "",
        text_embedding: bytes = None,
        visual_embedding: bytes = None,
        model_version: str = "",
    ) -> str:
        """Create a clip with topics, embeddings, and FTS index."""
        body = {
            "id": clip_id,
            "source_id": source_id,
            "title": title,
            "duration_seconds": duration_seconds,
            "start_time": start_time,
            "end_time": end_time,
            "storage_key": storage_key,
            "thumbnail_key": thumbnail_key,
            "width": width,
            "height": height,
            "file_size_bytes": file_size_bytes,
            "transcript": transcript,
            "topics": topics or [],
            "content_score": content_score,
            "expires_at": expires_at,
            "platform": platform,
            "channel_name": channel_name,
            "model_version": model_version,
        }
        if text_embedding:
            body["text_embedding"] = base64.b64encode(text_embedding).decode()
        if visual_embedding:
            body["visual_embedding"] = base64.b64encode(visual_embedding).decode()

        resp = self._post("/clips", data=body)
        resp.raise_for_status()
        return resp.json().get("id", clip_id)

    # --- Topic operations ---

    def resolve_topic(self, name: str) -> str:
        """Resolve or create a topic. Returns topic ID."""
        resp = self._post("/topics/resolve", data={"name": name})
        resp.raise_for_status()
        return resp.json()["id"]

    # --- Health check ---

    def health_check(self) -> bool:
        """Check if the API is reachable."""
        try:
            resp = self._session().get(
                f"{self.api_url}/health", timeout=5
            )
            return resp.status_code == 200
        except Exception:
            return False

    def wait_for_api(self, max_wait: int = 120, interval: int = 5):
        """Block until the API is reachable or timeout."""
        start = time.time()
        while time.time() - start < max_wait:
            if self.health_check():
                log.info("API is reachable at %s", self.api_url)
                return True
            log.info("Waiting for API at %s ...", self.api_url)
            time.sleep(interval)
        raise ConnectionError(f"API at {self.api_url} not reachable after {max_wait}s")
