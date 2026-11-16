"""Groq ModelClient (OpenAI-compatible chat completions).

Replaces Vertex AI (Gemini) per owner decision: Groq has a free tier, Vertex does not.
Groq has no embedding model, so embeddings run locally (see copilot.embed.LocalEmbedder).

Free-tier rate limits apply per model; tokens-per-minute (TPM) is the binding constraint,
not requests-per-minute. Exceeding it returns HTTP 429, which is raised here so GuardedModel
retries with backoff and eventually falls back. Check current limits on the Groq console.

The API key is read from GROQ_API_KEY at call time and never appears in logs or errors.
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
GROQ_URL = "https://api.groq.com/openai/v1/chat/completions"
TIMEOUT_S = 30.0


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
        key = os.environ.get("GROQ_API_KEY", "").strip()
        if not key:
            raise RuntimeError("GROQ_API_KEY is not set; the Groq model client is unavailable")
        model = self._model or os.environ.get("GROQ_MODEL") or DEFAULT_MODEL
        payload: dict = {"model": model, "messages": messages, "temperature": 0}
        if tools:
            payload["tools"] = tools
            payload["tool_choice"] = "auto"
        async with httpx.AsyncClient(timeout=TIMEOUT_S, transport=self._transport) as client:
            resp = await client.post(
                GROQ_URL, json=payload, headers={"Authorization": f"Bearer {key}"}
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
