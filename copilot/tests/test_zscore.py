import json
import math

import httpx
import pytest

from copilot.detect import (
    AD1_QUERIES,
    DetectionEvent,
    detect,
    fetch_series,
    prometheus_url,
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
    # Constant history has std 0; the 1e-9 floor keeps z finite.
    values = [5.0] * 30 + [6.0] * 3
    events = detect({"m": _series(values)})
    assert len(events) == 1
    assert math.isfinite(events[0].peak_z)


def test_constant_series_is_silent() -> None:
    assert detect({"a": _series([0.0] * 60), "b": _series([7.0] * 60)}) == []


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
        "metric": "queue_depth",
        "first_ts": 1.0,
        "last_ts": 2.0,
        "peak_z": 4.5,
        "value_at_detection": 99.0,
    }


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
async def test_fetch_series_parses_and_sums_matrix() -> None:
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
        "query": "ridematch_queue_depth",
        "start": "90.0",
        "end": "120.0",
        "step": "15",
    }
    assert points == [(100.0, 3.5), (115.0, 4.0)]


@pytest.mark.asyncio
async def test_fetch_series_raises_on_prometheus_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, text=json.dumps({"status": "error", "error": "bad query"}))

    async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as client:
        with pytest.raises(RuntimeError, match="bad query"):
            await fetch_series(client, "http://prom:9090", "up", 0.0, 1.0, 15)
