"""
Shared LLM HTTP client for the ClipFeed ingestion/scout layer.
Uses LiteLLM for provider abstraction (Ollama, OpenAI-compatible, Anthropic, etc.).
"""

import json
import logging
import os
import re
import time

from litellm import completion
import requests

logger = logging.getLogger("llm_client")

# Master toggle — when false, all LLM calls are skipped instantly.
ENABLE_AI = os.getenv("ENABLE_AI", "true").lower() == "true"

LLM_PROVIDER = os.getenv("LLM_PROVIDER", "ollama").strip().lower()
LLM_URL = os.getenv("LLM_URL", "http://llm:11434").rstrip("/")
OLLAMA_MODEL = os.getenv("OLLAMA_MODEL", "llama3.2:3b")
LLM_BASE_URL = os.getenv("LLM_BASE_URL", "").rstrip("/")
LLM_MODEL = os.getenv("LLM_MODEL", "").strip() or OLLAMA_MODEL
LLM_API_KEY = os.getenv("LLM_API_KEY", "").strip()
ANTHROPIC_VERSION = os.getenv("ANTHROPIC_VERSION", "2023-06-01").strip() or "2023-06-01"

# Timeouts
AVAILABILITY_TIMEOUT = 3
GENERATE_TIMEOUT = 30
PULL_TIMEOUT = int(os.getenv("LLM_PULL_TIMEOUT", "900"))

# Log configuration at import time
logger.info(
    "[LLM] Config loaded: ENABLE_AI=%s provider=%s model=%s base_url=%s",
    ENABLE_AI, LLM_PROVIDER, LLM_MODEL, LLM_BASE_URL or LLM_URL,
)


def _provider() -> str:
    return (LLM_PROVIDER or "ollama").lower()


def _model(model: str | None = None) -> str:
    return (model or LLM_MODEL or OLLAMA_MODEL).strip()


def _base_url() -> str:
    provider = _provider()
    if provider == "ollama":
        return (LLM_BASE_URL or LLM_URL).rstrip("/")
    if provider == "anthropic":
        return (LLM_BASE_URL or "https://api.anthropic.com/v1").rstrip("/")
    return (LLM_BASE_URL or "https://api.openai.com/v1").rstrip("/")


def _anthropic_headers() -> dict:
    key = (LLM_API_KEY or "").strip()
    if not key:
        return {}
    return {
        "Content-Type": "application/json",
        "x-api-key": key,
        "anthropic-version": ANTHROPIC_VERSION,
    }


def _extract_text_content(content) -> str:
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = []
        for item in content:
            if not isinstance(item, dict):
                continue
            if item.get("type") == "text" and isinstance(item.get("text"), str):
                parts.append(item["text"])
        return " ".join(parts).strip()
    return ""


def _litellm_model(model: str | None = None) -> str:
    name = _model(model)
    if "/" in name:
        return name
    provider = _provider()
    if provider == "ollama":
        return f"ollama/{name}"
    if provider == "anthropic":
        return f"anthropic/{name}"
    return f"openai/{name}"


def _litellm_params(model: str, max_tokens: int) -> dict:
    params = {
        "model": _litellm_model(model),
        "max_tokens": max_tokens,
        "temperature": 0.2,
    }

    if LLM_API_KEY:
        params["api_key"] = LLM_API_KEY

    base = _base_url()
    if base:
        params["api_base"] = base

    if _provider() == "anthropic":
        params["extra_headers"] = {"anthropic-version": ANTHROPIC_VERSION}

    return params


def _extract_completion_text(response) -> str:
    choices = getattr(response, "choices", None)
    if not choices:
        if isinstance(response, dict):
            choices = response.get("choices", [])
        else:
            choices = []
    if not choices:
        return ""

    first = choices[0]
    message = getattr(first, "message", None)
    if message is None and isinstance(first, dict):
        message = first.get("message")
    if message is None:
        return ""

    content = getattr(message, "content", None)
    if content is None and isinstance(message, dict):
        content = message.get("content")

    return _extract_text_content(content)


