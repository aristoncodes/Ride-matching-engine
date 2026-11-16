"""Provider-agnostic model-call wrapper: timeout, retry, cache, circuit breaker (ticket 2.7).

Never logs prompts or model outputs; only attempt counts and error class names.
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import time
from collections.abc import Awaitable, Callable
from typing import Any, Protocol

log = logging.getLogger(__name__)

CACHE_TTL_S = 3600
DEFAULT_REDIS_URL = "redis://localhost:6379/0"


class ModelClient(Protocol):
    async def generate(self, system: str, user: str) -> str: ...


class ModelUnavailable(Exception):
    """The model could not produce an answer; callers return the fallback response."""


class Cache(Protocol):
    async def get(self, key: str) -> str | None: ...
    async def set(self, key: str, value: str, ttl_s: int) -> None: ...


class InMemoryCache:
    def __init__(self) -> None:
        self._data: dict[str, str] = {}

    async def get(self, key: str) -> str | None:
        return self._data.get(key)

    async def set(self, key: str, value: str, ttl_s: int) -> None:
        self._data[key] = value


class RedisCache:
    """Redis-backed cache. Connects lazily; nothing happens at import or construction."""

    def __init__(self, url: str | None = None) -> None:
        self._url = url or os.environ.get("REDIS_URL", DEFAULT_REDIS_URL)
        self._client: Any = None

    def _conn(self) -> Any:
        if self._client is None:
            import redis.asyncio as aioredis

            self._client = aioredis.from_url(self._url, decode_responses=True)
        return self._client

    async def get(self, key: str) -> str | None:
        return await self._conn().get(key)

    async def set(self, key: str, value: str, ttl_s: int) -> None:
        await self._conn().set(key, value, ex=ttl_s)


def cache_key(system: str, user: str) -> str:
    return hashlib.sha256((system + "\n" + user).encode("utf-8")).hexdigest()


class GuardedModel:
    def __init__(
        self,
        inner: ModelClient,
        *,
        cache: Cache,
        timeout_s: float = 20.0,
        retries: int = 2,
        breaker_threshold: int = 5,
        breaker_cooldown_s: float = 60.0,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._inner = inner
        self._cache = cache
        self._timeout_s = timeout_s
        self._retries = retries
        self._threshold = breaker_threshold
        self._cooldown_s = breaker_cooldown_s
        self._clock = clock
        self._sleep = sleep
        self._failures = 0
        self._opened_at: float | None = None
        self._trial_in_flight = False

    async def _cache_get(self, key: str) -> str | None:
        try:
            return await self._cache.get(key)
        except Exception as exc:
            log.warning("model cache get failed: %s", type(exc).__name__)
            return None

    async def _cache_set(self, key: str, value: str) -> None:
        try:
            await self._cache.set(key, value, CACHE_TTL_S)
        except Exception as exc:
            log.warning("model cache set failed: %s", type(exc).__name__)

    def _admit(self) -> bool:
        """Return True if this call is a half-open trial. Raise if the breaker rejects."""
        if self._opened_at is None:
            return False
        if self._clock() - self._opened_at < self._cooldown_s or self._trial_in_flight:
            raise ModelUnavailable("circuit breaker open")
        self._trial_in_flight = True
        return True

    def _record_success(self) -> None:
        self._failures = 0
        self._opened_at = None
        self._trial_in_flight = False

    def _record_failure(self, trial: bool) -> None:
        self._failures += 1
        if trial or self._failures >= self._threshold:
            self._opened_at = self._clock()
            log.warning("model circuit breaker opened after %d failures", self._failures)
        self._trial_in_flight = False

    async def generate(self, system: str, user: str) -> str:
        key = cache_key(system, user)
        cached = await self._cache_get(key)
        if cached is not None:
            return cached

        trial = self._admit()
        attempts = self._retries + 1
        last: BaseException | None = None
        for attempt in range(attempts):
            try:
                out = await asyncio.wait_for(
                    self._inner.generate(system, user), timeout=self._timeout_s
                )
            except Exception as exc:  # includes TimeoutError
                last = exc
                log.warning(
                    "model call failed: attempt %d/%d error=%s",
                    attempt + 1,
                    attempts,
                    type(exc).__name__,
                )
                if attempt < attempts - 1:
                    await self._sleep(0.5 * (2**attempt))
                continue
            self._record_success()
            await self._cache_set(key, out)
            return out

        self._record_failure(trial)
        raise ModelUnavailable(
            f"model failed after {attempts} attempts: {type(last).__name__}"
        )
