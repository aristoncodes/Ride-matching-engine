"""OpenAI-compatible ModelClient for Groq or a local Ollama server.

The same client serves Groq (free tier, rate-limited) and a local Ollama server (no limits,
slower), selected by LLM_BASE_URL. The default is Groq. A base URL anywhere else, such as
http://localhost:11434/v1 or http://host.docker.internal:11434/v1, is treated as a local
server: no Authorization header is sent and no API key is required. Set the model name in
GROQ_MODEL (for example qwen2.5:3b for Ollama). A local server may answer 500 while a model
is loading; that raises like any 5xx so GuardedModel retries.

Replaces Vertex AI (Gemini) per owner decision: Groq has a free tier, Vertex does not.
Groq has no embedding model, so embeddings run locally (see copilot.embed.LocalEmbedder).

Free-tier rate limits apply per model; tokens-per-minute (TPM) is the binding constraint,
not requests-per-minute. Exceeding it returns HTTP 429, which is raised here so GuardedModel
retries with backoff and eventually falls back. Check current limits on the Groq console.

For Groq, the API key is read from GROQ_API_KEY at call time and never appears in logs or errors.
Prompts and outputs are never logged.

Default model is qwen/qwen3.8-27b: openai/gpt-oss-120b, called with no tools offered, emits
a built-in tool call that Groq rejects with HTTP 400 (tool_use_failed). Qwen replies in plain
text. Override with GROQ_MODEL.
"""

from __future__ import annotations

import json
import os

import httpx

from copilot.agents.model import ChatResult, ModelUsage, ToolCall, record_usage

DEFAULT_MODEL = "qwen/qwen3.8-27b"
GROQ_BASE_URL = "https://api.groq.com/openai/v1"
# Per-HTTP-call timeout. MODEL_CALL_TIMEOUT_S raises it for a slow local model (set it with the
# GuardedModel timeout in components.py so the two stay in step).
TIMEOUT_S = float(os.environ.get("MODEL_CALL_TIMEOUT_S", "30"))
# Hosts treated as a local server: no key is sent and none is required.
LOCAL_HOSTS = frozenset({"localhost", "127.0.0.1", "::1", "host.docker.internal"})


class GroqClient:
    def __init__(
        self, model: str | None = None, transport: httpx.AsyncBaseTransport | None = None
    ) -> None:
        self._model = model
        self._transport = transport  # tests inject httpx.MockTransport
        self.last_usage: ModelUsage | None = None  # usage of the most recent call

    async def generate(self, system: str, user: str) -> str:
        res = await self.chat(
            [{"role": "system", "content": system}, {"role": "user", "content": user}]
        )
        return res.text

    async def chat(self, messages: list[dict], tools: list[dict] | None = None) -> ChatResult:
        base = (os.environ.get("LLM_BASE_URL") or "").strip().rstrip("/") or GROQ_BASE_URL
        headers: dict[str, str] = {}
        if httpx.URL(base).host not in LOCAL_HOSTS:
            # Any hosted OpenAI-compatible provider (Groq, Gemini's OpenAI endpoint) needs a key.
            # LLM_API_KEY is the provider-neutral name; GROQ_API_KEY still works for Groq.
            key = (os.environ.get("LLM_API_KEY") or os.environ.get("GROQ_API_KEY") or "").strip()
            if not key:
                raise RuntimeError(
                    "No model API key is set (LLM_API_KEY or GROQ_API_KEY); "
                    "the model client is unavailable"
                )
            headers["Authorization"] = f"Bearer {key}"
        model = self._model or os.environ.get("GROQ_MODEL") or DEFAULT_MODEL
        payload: dict = {"model": model, "messages": messages, "temperature": 0}
        if tools:
            payload["tools"] = tools
            payload["tool_choice"] = "auto"
        async with httpx.AsyncClient(timeout=TIMEOUT_S, transport=self._transport) as client:
            resp = await client.post(
                base + "/chat/completions", json=payload, headers=headers
            )
        # 429 / 5xx raise HTTPStatusError so GuardedModel retries. The message holds only the
        # status and URL, never the request headers.
        resp.raise_for_status()
        data = resp.json()
        usage_obj = data.get("usage") or {}
        usage = ModelUsage(
            int(usage_obj.get("prompt_tokens") or 0),
            int(usage_obj.get("completion_tokens") or 0),
            model,
        )
        self.last_usage = usage
        record_usage(usage)
        message = data["choices"][0]["message"]
        return ChatResult(message.get("content") or "", _parse_tool_calls(message))


def _parse_tool_calls(message: dict) -> list[ToolCall]:
    calls: list[ToolCall] = []
    for i, tc in enumerate(message.get("tool_calls") or []):
        if not isinstance(tc, dict):
            continue
        fn = tc.get("function") or {}
        raw = fn.get("arguments")
        args: dict = {}
        error: str | None = None
        if isinstance(raw, dict):
            args = raw
        elif raw is None or (isinstance(raw, str) and not raw.strip()):
            args = {}
        else:
            try:
                decoded = json.loads(raw)
            except (ValueError, TypeError):
                decoded = None
                error = "arguments were not valid JSON"
            if error is None and not isinstance(decoded, dict):
                error = "arguments must be a JSON object"
            elif error is None:
                args = decoded
        calls.append(
            ToolCall(str(tc.get("id") or f"call_{i}"), str(fn.get("name") or ""), args, error)
        )
    return calls
