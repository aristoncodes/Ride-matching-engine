"""FastAPI app (ticket 2.4): /diagnose, /alerts, /incidents, /actions.

Logging: route, status code and latency only. Never request bodies, alert annotations,
prompts, or the token.

Credentials (three, deliberately separate):
  COPILOT_API_TOKEN      /diagnose, /incidents, GET routes. Cannot approve anything.
  COPILOT_WEBHOOK_TOKEN  /alerts only (Alertmanager). Cannot call anything else.
  COPILOT_APPROVERS      name=token pairs. Approve/reject and verdicts only. Approval is split
                         from the other routes because approving runs a change on the cluster
                         and records who did it (AC-3). With one shared token, any holder could
                         approve as anyone; the approver name is derived from the token instead
                         of being taken from the request body.

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
from dataclasses import dataclass, field
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
MAX_CONCURRENT_DIAGNOSES = 4
MISSING_TOKEN_MSG = "COPILOT_API_TOKEN is required to start the copilot API; set it and retry."
MISSING_WEBHOOK_MSG = (
    "COPILOT_WEBHOOK_TOKEN is required to start the copilot API; set it and retry."
)
ACTION_FAILED_MSG = "action execution failed; see logs"
OPEN_STATUSES = ("running", "diagnosed", "needs_human")


def parse_approvers(raw: str) -> dict[str, str]:
    """Parse 'alice=tok1,bob=tok2' into {name: token}. Raises ValueError on a bad pair."""
    out: dict[str, str] = {}
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        name, sep, tok = part.partition("=")
        name, tok = name.strip(), tok.strip()
        if not sep or not name or not tok:
            raise ValueError("COPILOT_APPROVERS must be comma-separated name=token pairs")
        if name in out:
            raise ValueError(f"duplicate approver name {name!r}")
        out[name] = tok
    return out


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
    webhook_token: str = ""
    approvers: dict[str, str] = field(default_factory=dict)  # name -> token
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


# No ActionDecision body: the approver is identified by credential, and the actions table has
# no column for a free-text note, so the old `note` field was dropped.


class VerdictRequest(BaseModel):
    verdict: Literal["correct", "partly", "wrong"]
    verdict_by: str = Field(min_length=1)


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


def _fingerprint(alert: dict) -> str:
    fp = alert.get("fingerprint")
    if isinstance(fp, str) and fp:
        return fp
    labels = alert.get("labels") or {}
    return hashlib.sha256(json.dumps(labels, sort_keys=True, default=str).encode()).hexdigest()


async def _create_incident(
    deps: AppDeps, req: DiagnoseRequest, incident_id: str | None = None
) -> str:
    incident_id = incident_id or str(uuid.uuid4())
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
    trace: list[dict] = []
    evidence: list = []
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
        trace, evidence = result.trace, result.evidence
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
            except Exception as exc:  # noqa: BLE001 - e.g. database error: do not leave 'running'
                log.error("propose failed: %s", type(exc).__name__)
                status = "failed"
                fallback = await _fallback(deps, evidence, trigger, "action proposal failed")

    await deps.pool_app.execute(
        "UPDATE incidents_seen SET status=$2, diagnosis=$3::jsonb, fallback=$4::jsonb, "
        "tokens_in=$5, tokens_out=$6, latency_ms=$7, trace=$8::jsonb WHERE id=$1",
        uuid.UUID(incident_id),
        status,
        _js(diagnosis.model_dump()) if diagnosis else None,
        _js(fallback) if fallback is not None else None,
        tokens_in,
        tokens_out,
        int((time.monotonic() - t0) * 1000),
        _js(trace),
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
        if len(self._hits) > 10_000:  # bad-token guessing creates keys; drop idle ones
            for k in [k for k, v in self._hits.items() if not v or now - v[-1] >= RATE_WINDOW_S]:
                del self._hits[k]
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

    def _throttle(request: Request) -> str:
        """Rate-limit every request, valid token or not, keyed on client address plus a hash
        of the presented token, so guessing is throttled. Returns the presented token."""
        header = request.headers.get("authorization", "")
        scheme, _, token = header.partition(" ")
        token = token if scheme.lower() == "bearer" else ""
        client = request.client.host if request.client else "unknown"
        key = client + ":" + hashlib.sha256(token.encode()).hexdigest()
        wait = limiter.check(key)
        if wait is not None:
            raise HTTPException(
                429, "rate limit exceeded", headers={"Retry-After": str(int(wait + 0.999))}
            )
        return token

    def _unauthorized() -> HTTPException:
        return HTTPException(401, "unauthorized", headers={"WWW-Authenticate": "Bearer"})

    def _matches(token: str, expected: str) -> bool:
        return bool(expected) and hmac.compare_digest(token.encode(), expected.encode())

    async def auth(request: Request) -> None:
        if not _matches(_throttle(request), deps.api_token):
            raise _unauthorized()

    async def webhook_auth(request: Request) -> None:
        if not _matches(_throttle(request), deps.webhook_token):
            raise _unauthorized()

    async def approver_auth(request: Request) -> str:
        """Return the approver's name. No credential: 401. Not an approver token: 403."""
        token = _throttle(request)
        if not token:
            raise _unauthorized()
        found: str | None = None
        for name, tok in deps.approvers.items():  # no early exit: constant work per request
            if _matches(token, tok):
                found = name
        if found is None:
            raise HTTPException(403, "forbidden: not an approver credential")
        return found

    guard = [Depends(auth)]

    @app.get("/healthz")
    async def healthz() -> dict:
        return {"status": "ok"}

    @app.post("/diagnose", response_model=DiagnoseResponse, dependencies=guard)
    async def diagnose(req: DiagnoseRequest) -> DiagnoseResponse:
        incident_id = await _create_incident(deps, req)
        return await _run(deps, incident_id, req)

    # fingerprint -> incident_id for incidents still open (running, diagnosed, needs_human).
    # Alertmanager re-sends still-firing alerts every repeat_interval. This map is in memory and
    # PER PROCESS: it is lost on restart and not shared between replicas, so a restart or a
    # second replica can open a duplicate incident. 'failed' incidents are removed so a later
    # re-send retries.
    open_alerts: dict[str, str] = {}
    sem = asyncio.Semaphore(MAX_CONCURRENT_DIAGNOSES)

    @app.post("/alerts", status_code=202, dependencies=[Depends(webhook_auth)])
    async def alerts(body: dict) -> dict:
        ids: list[str] = []
        for alert in body.get("alerts") or []:
            if not isinstance(alert, dict) or alert.get("status") != "firing":
                continue
            fp = _fingerprint(alert)
            existing = open_alerts.get(fp)
            if existing is not None:
                ids.append(existing)
                continue
            req = DiagnoseRequest(source="alert", alert=alert)
            incident_id = str(uuid.uuid4())
            open_alerts[fp] = incident_id  # reserve before any await: no duplicate race
            try:
                await _create_incident(deps, req, incident_id)
            except Exception:
                open_alerts.pop(fp, None)
                raise
            ids.append(incident_id)
            log.info("alert accepted: alertname=%s", (alert.get("labels") or {}).get("alertname"))
            task = asyncio.create_task(_safe_run(deps, incident_id, req, fp))
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
        out["trace"] = _load(row.get("trace"))
        out["actions"] = [_action_out(a) for a in actions]
        return out

    @app.post("/incidents/{incident_id}/verdict")
    async def verdict(
        incident_id: str, body: VerdictRequest, approver: str = Depends(approver_auth)
    ) -> dict:
        if body.verdict_by != approver:
            raise HTTPException(403, "verdict_by must match the authenticated approver")
        try:
            iid = uuid.UUID(incident_id)
        except ValueError:
            raise HTTPException(404, "incident not found") from None
        row = await deps.pool_app.fetchrow(
            "UPDATE incidents_seen SET verdict=$2, verdict_by=$3, verdict_at=now() "
            "WHERE id=$1 RETURNING id",
            iid,
            body.verdict,
            approver,
        )
        if row is None:
            raise HTTPException(404, "incident not found")
        return {"incident_id": incident_id, "verdict": body.verdict, "verdict_by": approver}

    async def _decide(action_id: str, approver: str, approve: bool) -> ActionResponse:
        try:
            uuid.UUID(action_id)
        except ValueError:
            raise HTTPException(404, "action not found") from None
        try:
            status = await k8s.decide(deps.pool_app, action_id, approve, approver, deps.k8s_api)
        except ActionError as exc:
            # Fixed messages only: never echo text from the actions layer.
            log.warning("decide refused: %s", type(exc).__name__)
            msg = str(exc)
            if "not found" in msg:
                raise HTTPException(404, "action not found") from None
            if "only 'proposed'" in msg or "rate limited" in msg:
                raise HTTPException(409, "action cannot be decided in its current state") from None
            raise HTTPException(422, "action rejected by validation") from None
        row = await deps.pool_app.fetchrow(
            "SELECT result FROM actions WHERE id = $1", uuid.UUID(action_id)
        )
        result = row["result"] if row else None
        if status == "failed":
            log.error("action failed: %s", _exc_class(result))
            result = ACTION_FAILED_MSG
        return ActionResponse(action_id=action_id, status=status, result=result)

    @app.post("/actions/{action_id}/approve", response_model=ActionResponse)
    async def approve(action_id: str, approver: str = Depends(approver_auth)) -> ActionResponse:
        return await _decide(action_id, approver, True)

    @app.post("/actions/{action_id}/reject", response_model=ActionResponse)
    async def reject(action_id: str, approver: str = Depends(approver_auth)) -> ActionResponse:
        return await _decide(action_id, approver, False)

    async def _safe_run(d: AppDeps, incident_id: str, req: DiagnoseRequest, fp: str) -> None:
        try:
            async with sem:  # excess diagnoses wait here; none are dropped
                res = await _run(d, incident_id, req)
            if res.status == "failed":
                open_alerts.pop(fp, None)
        except Exception as exc:  # noqa: BLE001 - background task: record, never crash the loop
            log.error("background diagnose failed: %s", type(exc).__name__)
            open_alerts.pop(fp, None)
            try:
                await d.pool_app.execute(
                    "UPDATE incidents_seen SET status='failed' WHERE id=$1",
                    uuid.UUID(incident_id),
                )
            except Exception:  # noqa: BLE001
                pass

    return app


