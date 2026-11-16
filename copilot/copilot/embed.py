"""Embedding backends: a Protocol, a local fastembed implementation, and a fake for tests."""

from __future__ import annotations

import asyncio
import hashlib
import math
from typing import Any, Protocol, runtime_checkable

LOCAL_MODEL = "BAAI/bge-small-en-v1.5"
LOCAL_DIM = 384


@runtime_checkable
class Embedder(Protocol):
    dim: int

    async def embed(self, texts: list[str]) -> list[list[float]]: ...


def _sha256(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


class LocalEmbedder:
    """Local fastembed model (free; replaces Vertex text-embedding-005 per owner decision).

    Groq has no embedding model. bge-small-en-v1.5 gives 384-dim vectors. fastembed is
    synchronous, so the call runs in a thread. In-memory sha256-keyed cache.
    """

    dim: int = LOCAL_DIM

    def __init__(self, model_name: str = LOCAL_MODEL) -> None:
        self._model_name = model_name
        self._model: Any = None
        self._cache: dict[str, list[float]] = {}

    def _get_model(self) -> Any:
        if self._model is None:
            try:
                from fastembed import TextEmbedding
            except ImportError as exc:
                raise RuntimeError("fastembed is not installed. Run `uv add fastembed`.") from exc
            self._model = TextEmbedding(model_name=self._model_name)
        return self._model

    def _embed_sync(self, texts: list[str]) -> list[list[float]]:
        model = self._get_model()
        return [[float(v) for v in vec] for vec in model.embed(texts)]

    async def embed(self, texts: list[str]) -> list[list[float]]:
        keys = [_sha256(t) for t in texts]
        pending: dict[str, str] = {}
        for key, text in zip(keys, texts, strict=True):
            if key not in self._cache:
                pending.setdefault(key, text)
        items = list(pending.items())
        if items:
            vecs = await asyncio.to_thread(self._embed_sync, [t for _, t in items])
            for (key, _), vec in zip(items, vecs, strict=True):
                self._cache[key] = vec
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

    def __init__(self, dim: int = 384) -> None:
        self.dim = dim

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [_hash_vector(t, self.dim) for t in texts]
