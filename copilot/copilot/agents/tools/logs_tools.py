"""Logs tool (AG-3): named, parameterised SQL only. Pass the copilot_ro pool."""

from __future__ import annotations

from typing import Any, Literal, get_args

LogQuery = Literal["errors_by_service", "trace_request", "recent_events_by_service"]

SERVICES = frozenset({"ingestd", "requestd", "batcherd"})
ROW_LIMIT = 200

LOG_QUERIES: dict[str, str] = {
    "errors_by_service": (
        "SELECT id, ts, service, level, request_id, event, error FROM logs "
        # slog writes levels in uppercase (ERROR), so compare case-insensitively.
        "WHERE upper(level) IN ('ERROR', 'FATAL') AND ts >= now() - make_interval(mins => $1) "
        "ORDER BY ts DESC LIMIT 200"
    ),
    "trace_request": (
        "SELECT id, ts, service, level, request_id, event, error FROM logs "
        "WHERE request_id = $1 ORDER BY ts ASC LIMIT 200"
    ),
    "recent_events_by_service": (
        "SELECT id, ts, service, level, request_id, event, error FROM logs "
        "WHERE service = $1 AND ts >= now() - make_interval(mins => $2) "
        "ORDER BY ts DESC LIMIT 200"
    ),
}

# name -> ordered list of (param key, validator kind)
_PARAM_SPECS: dict[str, list[tuple[str, str]]] = {
    "errors_by_service": [("minutes", "minutes")],
    "trace_request": [("request_id", "request_id")],
    "recent_events_by_service": [("service", "service"), ("minutes", "minutes")],
}


def _validate(kind: str, key: str, value: Any) -> Any:
    if kind == "minutes":
        if isinstance(value, bool) or not isinstance(value, int) or not 1 <= value <= 120:
            raise ValueError(f"{key} must be an integer in 1..120")
    elif kind == "service":
        if not isinstance(value, str) or value not in SERVICES:
            raise ValueError(f"{key} must be one of {sorted(SERVICES)}")
    elif kind == "request_id":
        if not isinstance(value, str) or not value or len(value) > 128:
            raise ValueError(f"{key} must be a non-empty string up to 128 chars")
    return value


def build_args(name: str, params: dict) -> list[Any]:
    """Validate params for `name` and return them in $1, $2 order."""
    if name not in get_args(LogQuery) or name not in LOG_QUERIES:
        raise ValueError(f"unknown log query: {name!r}")
    if not isinstance(params, dict):
        raise ValueError("params must be a dict")
    spec = _PARAM_SPECS[name]
    allowed = {k for k, _ in spec}
    unknown = set(params) - allowed
    if unknown:
        raise ValueError(f"unknown params for {name}: {sorted(unknown)}")
    args: list[Any] = []
    for key, kind in spec:
        if key not in params:
            raise ValueError(f"missing param for {name}: {key}")
        args.append(_validate(kind, key, params[key]))
    return args


async def query_logs(pool: Any, name: LogQuery, params: dict) -> list[dict]:
    """Run a named log query on the read-only (copilot_ro) pool. At most 200 rows."""
    args = build_args(name, params)
    rows = await pool.fetch(LOG_QUERIES[name], *args)
    return [dict(r) for r in rows][:ROW_LIMIT]
