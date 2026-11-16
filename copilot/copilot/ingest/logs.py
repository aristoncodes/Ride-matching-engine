"""DP-1: load slog JSON log lines into the `logs` table.

Input: one JSON object per line, as emitted by slog's JSON handler (keys: time, level,
msg, optional service, request_id, error or err; any other key goes to `fields`).

Idempotency: every row stores `line_hash`, the sha256 of its raw line, which is UNIQUE.
A load inserts with ON CONFLICT (line_hash) DO NOTHING, so re-loading a file adds nothing.
This works per line, not per time window, so it is safe across pods: loading pod A's logs
never deletes pod B's rows, and two loads running at once cannot both insert the same line.
Rows are staged in a temp table first because COPY cannot express ON CONFLICT.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
import sys
from collections.abc import Iterable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import asyncpg

log = logging.getLogger(__name__)

# `err` is the key the services actually emit (slog.Error("msg", "err", err)); `error` is an
# accepted alias. If both are present, `error` wins.
_MAPPED_KEYS = frozenset({"time", "level", "msg", "service", "request_id", "error", "err"})
_COLUMNS = ("ts", "service", "level", "request_id", "event", "error", "fields", "line_hash")
_STAGE_DDL = """
CREATE TEMP TABLE logs_stage (
  ts          timestamptz,
  service     text,
  level       text,
  request_id  text,
  event       text,
  error       text,
  fields      jsonb,
  line_hash   text
) ON COMMIT DROP
"""
_INSERT_NEW = """
INSERT INTO logs (ts, service, level, request_id, event, error, fields, line_hash)
SELECT ts, service, level, request_id, event, error, fields, line_hash FROM logs_stage
ON CONFLICT (line_hash) DO NOTHING
"""
# slog writes RFC3339Nano (nanosecond fractions); fromisoformat accepts at most microseconds.
_FRACTION = re.compile(r"(\.\d{6})\d+")


@dataclass(frozen=True)
class LogRow:
    ts: datetime
    service: str
    level: str
    request_id: str | None
    event: str | None
    error: str | None
    fields: str  # JSON text for the jsonb column
    line_hash: str  # sha256 hex of the raw line


def _text(value: Any) -> str | None:
    if value is None or isinstance(value, str):
        return value
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def _parse_time(raw: str) -> datetime | None:
    try:
        ts = datetime.fromisoformat(_FRACTION.sub(r"\1", raw.replace("Z", "+00:00", 1)))
    except ValueError:
        return None
    return ts if ts.tzinfo else ts.replace(tzinfo=UTC)


def parse_line(line: str, default_service: str | None = None) -> LogRow | None:
    """Parse one JSON log line. Returns None if the line is malformed."""
    try:
        obj = json.loads(line)
    except ValueError:  # JSONDecodeError is a ValueError
        return None
    if not isinstance(obj, dict):
        return None

    raw_time, level = obj.get("time"), obj.get("level")
    if not isinstance(raw_time, str) or not isinstance(level, str) or not level:
        return None
    ts = _parse_time(raw_time)
    if ts is None:
        return None

    service = obj.get("service") or default_service
    if not isinstance(service, str) or not service:
        return None

    error = obj.get("error")
    if error is None:
        error = obj.get("err")

    fields = {k: v for k, v in obj.items() if k not in _MAPPED_KEYS}
    return LogRow(
        ts=ts,
        service=service,
        level=level,
        request_id=_text(obj.get("request_id")),
        event=_text(obj.get("msg")),
        error=_text(error),
        fields=json.dumps(fields, separators=(",", ":"), sort_keys=True),
        line_hash=hashlib.sha256(line.encode("utf-8")).hexdigest(),
    )


def parse_lines(lines: Iterable[str], service: str | None = None) -> tuple[list[LogRow], int]:
    """Parse many lines. Returns (rows, malformed_count). Blank lines are ignored silently."""
    rows: list[LogRow] = []
    skipped = 0
    for line in lines:
        if not line.strip():
            continue
        row = parse_line(line, service)
        if row is None:
            skipped += 1
        else:
            rows.append(row)
    return rows, skipped


def _inserted(status: str) -> int:
    """Row count from an asyncpg status string such as 'INSERT 0 3'."""
    return int(status.rsplit(" ", 1)[-1])


async def load_logs(
    pool: asyncpg.Pool,
    source: Path | str,
    *,
    from_stdin: bool = False,
    service: str | None = None,
) -> int:
    """Load JSON log lines into `logs`. Returns the number of rows newly inserted.

    Lines already in the table (same sha256) are skipped, so a re-run returns 0.
    `source` is a file path (e.g. a saved `kubectl logs` output). With `from_stdin=True`
    the lines are read from stdin and `source` is only used in messages.
    `service` is used for lines that have no `service` key.
    """
    if from_stdin:
        text = sys.stdin.read()
        label = "stdin"
    else:
        label = str(source)
        with open(source, encoding="utf-8", errors="replace") as fh:
            text = fh.read()

    rows, skipped = parse_lines(text.splitlines(), service)
    if skipped:
        log.warning("skipped %d malformed log line(s) from %s", skipped, label)
    if not rows:
        return 0

    records = [
        (r.ts, r.service, r.level, r.request_id, r.event, r.error, r.fields, r.line_hash)
        for r in rows
    ]
    async with pool.acquire() as conn, conn.transaction():
        await conn.execute(_STAGE_DDL)
        # One COPY for the whole batch: no per-row round trips.
        await conn.copy_records_to_table("logs_stage", records=records, columns=_COLUMNS)
        status = await conn.execute(_INSERT_NEW)
    return _inserted(status)
