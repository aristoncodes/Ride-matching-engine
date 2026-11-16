"""Rolling z-score detector over the AD-1 metrics (PRD AD-1, AD-3).

`detect` is a pure function over (ts, value) points so it can be tested without
a network. `fetch_series` and `run_once` are the thin Prometheus HTTP wrappers.
"""

from __future__ import annotations

import asyncio
import math
import os
import time
from collections import deque
from dataclasses import asdict, dataclass
from statistics import fmean, pstdev

import httpx

# Per-pod gauges carry only a tenant label, so every pod exports the same value: max, not sum.
PROMQL_QUEUE_DEPTH = "max by (tenant) (ridematch_queue_depth)"
PROMQL_QUEUE_PENDING = "max by (tenant) (ridematch_queue_pending)"
PROMQL_QUEUE_DEAD_LETTERED = "max by (tenant) (ridematch_queue_dead_lettered)"
PROMQL_SOLVE_P99 = (
    "histogram_quantile(0.99, sum by (le) (rate(ridematch_solve_seconds_bucket[5m])))"
)
PROMQL_DEPENDENCY_ERRORS = "sum(rate(ridematch_dependency_errors_total[1m]))"
PROMQL_DRIVERS_INDEXED = "max by (tenant) (ridematch_drivers_indexed)"

AD1_QUERIES: dict[str, str] = {
    "queue_depth": PROMQL_QUEUE_DEPTH,
    "queue_pending": PROMQL_QUEUE_PENDING,
    "queue_dead_lettered": PROMQL_QUEUE_DEAD_LETTERED,
    "solve_p99": PROMQL_SOLVE_P99,
    "dependency_errors": PROMQL_DEPENDENCY_ERRORS,
    "drivers_indexed": PROMQL_DRIVERS_INDEXED,
}

DEFAULT_PROMETHEUS_URL = "http://localhost:9090"
DEFAULT_STEP_SECONDS = 15
ABSENT_KEY_TS = -1.0  # `seen` key timestamp for a metric with no points
STD_FLOOR_REL = 0.05  # std is at least 5% of |mean| so flat series do not explode
STD_FLOOR_ABS = 1.0  # default absolute floor, for count-like metrics
# Per-metric absolute floors: counts use 1.0, seconds and rates use 0.01.
STD_FLOOR_ABS_BY_METRIC: dict[str, float] = {
    "queue_depth": 1.0,
    "queue_pending": 1.0,
    "queue_dead_lettered": 1.0,
    "drivers_indexed": 1.0,
    "solve_p99": 0.01,
    "dependency_errors": 0.01,
}


@dataclass(frozen=True)
class DetectionEvent:
    metric: str
    first_ts: float
    last_ts: float
    peak_z: float
    value_at_detection: float
    reason: str = "zscore"


def detect(
    series: dict[str, list[tuple[float, float]]],
    window: int = 30,
    z_threshold: float = 3.0,
    sustain: int = 3,
) -> list[DetectionEvent]:
    """Flag sustained deviations in each series.

    A point is scored against the previous `window` points (mean and population
    std, with std_eff = max(std, 0.05 * |mean|, per-metric floor)). It is deviant when
    |z| > z_threshold. An event fires once per run of deviant points, at the point
    where the run reaches `sustain`. Deviant points are not added to the baseline,
    so a sustained anomaly cannot pull the window toward itself. Points before the
    window is full are not scored.
    """
    if window < 2:
        raise ValueError("window must be >= 2")
    if sustain < 1:
        raise ValueError("sustain must be >= 1")
    events: list[DetectionEvent] = []
    for metric in sorted(series):
        events.extend(_detect_one(metric, series[metric], window, z_threshold, sustain))
    return events


