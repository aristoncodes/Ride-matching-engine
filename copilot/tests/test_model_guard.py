"""Tests for the model-call wrapper (ticket 2.7). No network."""

from __future__ import annotations

import asyncio
import logging

import pytest

from copilot.agents.model import GuardedModel, InMemoryCache, ModelUnavailable

SECRET_SYS = "SYSTEM-SECRET-PROMPT"
SECRET_USER = "USER-SECRET-LOGLINE"


class FakeInner:
    def __init__(self, fail_times: int = 0, delay: float = 0.0, answer: str = "ANSWER-TEXT"):
        self.calls = 0
        self.fail_times = fail_times
        self.delay = delay
        self.answer = answer

    async def generate(self, system: str, user: str) -> str:
        self.calls += 1
        if self.delay:
            await asyncio.sleep(self.delay)
        if self.calls <= self.fail_times:
            raise RuntimeError("boom " + user)
        return self.answer


class Clock:
    def __init__(self) -> None:
        self.t = 1000.0

    def __call__(self) -> float:
        return self.t


async def no_sleep(_: float) -> None:
    return None


def make(inner, cache=None, **kw):
    kw.setdefault("sleep", no_sleep)
    return GuardedModel(inner, cache=cache or InMemoryCache(), **kw)


def run(coro):
    return asyncio.run(coro)


def test_success_cached():
    inner = FakeInner()
    m = make(inner)

    async def go():
        a = await m.generate("s", "u")
        b = await m.generate("s", "u")
        return a, b

    assert run(go()) == ("ANSWER-TEXT", "ANSWER-TEXT")
    assert inner.calls == 1


def test_transient_failure_then_success():
    inner = FakeInner(fail_times=2)
    assert run(make(inner).generate("s", "u")) == "ANSWER-TEXT"
    assert inner.calls == 3


def test_permanent_failure_raises():
    inner = FakeInner(fail_times=99)
    with pytest.raises(ModelUnavailable):
        run(make(inner).generate("s", "u"))
    assert inner.calls == 3


def test_backoff_delays():
    delays: list[float] = []

    async def rec(d: float) -> None:
        delays.append(d)

    with pytest.raises(ModelUnavailable):
        run(make(FakeInner(fail_times=99), sleep=rec).generate("s", "u"))
    assert delays == [0.5, 1.0]


def test_timeout_counts_as_failure():
    inner = FakeInner(delay=0.5)
    with pytest.raises(ModelUnavailable):
        run(make(inner, timeout_s=0.05).generate("s", "u"))
    assert inner.calls == 3


def test_breaker_open_halfopen_close():
    clock = Clock()
    inner = FakeInner(fail_times=6)
    m = make(inner, retries=0, breaker_threshold=3, breaker_cooldown_s=60, clock=clock)

    async def go():
        for i in range(3):
            with pytest.raises(ModelUnavailable):
                await m.generate("s", f"u{i}")
        assert inner.calls == 3
        with pytest.raises(ModelUnavailable):
            await m.generate("s", "x")
        assert inner.calls == 3  # rejected without calling inner
        clock.t += 61
        with pytest.raises(ModelUnavailable):  # trial call fails (call 4) -> reopens
            await m.generate("s", "y")
        assert inner.calls == 4
        with pytest.raises(ModelUnavailable):
            await m.generate("s", "z")
        assert inner.calls == 4
        inner.fail_times = 0
        clock.t += 61
        assert await m.generate("s", "ok") == "ANSWER-TEXT"  # trial succeeds, closes
        assert await m.generate("s", "ok2") == "ANSWER-TEXT"
        assert inner.calls == 6

    run(go())


class BrokenCache:
    async def get(self, key):
        raise ConnectionError("down")

    async def set(self, key, value, ttl_s):
        raise ConnectionError("down")


def test_cache_errors_do_not_break():
    inner = FakeInner()
    assert run(make(inner, cache=BrokenCache()).generate("s", "u")) == "ANSWER-TEXT"


def test_no_prompt_in_logs(caplog):
    caplog.set_level(logging.DEBUG)
    inner = FakeInner(fail_times=99)
    m = make(inner, cache=BrokenCache(), retries=1, breaker_threshold=1)

    async def go():
        for _ in range(2):
            with pytest.raises(ModelUnavailable):
                await m.generate(SECRET_SYS, SECRET_USER)

    run(go())
    run(make(FakeInner(answer="OUT-SECRET")).generate(SECRET_SYS, SECRET_USER))
    assert caplog.records
    text = caplog.text
    for s in (SECRET_SYS, SECRET_USER, "OUT-SECRET"):
        assert s not in text


def test_cancel_clears_trial_flag_and_counts_failure():
    clock = Clock()
    inner = FakeInner(fail_times=1)
    m = make(inner, retries=0, breaker_threshold=1, breaker_cooldown_s=60, clock=clock)

    async def go():
        with pytest.raises(ModelUnavailable):
            await m.generate("s", "a")  # opens the breaker
        clock.t += 61
        inner.delay = 10
        task = asyncio.ensure_future(m.generate("s", "b"))  # half-open trial
        await asyncio.sleep(0.05)
        assert m._trial_in_flight is True
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert m._trial_in_flight is False
        assert m._opened_at == clock.t  # cancellation re-opened the breaker
        clock.t += 61
        inner.delay = 0
        assert await m.generate("s", "c") == "ANSWER-TEXT"  # a new trial is admitted

    run(go())


def test_total_budget_stops_retries_early():
    clock = Clock()
    inner = FakeInner(fail_times=99)

    async def sleep(d):
        clock.t += d

    m = make(inner, retries=5, timeout_s=20, total_budget_s=1.2, clock=clock, sleep=sleep)
    with pytest.raises(ModelUnavailable):
        run(m.generate("s", "u"))
    # backoffs 0.5 then 1.0: the third attempt would land at 1.5 > 1.2
    assert inner.calls == 2
