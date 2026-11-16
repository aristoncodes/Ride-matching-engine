"""DP-1: load slog JSON log lines into the `logs` table.

Input: one JSON object per line, as emitted by slog's JSON handler (keys: time, level,
msg, optional service, request_id, error; any other key goes to `fields`).

Idempotency trade-off: the schema is fixed (no content hash or source column), so a
re-run cannot be matched line by line. Instead, one load deletes the rows already stored
for each (service, ts range) covered by the file, then inserts the file's rows, all in one
transaction. Re-running the same file is therefore safe. The cost: rows from a different
source that share a service and time window are deleted too. Load one source per window.
"""

from __future__ import annotations

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

_MAPPED_KEYS = frozenset({"time", "level", "msg", "service", "request_id", "error"})
_COLUMNS = ("ts", "service", "level", "request_id", "event", "error", "fields")
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

    fields = {k: v for k, v in obj.items() if k not in _MAPPED_KEYS}
    return LogRow(
        ts=ts,
        service=service,
        level=level,
        request_id=_text(obj.get("request_id")),
        event=_text(obj.get("msg")),
        error=_text(obj.get("error")),
        fields=json.dumps(fields, separators=(",", ":"), sort_keys=True),
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


async def load_logs(
    pool: asyncpg.Pool,
    source: Path | str,
    *,
    from_stdin: bool = False,
    service: str | None = None,
) -> int:
    """Load JSON log lines into `logs`. Returns the number of rows inserted.

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

    # Per-service time window covered by this file (see module docstring).
    windows: dict[str, tuple[datetime, datetime]] = {}
    for row in rows:
        lo, hi = windows.get(row.service, (row.ts, row.ts))
        windows[row.service] = (min(lo, row.ts), max(hi, row.ts))

    records = [(r.ts, r.service, r.level, r.request_id, r.event, r.error, r.fields) for r in rows]
    async with pool.acquire() as conn, conn.transaction():
        for svc, (lo, hi) in windows.items():
            await conn.execute(
                "DELETE FROM logs WHERE service = $1 AND ts BETWEEN $2 AND $3", svc, lo, hi
            )
        # One COPY for the whole batch: no per-row round trips.
        await conn.copy_records_to_table("logs", records=records, columns=_COLUMNS)
    return len(rows)
