"""
Shared Ollama HTTP client for the ClipFeed ingestion layer.
Provides LLM-backed helpers for title generation, topic refinement, and relevance scoring.
"""

import json
import logging
import os
import re

import requests

logger = logging.getLogger("ollama_client")

OLLAMA_URL = os.getenv("OLLAMA_URL", "http://ollama:11434").rstrip("/")
OLLAMA_MODEL = os.getenv("OLLAMA_MODEL", "llama3.2:3b")

# Timeouts
AVAILABILITY_TIMEOUT = 3
GENERATE_TIMEOUT = 30


def is_available() -> bool:
    """Check if Ollama is reachable (GET /api/tags, 3s timeout)."""
    try:
        r = requests.get(
            f"{OLLAMA_URL}/api/tags",
            timeout=AVAILABILITY_TIMEOUT,
        )
        r.raise_for_status()
        return True
    except requests.RequestException as e:
        logger.debug("Ollama unavailable: %s", e)
        return False


def generate(
    prompt: str,
    model: str | None = None,
    max_tokens: int = 256,
) -> str:
    """
    Call POST /api/generate with stream=false, return the response text.
    Uses OLLAMA_MODEL as default model. Timeout 30s.
    Returns empty string on any error.
    """
    model = model or OLLAMA_MODEL
    try:
        r = requests.post(
            f"{OLLAMA_URL}/api/generate",
            json={
                "model": model,
                "prompt": prompt,
                "stream": False,
                "options": {"num_predict": max_tokens},
            },
            timeout=GENERATE_TIMEOUT,
        )
        r.raise_for_status()
        data = r.json()
        return data.get("response", "").strip()
    except requests.RequestException as e:
        logger.warning("Ollama generate failed: %s", e)
        return ""
    except (json.JSONDecodeError, KeyError) as e:
        logger.warning("Ollama response parse error: %s", e)
        return ""


def generate_title(transcript: str, source_title: str = "") -> str:
    """
    Use LLM to generate a concise clip title.
    Returns empty string on failure.
    """
    excerpt = transcript[:500] if transcript else ""
    prompt = (
        "Generate a concise, engaging title (5-10 words) for this video clip. "
        "Only respond with the title, no quotes or explanation.\n\n"
        f"Source: {source_title}\n"
        f"Transcript: {excerpt}"
    )
    result = generate(prompt)
    if not result:
        return ""
    return result.strip()


def refine_topics(transcript: str, keybert_topics: list) -> list:
    """
    Send transcript excerpt and KeyBERT topics to LLM asking it to confirm/refine them
    and suggest a parent category. Returns list of topic strings.
    Returns original keybert_topics on failure.
    """
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
        return list(keybert_topics)

    try:
        parsed = json.loads(result)
        if isinstance(parsed, list):
            return [str(item.get("topic", item)) for item in parsed if item]
    except json.JSONDecodeError:
        # Try to extract JSON from markdown or mixed response
        match = re.search(r"\[[\s\S]*?\]", result)
        if match:
            try:
                parsed = json.loads(match.group(0))
                if isinstance(parsed, list):
                    return [str(item.get("topic", item)) for item in parsed if item]
            except json.JSONDecodeError:
                pass
        logger.warning("Ollama refine_topics: could not parse JSON from %r", result[:200])

    return list(keybert_topics)


def evaluate_candidate(
    title: str,
    channel: str,
    top_topics: list,
) -> float:
    """
    Rate relevance 1-10 given user interests.
    Returns 0.0 on failure.
    """
    topics_str = ", ".join(str(t) for t in top_topics) if top_topics else "(none)"
    prompt = (
        f"Given these user interests: {topics_str}. "
        f"Rate 1-10 how relevant this video is: '{title}' by '{channel}'. "
        "Reply with just the number."
    )
    result = generate(prompt)
    if not result:
        return 0.0

    match = re.search(r"(\d+(?:\.\d+)?)", result.strip())
    if match:
        try:
            score = float(match.group(1))
            return max(0.0, min(10.0, score))
        except ValueError:
            pass

    logger.warning("Ollama evaluate_candidate: could not parse score from %r", result[:100])
    return 0.0
