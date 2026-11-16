import json
import math

import httpx
import pytest

import copilot.detect.zscore as zscore
from copilot.detect import (
    AD1_QUERIES,
    DetectionEvent,
    detect,
    fetch_series,
    prometheus_url,
    run_once,
    to_diagnose_payload,
)

STEP = 15.0


def _series(values: list[float], t0: float = 0.0) -> list[tuple[float, float]]:
    return [(t0 + i * STEP, v) for i, v in enumerate(values)]


def _noisy(base: float, n: int, amp: float = 0.1) -> list[float]:
    return [base + (amp if i % 2 else -amp) for i in range(n)]


def test_flat_series_fires_nothing() -> None:
    assert detect({"m": _series([10.0] * 100)}) == []


def test_single_spike_does_not_fire_with_sustain_3() -> None:
    values = _noisy(10.0, 80)
    values[40] = 100.0
    assert detect({"m": _series(values)}, sustain=3) == []


def test_sustained_step_change_fires_exactly_once() -> None:
    values = _noisy(10.0, 30) + _noisy(100.0, 40)
    points = _series(values)
    events = detect({"m": points})
    assert len(events) == 1
    event = events[0]
    assert event.metric == "m"
    assert event.first_ts == points[30][0]
    assert event.last_ts == points[32][0]
    assert event.peak_z > 3.0
    assert event.value_at_detection == pytest.approx(100.0, abs=0.2)


def test_constant_series_std_floor_avoids_divide_by_zero() -> None:
    # Constant history has std 0; the floor keeps z finite.
    values = [5.0] * 30 + [50.0] * 3
    events = detect({"m": _series(values)})
    assert len(events) == 1
    assert math.isfinite(events[0].peak_z)


def test_constant_series_is_silent() -> None:
    assert detect({"a": _series([0.0] * 60), "b": _series([7.0] * 60)}) == []


def test_flat_zero_series_single_step_to_one_does_not_fire() -> None:
    # Absolute floor of 1.0 means a step of 1 is z = 1, below the threshold.
    values = [0.0] * 60
    values[40] = 1.0
    assert detect({"m": _series(values)}) == []


def test_flat_zero_series_sustained_step_to_fifty_fires() -> None:
    values = [0.0] * 30 + [50.0] * 5
    points = _series(values)
    events = detect({"m": points})
    assert len(events) == 1
    assert events[0].first_ts == points[30][0]
    assert events[0].value_at_detection == 50.0


def test_baseline_frozen_during_deviant_run_still_fires() -> None:
    # With window=5 and sustain=5, a baseline that absorbed the step would go quiet
    # before the run reached 5 points. Frozen baseline keeps every step point deviant.
    points = _series(_noisy(10.0, 5) + [100.0] * 6)
    events = detect({"m": points}, window=5, sustain=5)
    assert len(events) == 1
    assert events[0].first_ts == points[5][0]
    assert events[0].last_ts == points[9][0]


def test_fewer_points_than_window_fires_nothing() -> None:
    assert detect({"m": _series([10.0] * 5 + [1000.0] * 5)}, window=30) == []


def test_invalid_parameters_rejected() -> None:
    with pytest.raises(ValueError):
        detect({}, window=1)
    with pytest.raises(ValueError):
        detect({}, sustain=0)


def test_to_diagnose_payload_shape() -> None:
    event = DetectionEvent("queue_depth", 1.0, 2.0, 4.5, 99.0)
    assert to_diagnose_payload(event) == {
        "source": "detector",
        "alert": None,
        "question": None,
        "detection": {
            "metric": "queue_depth",
            "first_ts": 1.0,
            "last_ts": 2.0,
            "peak_z": 4.5,
            "value_at_detection": 99.0,
            "reason": "zscore",
        },
    }


@pytest.mark.asyncio
async def test_run_once_skips_events_already_in_seen(monkeypatch: pytest.MonkeyPatch) -> None:
    step_points = _series(_noisy(10.0, 30) + _noisy(100.0, 10))

    async def fake_fetch(client, base_url, promql, start, end, step):  # type: ignore[no-untyped-def]
        if promql == AD1_QUERIES["queue_depth"]:
            return step_points
        return _series([0.0] * 40)

    monkeypatch.setattr(zscore, "fetch_series", fake_fetch)
    seen: set[tuple[str, float]] = set()

    first = await run_once("http://prom:9090", seen=seen)
    assert [(e.metric, e.first_ts) for e in first] == [("queue_depth", step_points[30][0])]
    assert seen == {("queue_depth", step_points[30][0])}

    second = await run_once("http://prom:9090", seen=seen)
    assert second == []
    assert len(seen) == 1


