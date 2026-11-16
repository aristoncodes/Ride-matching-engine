"""Agent tool tests (ticket 2.1). No network, no database."""

from __future__ import annotations

import inspect
from typing import get_args

import httpx
import pytest

from copilot.agents.tools import logs_tools, metrics_tools, runbook_tools
from copilot.agents.tools.logs_tools import LOG_QUERIES, LogQuery, query_logs
from copilot.agents.tools.metrics_tools import (
    METRIC_QUERIES,
    MetricQuery,
    query_metric,
)
from copilot.agents.tools.runbook_tools import search_runbook
from copilot.embed import FakeEmbedder


def _client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


def test_every_metric_name_maps_to_promql():
    names = set(get_args(MetricQuery))
    assert len(names) == 11
    assert names == set(METRIC_QUERIES)
    assert all(isinstance(q, str) and "ridematch_" in q for q in METRIC_QUERIES.values())


def test_query_metric_signature_has_no_free_promql():
    params = list(inspect.signature(query_metric).parameters)
    assert params == ["client", "base_url", "name", "window_minutes"]


@pytest.mark.asyncio
async def test_unknown_metric_and_window_bounds():
    def handler(request):  # pragma: no cover - must not be reached
        raise AssertionError("no request expected")

    async with _client(handler) as c:
        with pytest.raises(ValueError):
            await query_metric(c, "http://p", "up{job='x'}")  # type: ignore[arg-type]
        for bad in (0, 121, -1):
            with pytest.raises(ValueError):
                await query_metric(c, "http://p", "queue_depth", bad)


@pytest.mark.asyncio
async def test_query_metric_params_and_parsing():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        seen["params"] = dict(request.url.params)
        return httpx.Response(
            200,
            json={
                "status": "success",
                "data": {
                    "resultType": "matrix",
                    "result": [
                        {"metric": {"tenant": "a"}, "values": [[1000.5, "3"], [1015.5, "4.5"]]}
                    ],
                },
            },
        )

    async with _client(handler) as c:
        out = await query_metric(c, "http://prom:9090/", "queue_depth", 10)
    assert seen["path"] == "/api/v1/query_range"
    assert seen["params"]["query"] == METRIC_QUERIES["queue_depth"]
    assert seen["params"]["step"] == "15"
    assert float(seen["params"]["end"]) - float(seen["params"]["start"]) == pytest.approx(600)
    assert out == [
        {"ts": 1000.5, "value": 3.0, "labels": {"tenant": "a"}},
        {"ts": 1015.5, "value": 4.5, "labels": {"tenant": "a"}},
    ]


class FakePool:
    def __init__(self, rows=None):
        self.rows = rows or []
        self.calls: list[tuple[str, tuple]] = []

    async def fetch(self, sql, *args):
        self.calls.append((sql, args))
        return self.rows


@pytest.mark.asyncio
async def test_logs_rejections():
    pool = FakePool()
    bad = [
        ("drop_table", {}),
        ("trace_request", {"request_id": "r1", "extra": 1}),
        ("trace_request", {}),
        ("trace_request", {"request_id": 5}),
        ("recent_events_by_service", {"service": "redis", "minutes": 5}),
        ("recent_events_by_service", {"service": "ingestd", "minutes": 0}),
        ("recent_events_by_service", {"service": "ingestd", "minutes": 121}),
        ("recent_events_by_service", {"service": "ingestd", "minutes": "5"}),
        ("errors_by_service", {"minutes": 500}),
    ]
    for name, params in bad:
        with pytest.raises(ValueError):
            await query_logs(pool, name, params)  # type: ignore[arg-type]
    assert pool.calls == []


def test_log_sql_is_constant_and_parameterised():
    assert set(LOG_QUERIES) == set(get_args(LogQuery))
    for sql in LOG_QUERIES.values():
        assert "$1" in sql
        assert "{" not in sql and "}" not in sql
        assert "LIMIT 200" in sql
        assert sql.startswith("SELECT id, ts,")  # logs primary key, used as the evidence ref
    assert logs_tools.ROW_LIMIT == 200


@pytest.mark.asyncio
async def test_logs_happy_path_passes_params_positionally():
    pool = FakePool([{"id": 9, "ts": 1, "service": "ingestd"}])
    out = await query_logs(pool, "recent_events_by_service", {"service": "ingestd", "minutes": 7})
    assert out == [{"id": 9, "ts": 1, "service": "ingestd"}]
    assert out[0]["id"] == 9
    sql, args = pool.calls[0]
    assert sql == LOG_QUERIES["recent_events_by_service"]
    assert args == ("ingestd", 7)


@pytest.mark.asyncio
async def test_runbook_k_bounds():
    pool = FakePool()
    for bad in (0, 11, -3):
        with pytest.raises(ValueError):
            await search_runbook(pool, FakeEmbedder(), "queue stuck", bad)
    assert pool.calls == []


@pytest.mark.asyncio
async def test_runbook_search_sql_and_output():
    rows = [
        {
            "source_file": "docs/Runbook.md",
            "anchor": "a",
            "heading": "H",
            "content": "c",
            "score": 0.75,
        }
    ]
    pool = FakePool(rows)
    out = await search_runbook(pool, FakeEmbedder(), "queue stuck", 3)
    sql, args = pool.calls[0]
    assert "<=>" in sql
    assert "'docs/%'" in sql
    assert "copilot/" in sql and "NOT LIKE" in sql
    assert "{" not in sql
    assert args[1] == 3
    assert args[0].startswith("[") and len(args[0].strip("[]").split(",")) == 384
    assert out == [
        {
            "source_file": "docs/Runbook.md",
            "anchor": "a",
            "heading": "H",
            "content": "c",
            "score": 0.75,
        }
    ]
    assert runbook_tools.SEARCH_SQL == sql
    assert metrics_tools.STEP_SECONDS == 15