def _detect_one(
    metric: str,
    points: list[tuple[float, float]],
    window: int,
    z_threshold: float,
    sustain: int,
) -> list[DetectionEvent]:
    floor_abs = STD_FLOOR_ABS_BY_METRIC.get(metric, STD_FLOOR_ABS)
    history: deque[float] = deque(maxlen=window)
    events: list[DetectionEvent] = []
    run_len = 0
    run_first_ts = 0.0
    run_peak = 0.0
    for ts, value in sorted(points, key=lambda p: p[0]):
        if len(history) == window:
            mean = fmean(history)
            std = max(pstdev(history), STD_FLOOR_REL * abs(mean), floor_abs)
            z = (value - mean) / std
            if abs(z) > z_threshold:
                if run_len == 0:
                    run_first_ts = ts
                    run_peak = 0.0
                run_len += 1
                run_peak = max(run_peak, abs(z))
                if run_len == sustain:
                    events.append(
                        DetectionEvent(metric, run_first_ts, ts, run_peak, value)
                    )
                # Deviant points stay out of the baseline while the run lasts.
                continue
            run_len = 0
        history.append(value)
    return events


def prometheus_url() -> str:
    return os.environ.get("PROMETHEUS_URL", DEFAULT_PROMETHEUS_URL).rstrip("/")


async def fetch_series(
    client: httpx.AsyncClient,
    base_url: str,
    promql: str,
    start: float,
    end: float,
    step: int,
) -> list[tuple[float, float]]:
    """Run a Prometheus range query; take the max of all returned series per timestamp.

    NaN and infinite samples are dropped.
    """
    resp = await client.get(
        f"{base_url.rstrip('/')}/api/v1/query_range",
        params={"query": promql, "start": start, "end": end, "step": step},
    )
    resp.raise_for_status()
    body = resp.json()
    if body.get("status") != "success":
        raise RuntimeError(f"prometheus query failed: {body.get('error', 'unknown error')}")
    totals: dict[float, float] = {}
    for item in body["data"]["result"]:
        for raw_ts, raw_value in item["values"]:
            value = float(raw_value)
            if math.isfinite(value):
                ts = float(raw_ts)
                totals[ts] = max(totals[ts], value) if ts in totals else value
    return sorted(totals.items())


async def run_once(
    base_url: str,
    minutes: int = 15,
    seen: set[tuple[str, float]] | None = None,
) -> list[DetectionEvent]:
    """Query every AD-1 metric over the last `minutes` and run detect.

    Each poll rescans the whole window, so the same run is detected again. If
    `seen` is given, events whose (metric, first_ts) key is already in it are
    skipped, and new keys are added to it. Callers keep one set across polls.
    """
    end = time.time()
    start = end - minutes * 60
    async with httpx.AsyncClient(timeout=10.0) as client:
        results = await asyncio.gather(
            *(
                fetch_series(client, base_url, promql, start, end, DEFAULT_STEP_SECONDS)
                for promql in AD1_QUERIES.values()
            )
        )
    by_metric = dict(zip(AD1_QUERIES, results, strict=True))
    events = detect({m: pts for m, pts in by_metric.items() if pts})
    # No points at all (for example ingestd scaled to 0) is a detection, not silence.
    for metric, pts in by_metric.items():
        if not pts:
            events.append(DetectionEvent(metric, end, end, 0.0, 0.0, "absent"))
    if seen is None:
        return events
    fresh = []
    for e in events:
        # An absent metric has a stable key so it is reported once until it comes back.
        key = (e.metric, ABSENT_KEY_TS) if e.reason == "absent" else (e.metric, e.first_ts)
        if key not in seen:
            seen.add(key)
            fresh.append(e)
    for metric, pts in by_metric.items():
        if pts:
            seen.discard((metric, ABSENT_KEY_TS))
    return fresh


def to_diagnose_payload(event: DetectionEvent) -> dict[str, object]:
    """Request body for POST /diagnose (AD-3) with source="detector".

    NOTE: the API ticket must add a `detection` field to DiagnoseRequest. Until it
    does, this payload will not validate against the planned schema.
    """
    return {
        "source": "detector",
        "alert": None,
        "question": None,
        "detection": asdict(event),
    }