def _exc_class(result: Any) -> str:
    """Class name from a stored 'ClassName: message' result; never the message."""
    head = str(result or "").partition(":")[0].strip()
    return head if head.isidentifier() else "unknown"


def _action_out(a: Any) -> dict:
    d = {k: _plain(v) for k, v in dict(a).items()}
    if d.get("status") == "failed":
        d["result"] = ACTION_FAILED_MSG
    return d


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
    webhook_token = os.environ.get("COPILOT_WEBHOOK_TOKEN", "")
    if not webhook_token:
        raise SystemExit(MISSING_WEBHOOK_MSG)
    try:
        approvers = parse_approvers(os.environ.get("COPILOT_APPROVERS", ""))
    except ValueError as exc:
        raise SystemExit(str(exc)) from None
    secrets = [token, webhook_token, *approvers.values()]
    if len(set(secrets)) != len(secrets):
        raise SystemExit("API, webhook and approver tokens must all be different")
    if not approvers:
        log.warning("COPILOT_APPROVERS is empty: nobody can approve actions or give verdicts")
    deps = AppDeps(
        pool_app=None,
        pool_ro=None,
        prom_client=None,
        prom_base_url=os.environ.get("PROMETHEUS_URL", "http://prometheus:9090"),
        embedder=None,
        guarded_model=None,
        k8s_api=None,
        api_token=token,
        webhook_token=webhook_token,
        approvers=approvers,
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


app = build_default_app()
