"""Metrics tool (AG-2): fixed PromQL by enum name. The model never supplies PromQL."""

from __future__ import annotations

import time
from typing import Literal, get_args

import httpx

MetricQuery = Literal[
    "queue_depth",
    "queue_pending",
    "dead_lettered",
    "solve_p99",
    "match_latency_p99",
    "match_rate",
    "http_5xx_ratio",
    "engine_dependency_errors",
    "drivers_indexed",
    "batch_size_mean",
    "solve_seconds_per_rider",
]

METRIC_QUERIES: dict[str, str] = {
    "queue_depth": "max by (tenant) (ridematch_queue_depth)",
    "queue_pending": "max by (tenant) (ridematch_queue_pending)",
    "dead_lettered": "max by (tenant) (ridematch_queue_dead_lettered)",
    "solve_p99": (
        "histogram_quantile(0.99, sum by (le) (rate(ridematch_solve_seconds_bucket[5m])))"
    ),
    "match_latency_p99": (
        "histogram_quantile(0.99, sum by (le) (rate(ridematch_match_latency_seconds_bucket[5m])))"
    ),
    "match_rate": 'sum(rate(ridematch_riders_total{outcome="matched"}[1m]))',
    "http_5xx_ratio": (
        'sum(rate(ridematch_http_requests_total{status_class="5xx"}[5m])) '
        "/ clamp_min(sum(rate(ridematch_http_requests_total[5m])), 1e-9)"
    ),
    "engine_dependency_errors": (
        'sum(rate(ridematch_dependency_errors_total{dependency="engine"}[1m]))'
    ),
    "drivers_indexed": "max by (tenant) (ridematch_drivers_indexed)",
    "batch_size_mean": (
        "sum(rate(ridematch_batch_size_riders_sum[5m])) "
        "/ clamp_min(sum(rate(ridematch_batch_size_riders_count[5m])), 1e-9)"
    ),
    "solve_seconds_per_rider": (
        "sum(rate(ridematch_solve_seconds_sum[5m])) "
        "/ clamp_min(sum(rate(ridematch_batch_size_riders_sum[5m])), 1e-9)"
    ),
}

STEP_SECONDS = 15


async def query_metric(
    client: httpx.AsyncClient,
    base_url: str,
    name: MetricQuery,
    window_minutes: int = 15,
) -> list[dict]:
    """Run the fixed query for `name` over the last `window_minutes`.

    Returns [{"ts": float, "value": float, "labels": dict}].
    """
    if name not in get_args(MetricQuery) or name not in METRIC_QUERIES:
        raise ValueError(f"unknown metric query: {name!r}")
    if (
        isinstance(window_minutes, bool)
        or not isinstance(window_minutes, int)
        or not 1 <= window_minutes <= 120
    ):
        raise ValueError("window_minutes must be an integer in 1..120")

    end = time.time()
    start = end - window_minutes * 60
    resp = await client.get(
        f"{base_url.rstrip('/')}/api/v1/query_range",
        params={
            "query": METRIC_QUERIES[name],
            "start": start,
            "end": end,
            "step": STEP_SECONDS,
        },
    )
    resp.raise_for_status()
    body = resp.json()
    if body.get("status") != "success":
        raise RuntimeError(f"prometheus query failed: {body.get('error', 'unknown error')}")

    out: list[dict] = []
    for series in body["data"]["result"]:
        labels = dict(series.get("metric", {}))
        for ts, raw in series.get("values", []):
            out.append({"ts": float(ts), "value": float(raw), "labels": labels})
    return out
