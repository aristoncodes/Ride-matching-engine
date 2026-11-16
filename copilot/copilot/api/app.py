"""FastAPI app (ticket 2.4): /diagnose, /alerts, /incidents, /actions.

Logging: route, status code and latency only. Never request bodies, alert annotations,
prompts, or the token.

Proposing an action never runs anything. Only POST /actions/{id}/approve does, via decide().
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import json
import logging
import os
import time
import uuid
from collections import deque
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Literal

from fastapi import Depends, FastAPI, HTTPException, Request
from pydantic import BaseModel, Field, model_validator

from copilot.actions import k8s
from copilot.actions.k8s import ActionError
from copilot.agents.coordinator import investigate
from copilot.agents.tools.runbook_tools import search_runbook
from copilot.models import Diagnosis

log = logging.getLogger("copilot.api")

DIAGNOSE_TIMEOUT_S = 60.0
RATE_LIMIT = 30
RATE_WINDOW_S = 60.0
MISSING_TOKEN_MSG = "COPILOT_API_TOKEN is required to start the copilot API; set it and retry."


@dataclass
class AppDeps:
    pool_app: Any  # copilot_app pool: writes incidents_seen and actions
    pool_ro: Any  # copilot_ro pool: agent reads
    prom_client: Any
    prom_base_url: str
    embedder: Any
    guarded_model: Any
    k8s_api: Any
    api_token: str
    clock: Callable[[], float] = time.monotonic


class DiagnoseRequest(BaseModel):
    source: Literal["alert", "detector", "question"]
    alert: dict | None = None
    question: str | None = None
    detection: dict | None = None

    @model_validator(mode="after")
    def _exactly_matching_field(self) -> DiagnoseRequest:
        field = {"alert": "alert", "detector": "detection", "question": "question"}[self.source]
        given = {n for n in ("alert", "question", "detection") if getattr(self, n) is not None}
        if given != {field}:
            raise ValueError(f"source {self.source!r} requires exactly the {field!r} field")
        return self


class DiagnoseResponse(BaseModel):
    incident_id: str
    status: Literal["running", "diagnosed", "needs_human", "failed"]
    diagnosis: Diagnosis | None = None
    fallback: dict | None = None
    action_id: str | None = None


class ActionDecision(BaseModel):
    decided_by: str = Field(min_length=1)
    note: str | None = None


class ActionResponse(BaseModel):
    action_id: str
    status: Literal["proposed", "approved", "rejected", "executed", "failed"]
    result: str | None = None


def _trigger(req: DiagnoseRequest) -> dict:
    if req.source == "alert":
        return req.alert or {}
    if req.source == "detector":
        return req.detection or {}
    return {"question": req.question}


def _js(v: Any) -> str:
    return json.dumps(v, default=str)


def _load(v: Any) -> Any:
    return json.loads(v) if isinstance(v, str) else v


async def _fallback(deps: AppDeps, evidence: list, trigger: dict, reason: str) -> dict:
    """Raw evidence plus the top 5 runbook matches; the model is not needed for this."""
    query = json.dumps(trigger, default=str)[:500]
    try:
        runbook = await search_runbook(deps.pool_ro, deps.embedder, query, 5)
    except Exception as exc:  # noqa: BLE001 - fallback must never fail the request
        log.warning("fallback runbook search failed: %s", type(exc).__name__)
        runbook = []
    return {
        "reason": reason,
        "evidence": [e.model_dump() for e in evidence],
        "runbook_matches": runbook,
    }


async def _create_incident(deps: AppDeps, req: DiagnoseRequest) -> str:
    incident_id = str(uuid.uuid4())
    await deps.pool_app.execute(
        "INSERT INTO incidents_seen (id, opened_at, source, trigger, status) "
        "VALUES ($1, now(), $2, $3::jsonb, 'running')",
        uuid.UUID(incident_id),
        req.source,
        _js(req.model_dump(exclude_none=True)),
    )
    return incident_id


async def _run(deps: AppDeps, incident_id: str, req: DiagnoseRequest) -> DiagnoseResponse:
    """The /diagnose flow for an already-created incident row."""
    t0 = time.monotonic()
    trigger = _trigger(req)
    diagnosis: Diagnosis | None = None
    fallback: dict | None = None
    action_id: str | None = None
    tokens_in = tokens_out = 0
    try:
        result = await asyncio.wait_for(
            investigate(
                trigger,
                model=deps.guarded_model,
                pool_ro=deps.pool_ro,
                prom_client=deps.prom_client,
                prom_base_url=deps.prom_base_url,
                embedder=deps.embedder,
            ),
            timeout=DIAGNOSE_TIMEOUT_S,
        )
        diagnosis = result.diagnosis
        tokens_in, tokens_out = result.tokens_in, result.tokens_out
        if diagnosis is None:
            fallback = await _fallback(deps, result.evidence, trigger, "model unavailable")
    except Exception as exc:  # noqa: BLE001 - never 500 because of the model or a slow tool
        log.warning("diagnose failed: %s", type(exc).__name__)
        reason = "timeout" if isinstance(exc, TimeoutError) else type(exc).__name__
        fallback = await _fallback(deps, [], trigger, reason)

    if diagnosis is None:
        status = "failed"
    else:
        status = "needs_human" if diagnosis.needs_human else "diagnosed"
        act = diagnosis.proposed_action
        if act.kind != "none" and not diagnosis.needs_human:
            try:
                action_id = await k8s.propose(deps.pool_app, incident_id, act, act.reason)
            except ActionError:
                log.warning("proposed action refused by validation")

    await deps.pool_app.execute(
        "UPDATE incidents_seen SET status=$2, diagnosis=$3::jsonb, fallback=$4::jsonb, "
        "tokens_in=$5, tokens_out=$6, latency_ms=$7 WHERE id=$1",
        uuid.UUID(incident_id),
        status,
        _js(diagnosis.model_dump()) if diagnosis else None,
        _js(fallback) if fallback is not None else None,
        tokens_in,
        tokens_out,
        int((time.monotonic() - t0) * 1000),
    )
    return DiagnoseResponse(
        incident_id=incident_id,
        status=status,
        diagnosis=diagnosis,
        fallback=fallback,
        action_id=action_id,
    )


class _RateLimiter:
    def __init__(self, clock: Callable[[], float]) -> None:
        self._clock = clock
        self._hits: dict[str, deque[float]] = {}

    def check(self, key: str) -> float | None:
        """Record a hit. Return seconds to wait if over the limit, else None."""
        now = self._clock()
        q = self._hits.setdefault(key, deque())
        while q and now - q[0] >= RATE_WINDOW_S:
            q.popleft()
        if len(q) >= RATE_LIMIT:
            return max(RATE_WINDOW_S - (now - q[0]), 1.0)
        q.append(now)
        return None


def create_app(
    deps: AppDeps, on_startup: Callable[[AppDeps], Awaitable[None]] | None = None
) -> FastAPI:
    from contextlib import asynccontextmanager

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        if on_startup is not None:
            await on_startup(deps)
        yield

    app = FastAPI(title="ride-matching copilot", lifespan=lifespan)
    app.state.tasks = set()
    limiter = _RateLimiter(deps.clock)

    @app.middleware("http")
    async def access_log(request: Request, call_next):
        t0 = time.monotonic()
        status = 500
        try:
            response = await call_next(request)
            status = response.status_code
            return response
        finally:
            route = request.scope.get("route")
            path = getattr(route, "path", "unmatched")
            log.info(
                "route=%s status=%d latency_ms=%d", path, status, (time.monotonic() - t0) * 1000
            )

    async def auth(request: Request) -> None:
        header = request.headers.get("authorization", "")
        scheme, _, token = header.partition(" ")
        ok = scheme.lower() == "bearer" and hmac.compare_digest(
            token.encode(), deps.api_token.encode()
        )
        if not ok:
            raise HTTPException(401, "unauthorized", headers={"WWW-Authenticate": "Bearer"})
        key = hashlib.sha256(token.encode()).hexdigest()
        wait = limiter.check(key)
        if wait is not None:
            raise HTTPException(
                429, "rate limit exceeded", headers={"Retry-After": str(int(wait + 0.999))}
            )

    guard = [Depends(auth)]

    @app.get("/healthz")
    async def healthz() -> dict:
        return {"status": "ok"}

    @app.post("/diagnose", response_model=DiagnoseResponse, dependencies=guard)
    async def diagnose(req: DiagnoseRequest) -> DiagnoseResponse:
        incident_id = await _create_incident(deps, req)
        return await _run(deps, incident_id, req)

    @app.post("/alerts", status_code=202, dependencies=guard)
    async def alerts(body: dict) -> dict:
        ids: list[str] = []
        for alert in body.get("alerts") or []:
            if not isinstance(alert, dict) or alert.get("status") != "firing":
                continue
            req = DiagnoseRequest(source="alert", alert=alert)
            incident_id = await _create_incident(deps, req)
            ids.append(incident_id)
            log.info("alert accepted: alertname=%s", (alert.get("labels") or {}).get("alertname"))
            task = asyncio.create_task(_safe_run(deps, incident_id, req))
            app.state.tasks.add(task)
            task.add_done_callback(app.state.tasks.discard)
        return {"incident_ids": ids}

    @app.get("/incidents/{incident_id}", dependencies=guard)
    async def get_incident(incident_id: str) -> dict:
        try:
            iid = uuid.UUID(incident_id)
        except ValueError:
            raise HTTPException(404, "incident not found") from None
        row = await deps.pool_app.fetchrow("SELECT * FROM incidents_seen WHERE id = $1", iid)
        if row is None:
            raise HTTPException(404, "incident not found")
        actions = await deps.pool_app.fetch(
            "SELECT * FROM actions WHERE incident_id = $1 ORDER BY proposed_at", iid
        )
        out = {k: _plain(v) for k, v in dict(row).items()}
        for k in ("trigger", "diagnosis", "fallback"):
            out[k] = _load(row[k])
        out["actions"] = [{k: _plain(v) for k, v in dict(a).items()} for a in actions]
        return out

    async def _decide(action_id: str, body: ActionDecision, approve: bool) -> ActionResponse:
        try:
            uuid.UUID(action_id)
        except ValueError:
            raise HTTPException(404, "action not found") from None
        try:
            status = await k8s.decide(
                deps.pool_app, action_id, approve, body.decided_by, deps.k8s_api
            )
        except ActionError as exc:
            msg = str(exc)
            if "not found" in msg:
                raise HTTPException(404, msg) from None
            bad_state = "only 'proposed'" in msg or "rate limited" in msg
            raise HTTPException(409 if bad_state else 422, msg) from None
        row = await deps.pool_app.fetchrow(
            "SELECT result FROM actions WHERE id = $1", uuid.UUID(action_id)
        )
        return ActionResponse(
            action_id=action_id, status=status, result=row["result"] if row else None
        )

    @app.post("/actions/{action_id}/approve", response_model=ActionResponse, dependencies=guard)
    async def approve(action_id: str, body: ActionDecision) -> ActionResponse:
        return await _decide(action_id, body, True)

    @app.post("/actions/{action_id}/reject", response_model=ActionResponse, dependencies=guard)
    async def reject(action_id: str, body: ActionDecision) -> ActionResponse:
        return await _decide(action_id, body, False)

    async def _safe_run(d: AppDeps, incident_id: str, req: DiagnoseRequest) -> None:
        try:
            await _run(d, incident_id, req)
        except Exception as exc:  # noqa: BLE001 - background task: record, never crash the loop
            log.error("background diagnose failed: %s", type(exc).__name__)
            try:
                await d.pool_app.execute(
                    "UPDATE incidents_seen SET status='failed' WHERE id=$1",
                    uuid.UUID(incident_id),
                )
            except Exception:  # noqa: BLE001
                pass

    return app


def _plain(v: Any) -> Any:
    if isinstance(v, uuid.UUID):
        return str(v)
    if hasattr(v, "isoformat"):
        return v.isoformat()
    if hasattr(v, "as_tuple"):  # Decimal
        return float(v)
    return v


def build_default_app() -> FastAPI:
    """Wire real clients from the environment. Connections open at startup, not import."""
    token = os.environ.get("COPILOT_API_TOKEN", "")
    if not token:
        raise SystemExit(MISSING_TOKEN_MSG)
    deps = AppDeps(
        pool_app=None,
        pool_ro=None,
        prom_client=None,
        prom_base_url=os.environ.get("PROMETHEUS_URL", "http://prometheus:9090"),
        embedder=None,
        guarded_model=None,
        k8s_api=None,
        api_token=token,
    )

    async def startup(d: AppDeps) -> None:
        import httpx

        from copilot import db
        from copilot.agents.model import GuardedModel, RedisCache
        from copilot.agents.vertex import VertexClient
        from copilot.embed import VertexEmbedder

        d.pool_app = await db.connect(os.environ["COPILOT_DB_DSN"])
        d.pool_ro = await db.connect(os.environ["COPILOT_RO_DSN"])
        d.prom_client = httpx.AsyncClient(timeout=10.0)
        d.embedder = VertexEmbedder()
        d.guarded_model = GuardedModel(VertexClient(), cache=RedisCache())
        d.k8s_api = await asyncio.to_thread(k8s.KubernetesActions)

    return create_app(deps, startup)


if os.environ.get("COPILOT_API_TOKEN"):
    app = build_default_app()
else:
    raise SystemExit(MISSING_TOKEN_MSG)
