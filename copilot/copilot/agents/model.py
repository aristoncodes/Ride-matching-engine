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
from contextvars import ContextVar
from dataclasses import dataclass
from typing import Any, Protocol, TypeVar

from copilot.obs import metrics

log = logging.getLogger(__name__)

T = TypeVar("T")

CACHE_TTL_S = 3600
REDIS_TIMEOUT_S = 1.0
DEFAULT_REDIS_URL = "redis://localhost:6379/0"


@dataclass(frozen=True)
class ModelUsage:
    tokens_in: int
    tokens_out: int
    model: str


class _Acc:
    """Mutable per-investigation usage total, shared by the tasks of one investigation."""

    def __init__(self) -> None:
        self.tokens_in = 0
        self.tokens_out = 0
        self.model = ""


# Per-investigation accumulator; asyncio.gather children copy the context and share the object.
_acc_var: ContextVar[_Acc | None] = ContextVar("copilot_usage_acc", default=None)
# Per-call sink: GuardedModel sets a fresh list before each inner call; the client appends to
# it. wait_for runs the call in a child task, which shares the list through the copied context.
_sink_var: ContextVar[list[ModelUsage] | None] = ContextVar("copilot_usage_sink", default=None)


def _observe_call(sink: list[ModelUsage], elapsed_s: float) -> None:
    """Latency and output tokens/sec for one successful call. Never raises."""
    metrics.MODEL_CALL_SECONDS.observe(max(elapsed_s, 0.0))
    if elapsed_s <= 0:
        return
    for usage in sink:
        if usage.tokens_out > 0:
            metrics.MODEL_TOKENS_PER_SECOND.observe(usage.tokens_out / elapsed_s)


def record_usage(usage: ModelUsage) -> None:
    """Called by a ModelClient after a call to report its token usage."""
    sink = _sink_var.get()
    if sink is not None:
        sink.append(usage)


@dataclass(frozen=True)
class ToolCall:
    id: str
    name: str
    arguments: dict
    error: str | None = None  # set when the arguments string was not valid JSON


@dataclass
class ChatResult:
    text: str
    tool_calls: list[ToolCall]


class ModelClient(Protocol):
    async def generate(self, system: str, user: str) -> str: ...

    async def chat(self, messages: list[dict], tools: list[dict] | None = None) -> ChatResult: ...


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

            self._client = aioredis.from_url(
                self._url,
                decode_responses=True,
                socket_timeout=REDIS_TIMEOUT_S,
                socket_connect_timeout=REDIS_TIMEOUT_S,
            )
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
        total_budget_s: float = 45.0,
        retries: int = 2,
        breaker_threshold: int = 5,
        breaker_cooldown_s: float = 60.0,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self._inner = inner
        self._cache = cache
        self._timeout_s = timeout_s
        self._total_budget_s = total_budget_s
        self._retries = retries
        self._threshold = breaker_threshold
        self._cooldown_s = breaker_cooldown_s
        self._clock = clock
        self._sleep = sleep
        self._failures = 0
        self._opened_at: float | None = None
        self._trial_in_flight = False

    def reset_usage(self) -> None:
        """Start a fresh usage total for the current investigation."""
        _acc_var.set(_Acc())

    def usage_total(self) -> ModelUsage:
        acc = _acc_var.get()
        if acc is None:
            return ModelUsage(0, 0, "")
        return ModelUsage(acc.tokens_in, acc.tokens_out, acc.model)

    async def _cache_get(self, key: str) -> str | None:
        try:
            return await asyncio.wait_for(self._cache.get(key), timeout=REDIS_TIMEOUT_S)
        except Exception as exc:
            log.warning("model cache get failed: %s", type(exc).__name__)
            return None

    async def _cache_set(self, key: str, value: str) -> None:
        try:
            await asyncio.wait_for(
                self._cache.set(key, value, CACHE_TTL_S), timeout=REDIS_TIMEOUT_S
            )
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

    def _add_usage(self, sink: list[ModelUsage]) -> None:
        acc = _acc_var.get()
        if acc is None:
            acc = _Acc()
            _acc_var.set(acc)
        for u in sink:
            acc.tokens_in += u.tokens_in
            acc.tokens_out += u.tokens_out
            acc.model = u.model or acc.model

    async def generate(self, system: str, user: str) -> str:
        key = cache_key(system, user)
        cached = await self._cache_get(key)
        if cached is not None:
            return cached
        out = await self._guarded(lambda: self._inner.generate(system, user))
        await self._cache_set(key, out)
        return out

    async def chat(self, messages: list[dict], tools: list[dict] | None = None) -> ChatResult:
        """Chat turn with optional native tools. Never cached: tool turns depend on history."""
        return await self._guarded(lambda: self._inner.chat(messages, tools))

    async def _guarded(self, call: Callable[[], Awaitable[T]]) -> T:
        """One path for timeout, retry with backoff, breaker and usage accumulation."""
        trial = self._admit()
        attempts = self._retries + 1
        last: BaseException | None = None
        start = self._clock()
        settled = False
        try:
            for attempt in range(attempts):
                remaining = self._total_budget_s - (self._clock() - start)
                if remaining <= 0:
                    break
                sink: list[ModelUsage] = []
                _sink_var.set(sink)
                call_start = self._clock()
                try:
                    out = await asyncio.wait_for(call(), timeout=min(self._timeout_s, remaining))
                except Exception as exc:  # includes TimeoutError
                    last = exc
                    log.warning(
                        "model call failed: attempt %d/%d error=%s",
                        attempt + 1,
                        attempts,
                        type(exc).__name__,
                    )
                    if attempt < attempts - 1:
                        delay = 0.5 * (2**attempt)
                        if self._clock() - start + delay >= self._total_budget_s:
                            break  # the next attempt would not fit in the budget
                        await self._sleep(delay)
                    continue
                settled = True
                self._add_usage(sink)
                _observe_call(sink, self._clock() - call_start)
                self._record_success()
                return out

            settled = True
            self._record_failure(trial)
            name = type(last).__name__ if last else "BudgetExhausted"
            raise ModelUnavailable(f"model failed after {attempts} attempts: {name}")
        except asyncio.CancelledError:
            # Cancelled inside a call (e.g. the outer deadline): that counts as a failure.
            if not settled:
                settled = True
                self._record_failure(trial)
            raise
        finally:
            if not settled:
                self._trial_in_flight = False