def is_available() -> bool:
    """Check if configured LLM provider is reachable."""
    if not ENABLE_AI:
        logger.info("[LLM] AI disabled (ENABLE_AI=false), skipping availability check")
        return False

    provider = _provider()
    base = _base_url()
    logger.info("[LLM] Checking availability: provider=%s base_url=%s", provider, base)
    try:
        if provider == "ollama":
            url = f"{base}/api/tags"
            logger.debug("[LLM] GET %s (timeout=%ds)", url, AVAILABILITY_TIMEOUT)
            r = requests.get(url, timeout=AVAILABILITY_TIMEOUT)
        elif provider == "anthropic":
            headers = _anthropic_headers()
            if not headers.get("x-api-key"):
                logger.warning("[LLM] API key missing for provider=%s — cannot check availability", provider)
                return False
            url = f"{base}/models"
            logger.debug("[LLM] GET %s (timeout=%ds)", url, AVAILABILITY_TIMEOUT)
            r = requests.get(url, headers=headers, timeout=AVAILABILITY_TIMEOUT)
        else:
            if not LLM_API_KEY:
                logger.warning("[LLM] API key missing for provider=%s — cannot check availability", provider)
                return False
            url = f"{base}/models"
            logger.debug("[LLM] GET %s (timeout=%ds)", url, AVAILABILITY_TIMEOUT)
            r = requests.get(
                url,
                headers={
                    "Content-Type": "application/json",
                    "Authorization": f"Bearer {LLM_API_KEY}",
                },
                timeout=AVAILABILITY_TIMEOUT,
            )
        r.raise_for_status()
        logger.info("[LLM] Provider available: provider=%s status=%d", provider, r.status_code)
        return True
    except requests.RequestException as e:
        logger.warning("[LLM] Provider unavailable: provider=%s base_url=%s error=%s", provider, base, e)
        return False


def model_exists(model: str | None = None) -> bool:
    """Check if the configured model is available for the active provider."""
    provider = _provider()
    model = _model(model)
    if not model:
        return False

    if provider != "ollama":
        if not LLM_API_KEY:
            return False
        return True

    try:
        r = requests.get(
            f"{_base_url()}/api/tags",
            timeout=AVAILABILITY_TIMEOUT,
        )
        r.raise_for_status()
        data = r.json()
        models = data.get("models", []) if isinstance(data, dict) else []
        for item in models:
            if not isinstance(item, dict):
                continue
            name = str(item.get("name") or "").strip()
            if name == model:
                return True
        return False
    except requests.RequestException as e:
        logger.warning("LLM model check failed: %s", e)
        return False
    except (json.JSONDecodeError, KeyError, TypeError) as e:
        logger.warning("LLM /api/tags parse error: %s", e)
        return False


def ensure_model(model: str | None = None, auto_pull: bool = True) -> bool:
    """Ensure model exists; Ollama can auto-pull, API providers validate key/model."""
    provider = _provider()
    model = _model(model)
    logger.info("[LLM] Ensuring model: provider=%s model=%s auto_pull=%s", provider, model, auto_pull)
    if not model:
        logger.warning("[LLM] No model configured — cannot proceed")
        return False

    if provider != "ollama":
        if not LLM_API_KEY:
            logger.warning("[LLM] No API key configured for provider=%s — cannot use model %s", provider, model)
            return False
        logger.info("[LLM] API-based provider=%s with model=%s — assuming available", provider, model)
        return True

    if model_exists(model):
        logger.info("[LLM] Ollama model '%s' already present", model)
        return True

    if not auto_pull:
        logger.warning("[LLM] Ollama model '%s' not found and auto_pull disabled", model)
        return False

    logger.info("[LLM] Ollama model '%s' not found — pulling now (timeout=%ds)...", model, PULL_TIMEOUT)
    start = time.time()
    try:
        pull_url = f"{_base_url()}/api/pull"
        logger.info("[LLM] POST %s body={name: %s}", pull_url, model)
        r = requests.post(
            pull_url,
            json={"name": model, "stream": False},
            timeout=PULL_TIMEOUT,
        )
        r.raise_for_status()
        elapsed = time.time() - start
        logger.info("[LLM] Ollama model '%s' pull complete in %.1fs", model, elapsed)
        return model_exists(model)
    except requests.RequestException as e:
        elapsed = time.time() - start
        logger.error("[LLM] Ollama model pull FAILED for '%s' after %.1fs: %s", model, elapsed, e)
        return False


def generate(
    prompt: str,
    model: str | None = None,
    max_tokens: int = 256,
) -> str:
    """Generate text using configured provider. Returns empty string on failure."""
    model = _model(model)
    provider = _provider()
    prompt_preview = (prompt[:120] + "...") if len(prompt) > 120 else prompt
    logger.info("[LLM] Generate request: provider=%s model=%s max_tokens=%d prompt_len=%d",
                provider, model, max_tokens, len(prompt))
    logger.debug("[LLM] Prompt preview: %s", prompt_preview)
    try:
        if provider != "ollama" and not LLM_API_KEY:
            logger.warning("[LLM] API key missing for provider=%s — aborting generate", provider)
            return ""

        params = _litellm_params(model, max_tokens)
        logger.debug("[LLM] LiteLLM params: model=%s api_base=%s", params.get("model"), params.get("api_base"))

        start = time.time()
        response = completion(
            messages=[{"role": "user", "content": prompt}],
            timeout=GENERATE_TIMEOUT,
            **params,
        )
        elapsed = time.time() - start

        result = _extract_completion_text(response)
        result_preview = (result[:150] + "...") if len(result) > 150 else result
        logger.info("[LLM] Generate complete: provider=%s model=%s elapsed=%.2fs response_len=%d",
                    provider, model, elapsed, len(result))
        logger.debug("[LLM] Response preview: %s", result_preview)
        return result
    except Exception as e:
        logger.error("[LLM] Generate FAILED: provider=%s model=%s error=%s", provider, model, e)
        return ""


