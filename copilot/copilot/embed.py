"""Embedding backends: a Protocol, a Vertex AI implementation, and a fake for tests."""

from __future__ import annotations

import asyncio
import hashlib
import math
import os
from typing import Any, Protocol, runtime_checkable

VERTEX_MODEL = "text-embedding-005"
VERTEX_DIM = 768
VERTEX_BATCH = 64


@runtime_checkable
class Embedder(Protocol):
    dim: int

    async def embed(self, texts: list[str]) -> list[list[float]]: ...


def _sha256(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def _vertex_settings() -> tuple[str, str]:
    """Read Vertex project and location from the environment at call time."""
    project = os.environ.get("VERTEX_PROJECT", "").strip()
    location = os.environ.get("VERTEX_LOCATION", "").strip()
    if not project or not location:
        raise RuntimeError(
            "VERTEX_PROJECT and VERTEX_LOCATION must be set to use the Vertex embedder"
        )
    return project, location


def _load_vertex() -> tuple[Any, Any]:
    try:
        import vertexai
        from vertexai.language_models import TextEmbeddingModel
    except ImportError as exc:
        raise RuntimeError(
            "google-cloud-aiplatform is not installed. Run `uv add google-cloud-aiplatform`."
        ) from exc
    return vertexai, TextEmbeddingModel


class VertexEmbedder:
    """text-embedding-005 via google-cloud-aiplatform, with an in-memory sha256-keyed cache."""

    dim: int = VERTEX_DIM

    def __init__(self, model_name: str = VERTEX_MODEL, timeout_s: float = 10.0) -> None:
        self._model_name = model_name
        self._timeout_s = timeout_s
        self._model: Any = None
        self._cache: dict[str, list[float]] = {}

    def _get_model(self) -> Any:
        if self._model is None:
            project, location = _vertex_settings()
            vertexai, text_model_cls = _load_vertex()
            vertexai.init(project=project, location=location)
            self._model = text_model_cls.from_pretrained(self._model_name)
        return self._model

    async def embed(self, texts: list[str]) -> list[list[float]]:
        keys = [_sha256(t) for t in texts]
        pending: dict[str, str] = {}
        for key, text in zip(keys, texts, strict=True):
            if key not in self._cache:
                pending.setdefault(key, text)

        items = list(pending.items())
        if items:
            model = self._get_model()
            for start in range(0, len(items), VERTEX_BATCH):
                batch = items[start : start + VERTEX_BATCH]
                try:
                    results = await asyncio.wait_for(
                        asyncio.to_thread(model.get_embeddings, [t for _, t in batch]),
                        timeout=self._timeout_s,
                    )
                except TimeoutError as exc:
                    raise RuntimeError(
                        f"Vertex embedding call timed out after {self._timeout_s:g}s"
                    ) from exc
                for (key, _), result in zip(batch, results, strict=True):
                    self._cache[key] = [float(v) for v in result.values]

        return [list(self._cache[k]) for k in keys]


def _hash_vector(text: str, dim: int) -> list[float]:
    seed = hashlib.sha256(text.encode("utf-8")).digest()
    values: list[float] = []
    counter = 0
    while len(values) < dim:
        block = hashlib.sha256(seed + counter.to_bytes(4, "big")).digest()
        for i in range(0, len(block), 4):
            if len(values) == dim:
                break
            n = int.from_bytes(block[i : i + 4], "big")
            values.append(n / 2**31 - 1.0)
        counter += 1
    norm = math.sqrt(sum(v * v for v in values)) or 1.0
    return [v / norm for v in values]


class FakeEmbedder:
    """Deterministic, hash-based, unit-normalised vectors. For tests only."""

    def __init__(self, dim: int = 8) -> None:
        self.dim = dim

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [_hash_vector(t, self.dim) for t in texts]
