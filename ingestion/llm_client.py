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

LLM_PROVIDER = os.getenv("LLM_PROVIDER", "").strip().lower()
LLM_URL = os.getenv("LLM_URL", "http://llm:11434").rstrip("/")
OLLAMA_MODEL = os.getenv("OLLAMA_MODEL", "llama3.2:3b")
LLM_BASE_URL = os.getenv("LLM_BASE_URL", "").rstrip("/")
LLM_MODEL = os.getenv("LLM_MODEL", "").strip() or OLLAMA_MODEL
LLM_API_KEY = os.getenv("LLM_API_KEY", "").strip()
ANTHROPIC_VERSION = os.getenv("ANTHROPIC_VERSION", "2023-06-01").strip() or "2023-06-01"

# LiteLLM reads provider-specific env vars (GEMINI_API_KEY, ANTHROPIC_API_KEY,
# OPENAI_API_KEY) rather than the generic api_key kwarg for auth validation.
# Mirror LLM_API_KEY into the appropriate env var so LiteLLM finds it.
if LLM_API_KEY:
    _provider_key_map = {
        "gemini": "GEMINI_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
        "openai": "OPENAI_API_KEY",
    }
    _env_key = _provider_key_map.get(LLM_PROVIDER)
    if _env_key and not os.environ.get(_env_key):
        os.environ[_env_key] = LLM_API_KEY


def _ai_enabled() -> bool:
    """AI is enabled when a provider is configured (and API key present for cloud providers)."""
    if not LLM_PROVIDER:
        return False
    if LLM_PROVIDER == "ollama":
        return True
    return bool(LLM_API_KEY)

# Timeouts
AVAILABILITY_TIMEOUT = 3
GENERATE_TIMEOUT = 30
PULL_TIMEOUT = int(os.getenv("LLM_PULL_TIMEOUT", "900"))

# Log configuration at import time
logger.info(
    "[LLM] Config loaded: ai_enabled=%s provider=%s model=%s base_url=%s",
    _ai_enabled(), LLM_PROVIDER or "(none)", LLM_MODEL, LLM_BASE_URL or LLM_URL,
)


def _provider() -> str:
    return LLM_PROVIDER or "ollama"


def _model(model: str | None = None) -> str:
    return (model or LLM_MODEL or OLLAMA_MODEL).strip()


def _base_url() -> str:
    provider = _provider()
    if provider == "ollama":
        return (LLM_BASE_URL or LLM_URL).rstrip("/")
    if provider == "anthropic":
        return (LLM_BASE_URL or "https://api.anthropic.com/v1").rstrip("/")
    if provider == "gemini":
        return LLM_BASE_URL.rstrip("/") if LLM_BASE_URL else ""
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


def _strip_code_fences(text: str) -> str:
    """Remove markdown code fences (```json ... ```) from LLM responses."""
    text = text.strip()
    if text.startswith("```"):
        text = re.sub(r"^```\w*\n?", "", text)
    if text.endswith("```"):
        text = text[:-3]
    return text.strip()


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
    if provider == "gemini":
        return f"gemini/{name}"
    return f"openai/{name}"


def _is_gemini3_model(model_name: str) -> bool:
    """Return True for Gemini 3+ models regardless of provider prefix."""
    name = model_name.lower()
    for prefix in ("openai/", "gemini/", "anthropic/", "ollama/"):
        if name.startswith(prefix):
            name = name[len(prefix):]
            break
    return name.startswith("gemini-3")