@pytest.mark.asyncio
async def test_run_once_without_seen_returns_every_event(monkeypatch: pytest.MonkeyPatch) -> None:
    step_points = _series(_noisy(10.0, 30) + _noisy(100.0, 10))

    async def fake_fetch(client, base_url, promql, start, end, step):  # type: ignore[no-untyped-def]
        if promql == AD1_QUERIES["queue_depth"]:
            return step_points
        return _series([0.0] * 40)

    monkeypatch.setattr(zscore, "fetch_series", fake_fetch)
    assert len(await run_once("http://prom:9090")) == 1
    assert len(await run_once("http://prom:9090")) == 1


def test_ad1_queries_cover_all_six_metrics() -> None:
    assert set(AD1_QUERIES) == {
        "queue_depth",
        "queue_pending",
        "queue_dead_lettered",
        "solve_p99",
        "dependency_errors",
        "drivers_indexed",
    }


def test_prometheus_url_from_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("PROMETHEUS_URL", raising=False)
    assert prometheus_url() == "http://localhost:9090"
    monkeypatch.setenv("PROMETHEUS_URL", "http://prom.example:9090/")
    assert prometheus_url() == "http://prom.example:9090"


@pytest.mark.asyncio
async def test_fetch_series_parses_matrix() -> None:
    seen: dict[str, object] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["params"] = dict(request.url.params)
        body = {
            "status": "success",
            "data": {
                "resultType": "matrix",
                "result": [
                    {"metric": {"tenant": "a"}, "values": [[100, "1"], [115, "NaN"]]},
                    {"metric": {"tenant": "b"}, "values": [[100, "2.5"], [115, "4"]]},
                ],
            },
        }
        return httpx.Response(200, json=body)

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        points = await fetch_series(
            client, "http://prom:9090", AD1_QUERIES["queue_depth"], 90.0, 120.0, 15
        )

    assert seen["path"] == "/api/v1/query_range"
    assert seen["params"] == {
        "query": "max by (tenant) (ridematch_queue_depth)",
        "start": "90.0",
        "end": "120.0",
        "step": "15",
    }
    assert points == [(100.0, 2.5), (115.0, 4.0)]


@pytest.mark.asyncio
async def test_fetch_series_raises_on_prometheus_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, text=json.dumps({"status": "error", "error": "bad query"}))

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        with pytest.raises(RuntimeError, match="bad query"):
            await fetch_series(client, "http://prom:9090", "up", 0.0, 1.0, 15)


def test_gauge_promql_uses_max_by_tenant() -> None:
    assert AD1_QUERIES["queue_depth"] == "max by (tenant) (ridematch_queue_depth)"
    assert AD1_QUERIES["queue_pending"] == "max by (tenant) (ridematch_queue_pending)"
    assert AD1_QUERIES["queue_dead_lettered"] == "max by (tenant) (ridematch_queue_dead_lettered)"
    assert AD1_QUERIES["drivers_indexed"] == "max by (tenant) (ridematch_drivers_indexed)"
    assert AD1_QUERIES["dependency_errors"].startswith("sum(rate(")


@pytest.mark.asyncio
async def test_fetch_series_takes_max_across_series() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        result = [
            {"metric": {"pod": "a"}, "values": [[100, "7"], [115, "3"]]},
            {"metric": {"pod": "b"}, "values": [[100, "7"], [115, "5"]]},
            {"metric": {"pod": "c"}, "values": [[100, "7"]]},
        ]
        return httpx.Response(200, json={"status": "success", "data": {"result": result}})

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        points = await fetch_series(client, "http://prom:9090", "q", 0.0, 1.0, 15)
    assert points == [(100.0, 7.0), (115.0, 5.0)]


def test_slow_solver_step_in_seconds_fires() -> None:
    values = _noisy(0.05, 30, amp=0.002) + [2.0] * 10
    events = detect({"solve_p99": _series(values)})
    assert [e.metric for e in events] == ["solve_p99"]


def test_redis_error_rate_step_fires() -> None:
    values = [0.0] * 30 + [2.0] * 10
    events = detect({"dependency_errors": _series(values)})
    assert [e.metric for e in events] == ["dependency_errors"]


@pytest.mark.asyncio
async def test_run_once_emits_absent_event_for_empty_series(monkeypatch) -> None:
    async def fake_fetch(client, base_url, promql, start, end, step):
        if promql == AD1_QUERIES["drivers_indexed"]:
            return []
        return _series([10.0] * 40)

    monkeypatch.setattr(zscore, "fetch_series", fake_fetch)
    seen: set = set()
    events = await run_once("http://prom:9090", seen=seen)
    assert [(e.metric, e.reason) for e in events] == [("drivers_indexed", "absent")]
    assert await run_once("http://prom:9090", seen=seen) == []  # reported once
