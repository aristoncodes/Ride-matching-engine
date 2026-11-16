"""Vertex AI (Gemini) ModelClient. Lazy import; env read at call time. Never logs prompts."""

from __future__ import annotations

import os
from typing import Any

DEFAULT_MODEL = "gemini-2.5-flash"


class VertexClient:
    def __init__(self, model: str | None = None) -> None:
        self._model = model
        self._client: Any = None

    def _conn(self) -> Any:
        if self._client is None:
            from google import genai

            self._client = genai.Client(
                vertexai=True,
                project=os.environ["VERTEX_PROJECT"],
                location=os.environ.get("VERTEX_LOCATION", "us-central1"),
            )
        return self._client

    async def generate(self, system: str, user: str) -> str:
        from google.genai import types

        model = self._model or os.environ.get("VERTEX_MODEL", DEFAULT_MODEL)
        resp = await self._conn().aio.models.generate_content(
            model=model,
            contents=user,
            config=types.GenerateContentConfig(system_instruction=system, temperature=0.0),
        )
        return resp.text or ""