def _litellm_params(model: str, max_tokens: int) -> dict:
    litellm_model = _litellm_model(model)
    gemini3 = _is_gemini3_model(litellm_model)

    # Gemini 3+ models require temperature=1.0 regardless of how they are routed
    # (gemini/ or openai-compatible endpoint). Lower values cause output truncation.
    # For all other providers/models keep 0.2 for more deterministic outputs.
    temperature = 1.0 if (gemini3 or _provider() == "gemini") else 0.2

    params = {
        "model": litellm_model,
        "max_tokens": max_tokens,
        "temperature": temperature,
    }

    # Gemini 3 always runs thinking; reasoning_effort="none" maps to thinking_level="low"
    # (the minimum possible), preventing thinking tokens from consuming the output budget.
    # Only send this when using the native gemini/ provider -- the openai-compat endpoint
    # does not understand reasoning_effort and will raise UnsupportedParamsError.
    if gemini3 and _provider() == "gemini":
        params["reasoning_effort"] = "none"

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
    if not _ai_enabled():
        logger.info("[LLM] AI not configured (no provider/key), skipping availability check")
        return False

    provider = _provider()
    base = _base_url()
    logger.info("[LLM] Checking availability: provider=%s base_url=%s", provider, base)

    # For cloud providers where LiteLLM manages the endpoint internally (gemini, and
    # openai/anthropic when no custom base URL is set), skip the HTTP pre-check —
    # there is no local endpoint to probe. Trust that the API key is present.
    if provider == "gemini" or (provider in ("openai", "anthropic") and not base):
        logger.info("[LLM] Provider %s: skipping HTTP check (managed endpoint), key present", provider)
        return True

    try:
        if provider == "ollama":
            url = f"{base}/api/tags"
            logger.debug("[LLM] GET %s (timeout=%ds)", url, AVAILABILITY_TIMEOUT)
            r = requests.get(url, timeout=AVAILABILITY_TIMEOUT)
        elif provider == "anthropic":
            headers = _anthropic_headers()
            if not headers.get("x-api-key"):
                logger.warning("[LLM] API key missing for provider=%s -- cannot check availability", provider)
                return False
            url = f"{base}/models"
            logger.debug("[LLM] GET %s (timeout=%ds)", url, AVAILABILITY_TIMEOUT)
            r = requests.get(url, headers=headers, timeout=AVAILABILITY_TIMEOUT)
        else:
            if not LLM_API_KEY:
                logger.warning("[LLM] API key missing for provider=%s -- cannot check availability", provider)
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
        logger.warning("[LLM] No model configured -- cannot proceed")
        return False

    if provider != "ollama":
        if not LLM_API_KEY:
            logger.warning("[LLM] No API key configured for provider=%s -- cannot use model %s", provider, model)
            return False
        logger.info("[LLM] API-based provider=%s with model=%s -- assuming available", provider, model)
        return True

    if model_exists(model):
        logger.info("[LLM] Ollama model '%s' already present", model)
        return True

    if not auto_pull:
        logger.warning("[LLM] Ollama model '%s' not found and auto_pull disabled", model)
        return False

    logger.info("[LLM] Ollama model '%s' not found -- pulling now (timeout=%ds)...", model, PULL_TIMEOUT)
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


_api_client = None

def _get_api_client():
    """Lazily get the WorkerAPIClient singleton (set by worker at startup)."""
    global _api_client
    return _api_client

def set_api_client(client):
    """Called by the worker to inject the shared API client for LLM logging."""
    global _api_client
    _api_client = client

import threading
_tl = threading.local()

def _log_to_db(provider: str, model: str, prompt: str, response: str, error: str, duration_ms: int):
    # Prefer HTTP API client (injected by worker)
    client = _get_api_client()
    if client is not None:
        try:
            client.create_llm_log(provider, model, prompt, response, error, duration_ms)
            return
        except Exception as e:
            logger.warning("Failed to log LLM call via API: %s", e)
            return

    # Fallback: direct SQLite (for scout which mounts /data)
    db_path = os.getenv("DB_PATH", "/data/clipfeed.db")
    if not os.path.exists(db_path):
        return
    try:
        import sqlite3
        if not hasattr(_tl, "conn"):
            _tl.conn = sqlite3.connect(db_path, timeout=5.0)
        _tl.conn.execute(
            "INSERT INTO llm_logs (system, model, prompt, response, error, duration_ms) VALUES (?, ?, ?, ?, ?, ?)",
            (provider, model, prompt, response, error, duration_ms)
        )
        _tl.conn.commit()
    except Exception as e:
        logger.warning("Failed to log LLM call to SQLite: %s", e)

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
    
    start = time.time()
    
    try:
        if provider != "ollama" and not LLM_API_KEY:
            logger.warning("[LLM] API key missing for provider=%s -- aborting generate", provider)
            return ""

        params = _litellm_params(model, max_tokens)
        logger.debug("[LLM] LiteLLM params: model=%s api_base=%s", params.get("model"), params.get("api_base"))

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
        
        _log_to_db(provider, model, prompt, result, "", int(elapsed * 1000))
        return result
    except Exception as e:
        elapsed = time.time() - start
        logger.error("[LLM] Generate FAILED: provider=%s model=%s error=%s", provider, model, e)
        _log_to_db(provider, model, prompt, "", str(e), int(elapsed * 1000))
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
    result = generate(prompt, max_tokens=64)
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
    result = generate(prompt, max_tokens=512)
    if not result:
        logger.warning("[LLM] Topic refinement returned empty -- keeping original topics: %s", keybert_topics)
        return list(keybert_topics)

    cleaned = _strip_code_fences(result)
    try:
        parsed = json.loads(cleaned)
        if isinstance(parsed, list):
            refined = [str(item.get("topic", item)) for item in parsed if item]
            logger.info("[LLM] Topics refined: %s -> %s", keybert_topics, refined)
            return refined
    except json.JSONDecodeError:
        # Try to extract JSON array from mixed response (greedy to handle nested brackets)
        match = re.search(r"\[[\s\S]*\]", cleaned)
        if match:
            try:
                parsed = json.loads(match.group(0))
                if isinstance(parsed, list):
                    refined = [str(item.get("topic", item)) for item in parsed if item]
                    logger.info("[LLM] Topics refined (extracted from text): %s -> %s", keybert_topics, refined)
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
    user_profile: str | None = None,
) -> float | None:
    """
    Rate relevance 1-10 given user interests.
    If user_profile is provided, uses personalized scoring with the user's
    actual interests, favorite channels, and topic preferences.
    Returns None on request/parse failure.
    """
    logger.info("[LLM] Evaluating candidate: title=%r channel=%r profile=%s",
                title[:80] if title else "", channel,
                (user_profile[:80] + "...") if user_profile and len(user_profile) > 80 else user_profile)
    topics_str = ", ".join(str(t) for t in top_topics) if top_topics else "(none)"

    if user_profile:
        prompt = (
            "You are a content recommendation engine. A user has the following interest profile:\n"
            f"{user_profile}\n\n"
            f"Rate how likely this user would enjoy the following video on a scale of 1-10:\n"
            f"Title: '{title}'\n"
            f"Channel: '{channel}'\n\n"
            "Consider topic relevance, channel familiarity, and content style alignment. "
            "A score of 10 means perfect match for this user's tastes, 1 means completely irrelevant. "
            "Reply with just the number."
        )
    else:
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
            logger.info("[LLM] Candidate scored: %.1f -- title=%r channel=%r", score, title[:60] if title else "", channel)
            return score
        except ValueError:
            pass

    logger.warning("[LLM] Could not parse score from LLM response: %r (title=%r)", result[:100], title[:60] if title else "")
    return None


