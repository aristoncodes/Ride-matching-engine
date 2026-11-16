"""Tests for DP-1 log loading: pure parser tests, plus Postgres tests marked `pg`."""

from __future__ import annotations

import io
import json
import logging
import os
import uuid
from collections.abc import AsyncIterator

import asyncpg
import pytest
import pytest_asyncio

from copilot.db import apply_schema, connect
from copilot.ingest.logs import load_logs, parse_line, parse_lines


def _line(**kw: object) -> str:
    base = {"time": "2026-11-16T10:00:00.123456789Z", "level": "INFO", "msg": "tick"}
    base.update(kw)
    return json.dumps(base)


# ---- pure parser tests (always run) ----------------------------------------------


def test_parse_skips_malformed_lines_and_counts_them() -> None:
    lines = [
        _line(service="ingestd", request_id="r1"),
        "not json at all",
        "[1, 2, 3]",
        json.dumps({"time": "yesterday", "level": "INFO", "msg": "x", "service": "a"}),
        json.dumps({"time": "2026-11-16T10:00:00Z", "msg": "no level", "service": "a"}),
        json.dumps({"time": "2026-11-16T10:00:00Z", "level": "INFO", "msg": "no svc"}),
        "",  # blank: ignored, not counted
    ]
    rows, skipped = parse_lines(lines)
    assert len(rows) == 1
    assert skipped == 5
    assert rows[0].service == "ingestd"
    assert rows[0].request_id == "r1"


def test_parse_maps_slog_keys_and_puts_unknown_keys_in_fields() -> None:
    row = parse_line(
        _line(service="requestd", request_id="r9", error="boom", attempt=2, queue={"n": 1})
    )
    assert row is not None
    assert row.ts.tzinfo is not None
    assert row.ts.microsecond == 123456  # nanoseconds truncated, not rejected
    assert row.level == "INFO"
    assert row.event == "tick"
    assert row.request_id == "r9"
    assert row.error == "boom"
    assert json.loads(row.fields) == {"attempt": 2, "queue": {"n": 1}}


def test_service_falls_back_to_parameter_and_line_wins() -> None:
    no_svc = _line()
    assert parse_line(no_svc, default_service="batcherd").service == "batcherd"  # type: ignore[union-attr]
    assert parse_line(no_svc) is None  # no service anywhere: malformed
    with_svc = _line(service="ingestd")
    assert parse_line(with_svc, default_service="batcherd").service == "ingestd"  # type: ignore[union-attr]


def test_naive_timestamp_is_taken_as_utc() -> None:
    row = parse_line(_line(time="2026-11-16T10:00:00", service="ingestd"))
    assert row is not None
    assert row.ts.utcoffset().total_seconds() == 0  # type: ignore[union-attr]


# ---- Postgres tests (skip unless COPILOT_TEST_DSN is set) -------------------------


@pytest_asyncio.fixture(loop_scope="function")
async def pool() -> AsyncIterator[asyncpg.Pool]:
    dsn = os.environ.get("COPILOT_TEST_DSN")
    if not dsn:  # belt and braces; conftest already skips these tests
        pytest.skip("COPILOT_TEST_DSN not set")
    p = await connect(dsn)
    await apply_schema(p)
    try:
        yield p
    finally:
        await p.close()


async def _count(pool: asyncpg.Pool, service: str) -> int:
    return await pool.fetchval("SELECT count(*) FROM logs WHERE service = $1", service)


@pytest.mark.pg
@pytest.mark.asyncio
async def test_schema_is_idempotent(pool: asyncpg.Pool) -> None:
    await apply_schema(pool)
    await apply_schema(pool)


@pytest.mark.pg
@pytest.mark.asyncio
async def test_load_then_rerun_does_not_duplicate(pool: asyncpg.Pool, tmp_path) -> None:  # type: ignore[no-untyped-def]
    svc = f"test-{uuid.uuid4().hex[:8]}"
    path = tmp_path / "logs.jsonl"
    path.write_text(
        "\n".join(
            [
                _line(time="2026-11-16T10:00:00Z", service=svc, request_id="a"),
                _line(time="2026-11-16T10:00:01Z", service=svc, request_id="b", extra=1),
                "garbage",
            ]
        )
    )
    try:
        assert await load_logs(pool, path) == 2
        assert await _count(pool, svc) == 2
        assert await load_logs(pool, path) == 2  # same file again
        assert await _count(pool, svc) == 2  # no duplicates
        fields = await pool.fetchval(
            "SELECT fields FROM logs WHERE service = $1 AND request_id = 'b'", svc
        )
        assert json.loads(fields) == {"extra": 1}
    finally:
        await pool.execute("DELETE FROM logs WHERE service = $1", svc)


@pytest.mark.pg
@pytest.mark.asyncio
async def test_service_parameter_and_stdin(
    pool: asyncpg.Pool,
    tmp_path,
    monkeypatch: pytest.MonkeyPatch,  # type: ignore[no-untyped-def]
) -> None:
    svc = f"test-{uuid.uuid4().hex[:8]}"
    text = _line(time="2026-11-16T10:00:00Z") + "\n"  # no service key
    monkeypatch.setattr("sys.stdin", io.StringIO(text))
    try:
        assert await load_logs(pool, "stdin", from_stdin=True, service=svc) == 1
        assert await _count(pool, svc) == 1
    finally:
        await pool.execute("DELETE FROM logs WHERE service = $1", svc)


@pytest.mark.pg
@pytest.mark.asyncio
async def test_malformed_lines_logged_not_fatal(
    pool: asyncpg.Pool,
    tmp_path,
    caplog: pytest.LogCaptureFixture,  # type: ignore[no-untyped-def]
) -> None:
    svc = f"test-{uuid.uuid4().hex[:8]}"
    path = tmp_path / "bad.jsonl"
    path.write_text(_line(time="2026-11-16T10:00:00Z", service=svc) + "\n{oops\n")
    try:
        with caplog.at_level(logging.WARNING, logger="copilot.ingest.logs"):
            assert await load_logs(pool, path) == 1
        assert "skipped 1 malformed" in caplog.text
    finally:
        await pool.execute("DELETE FROM logs WHERE service = $1", svc)