def generate_title(transcript: str, source_title: str = "") -> str:
    """
    Use LLM to generate a concise clip title.
    Returns empty string on failure.
    """
    logger.info("[LLM] Generating clip title: source_title=%r transcript_len=%d",
                source_title[:60] if source_title else "", len(transcript or ""))
    excerpt = transcript[:500] if transcript else ""
    prompt = (
        "Generate a concise, engaging title (5-10 words) for this video clip. "
        "Only respond with the title, no quotes or explanation.\n\n"
        f"Source: {source_title}\n"
        f"Transcript: {excerpt}"
    )
    result = generate(prompt)
    if not result:
        logger.warning("[LLM] Title generation returned empty result")
        return ""
    title = result.strip()
    logger.info("[LLM] Generated title: %r", title)
    return title


def refine_topics(transcript: str, keybert_topics: list) -> list:
    """
    Send transcript excerpt and KeyBERT topics to LLM asking it to confirm/refine them
    and suggest a parent category. Returns list of topic strings.
    Returns original keybert_topics on failure.
    """
    logger.info("[LLM] Refining topics via LLM: keybert_topics=%s transcript_len=%d",
                keybert_topics, len(transcript or ""))
    topics_str = ", ".join(str(t) for t in keybert_topics) if keybert_topics else ""
    excerpt = transcript[:500] if transcript else ""
    prompt = (
        "Given this transcript excerpt, confirm or refine these topics: "
        f"{topics_str}. Also suggest a parent category for each. "
        "Respond as a JSON array of objects with 'topic' and 'parent' keys.\n\n"
        f"Transcript: {excerpt}"
    )
    result = generate(prompt)
    if not result:
        logger.warning("[LLM] Topic refinement returned empty — keeping original topics: %s", keybert_topics)
        return list(keybert_topics)

    try:
        parsed = json.loads(result)
        if isinstance(parsed, list):
            refined = [str(item.get("topic", item)) for item in parsed if item]
            logger.info("[LLM] Topics refined: %s -> %s", keybert_topics, refined)
            return refined
    except json.JSONDecodeError:
        # Try to extract JSON from markdown or mixed response
        match = re.search(r"\[[\s\S]*?\]", result)
        if match:
            try:
                parsed = json.loads(match.group(0))
                if isinstance(parsed, list):
                    refined = [str(item.get("topic", item)) for item in parsed if item]
                    logger.info("[LLM] Topics refined (extracted from markdown): %s -> %s", keybert_topics, refined)
                    return refined
            except json.JSONDecodeError:
                pass
        logger.warning("[LLM] Topic refinement: could not parse JSON from response: %r", result[:200])

    logger.info("[LLM] Keeping original topics (parse failed): %s", keybert_topics)
    return list(keybert_topics)


def evaluate_candidate(
    title: str,
    channel: str,
    top_topics: list,
) -> float | None:
    """
    Rate relevance 1-10 given user interests.
    Returns None on request/parse failure.
    """
    logger.info("[LLM] Evaluating candidate: title=%r channel=%r topics=%s",
                title[:80] if title else "", channel, top_topics[:5] if top_topics else [])
    topics_str = ", ".join(str(t) for t in top_topics) if top_topics else "(none)"
    prompt = (
        f"Given these user interests: {topics_str}. "
        f"Rate 1-10 how relevant this video is: '{title}' by '{channel}'. "
        "Reply with just the number."
    )
    result = generate(prompt)
    if not result:
        logger.warning("[LLM] Candidate evaluation returned empty for title=%r", title[:80] if title else "")
        return None

    match = re.search(r"(\d+(?:\.\d+)?)", result.strip())
    if match:
        try:
            score = float(match.group(1))
            score = max(0.0, min(10.0, score))
            logger.info("[LLM] Candidate scored: %.1f — title=%r channel=%r", score, title[:60] if title else "", channel)
            return score
        except ValueError:
            pass

    logger.warning("[LLM] Could not parse score from LLM response: %r (title=%r)", result[:100], title[:60] if title else "")
    return None
