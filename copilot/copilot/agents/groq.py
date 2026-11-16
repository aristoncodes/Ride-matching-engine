"""Groq ModelClient (OpenAI-compatible chat completions).

Replaces Vertex AI (Gemini) per owner decision: Groq has a free tier, Vertex does not.
Groq has no embedding model, so embeddings run locally (see copilot.embed.LocalEmbedder).

Free-tier rate limits apply per model; tokens-per-minute (TPM) is the binding constraint,
not requests-per-minute. Exceeding it returns HTTP 429, which is raised here so GuardedModel
retries with backoff and eventually falls back. Check current limits on the Groq console.

The API key is read from GROQ_API_KEY at call time and never appears in logs or errors.
Prompts and outputs are never logged.
"""

from __future__ import annotations

import os

import httpx

from copilot.agents.model import ModelUsage, record_usage

DEFAULT_MODEL = "openai/gpt-oss-120b"
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
        key = os.environ.get("GROQ_API_KEY", "").strip()
        if not key:
            raise RuntimeError("GROQ_API_KEY is not set; the Groq model client is unavailable")
        model = self._model or os.environ.get("GROQ_MODEL") or DEFAULT_MODEL
        payload = {
            "model": model,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            "temperature": 0,
        }
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
        return data["choices"][0]["message"].get("content") or ""