def generate_search_queries(
    identifier: str,
    source_type: str,
    user_profile: str | None = None,
    existing_queries: list[str] | None = None,
    count: int = 4,
) -> list[str]:
    """
    Generate varied YouTube search queries that combine the source identifier
    with user interests. Each call should produce different queries to discover
    fresh content that the static identifier search would miss.

    Returns a list of search query strings. Falls back to simple variations
    if LLM is unavailable or fails.
    """
    fallbacks = [
        identifier,
        f"{identifier} new",
        f"{identifier} highlights",
        f"{identifier} best",
    ]

    if not _ai_enabled() or not is_available():
        logger.info("[LLM] AI unavailable -- using fallback search queries for %r", identifier)
        return fallbacks[:count]

    existing_str = ""
    if existing_queries:
        existing_str = f"\nAvoid repeating these previously used queries: {', '.join(existing_queries[-10:])}\n"

    profile_str = ""
    if user_profile:
        profile_str = f"\nUser interest profile: {user_profile}\n"

    prompt = (
        f"Generate {count} YouTube search queries to find new, fresh content related to '{identifier}' "
        f"(source type: {source_type}).\n"
        f"{profile_str}"
        f"{existing_str}"
        "The queries should:\n"
        "- Always include or reference the source identifier\n"
        "- Vary the angle: recent content, specific sub-topics, compilations, highlights, related creators\n"
        "- Mix the user's interests with the source to find cross-over content\n"
        "- Be practical YouTube search queries (not too long)\n"
        "- Focus on discovering NEW content the user hasn't seen\n\n"
        f"Return exactly {count} queries as a JSON array of strings. Example:\n"
        f'["{identifier} latest", "{identifier} funny moments"]\n'
        "Reply with only the JSON array."
    )

    result = generate(prompt, max_tokens=512)
    if not result:
        logger.warning("[LLM] Search query generation returned empty for %r -- using fallbacks", identifier)
        return fallbacks[:count]

    cleaned = _strip_code_fences(result)
    try:
        parsed = json.loads(cleaned)
        if isinstance(parsed, list) and all(isinstance(q, str) for q in parsed):
            queries = [q.strip() for q in parsed if q.strip()]
            if queries:
                logger.info("[LLM] Generated %d search queries for %r: %s",
                            len(queries), identifier, queries)
                return queries[:count]
    except json.JSONDecodeError:
        # Try extracting JSON array from mixed response
        match = re.search(r"\[[\s\S]*\]", cleaned)
        if match:
            try:
                parsed = json.loads(match.group(0))
                if isinstance(parsed, list):
                    queries = [str(q).strip() for q in parsed if q]
                    if queries:
                        logger.info("[LLM] Generated %d search queries (extracted) for %r: %s",
                                    len(queries), identifier, queries)
                        return queries[:count]
            except json.JSONDecodeError:
                pass

    logger.warning("[LLM] Could not parse search queries from response: %r -- using fallbacks", result[:200])
    return fallbacks[:count]
