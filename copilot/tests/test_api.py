import asyncio
import json
import os
import uuid

import httpx
import pytest

pytestmark = pytest.mark.asyncio

os.environ.setdefault("COPILOT_API_TOKEN", "test-token")
os.environ.setdefault("COPILOT_WEBHOOK_TOKEN", "test-webhook")
os.environ.setdefault("COPILOT_APPROVERS", "root=test-approver")

from copilot.agents.coordinator import InvestigationResult  # noqa: E402
from copilot.agents.model import ModelUnavailable  # noqa: E402
from copilot.api import app as app_mod  # noqa: E402
from copilot.models import Diagnosis, Evidence, ProposedAction  # noqa: E402

TOKEN = "tok-123"
AUTH = {"Authorization": f"Bearer {TOKEN}"}
WEBHOOK = {"Authorization": "Bearer hook-tok"}
ALICE = {"Authorization": "Bearer alice-tok"}


class Conn:
    def __init__(self, pool):
        self.pool = pool

    async def fetchrow(self, sql, *args):
        return self.pool.action_row

    async def execute(self, sql, *args):
        self.pool.calls.append((sql, args))

    async def fetchval(self, sql, *args):
        return 0

    def transaction(self):
        return _Ctx(None)


class _Ctx:
    def __init__(self, v):
        self.v = v

    async def __aenter__(self):
        return self.v

    async def __aexit__(self, *a):
        return False


class FakePool:
    def __init__(self):
        self.calls = []
        self.incident_row = None
        self.action_row = None

    async def execute(self, sql, *args):
        self.calls.append((sql, args))

    async def fetchrow(self, sql, *args):
        if sql.lstrip().startswith("UPDATE"):
            self.calls.append((sql, args))
        return self.incident_row if "incidents_seen" in sql else {"result": None}

    async def fetch(self, sql, *args):
        if "doc_chunks" in sql:
            return [
                {
                    "source_file": "docs/R.md",
                    "anchor": "a",
                    "heading": "h",
                    "content": "c",
                    "score": 0.9,
                }
            ]
        return []

    def acquire(self):
        return _Ctx(Conn(self))

    def inserts(self, table):
        return [c for c in self.calls if c[0].lstrip().startswith(f"INSERT INTO {table}")]

    def calls_for(self, text):
        return [c for c in self.calls if text in c[0]]

    def updates(self):
        return [c for c in self.calls if c[0].lstrip().startswith("UPDATE incidents_seen")]


class Emb:
    async def embed(self, texts):
        return [[0.1, 0.2] for _ in texts]


class FakeApi:
    def __init__(self):
        self.calls = []

    def restart_pods(self, ns, dep):
        self.calls.append(("restart", dep))
        return "ok"

    def scale(self, ns, dep, n):
        self.calls.append(("scale", dep, n))
        return "ok"


def diagnosis(**kw):
    base = dict(
        summary="s",
        root_cause="r",
        component="batcherd",
        confidence=0.9,
        proposed_action=ProposedAction(kind="scale", deployment="batcherd", replicas=2),
    )
    base.update(kw)
    return Diagnosis(**base)


@pytest.fixture
def env(monkeypatch):
    state = {"result": InvestigationResult(diagnosis())}

    async def fake_investigate(trigger, **kw):
        r = state["result"]
        if isinstance(r, Exception):
            raise r
        return r

    monkeypatch.setattr(app_mod, "investigate", fake_investigate)
    pool = FakePool()
    clock = {"t": 1000.0}
    deps = app_mod.AppDeps(
        pool_app=pool,
        pool_ro=pool,
        prom_client=None,
        prom_base_url="http://p",
        embedder=Emb(),
        guarded_model=object(),
        k8s_api=FakeApi(),
        api_token=TOKEN,
        webhook_token="hook-tok",
        approvers={"alice": "alice-tok", "bob": "bob-tok"},
        clock=lambda: clock["t"],
    )
    app = app_mod.create_app(deps)
    client = httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://t")
    return app, client, pool, state, clock


BODY = {"source": "question", "question": "why is the queue growing?"}


async def test_auth(env):
    _, c, *_ = env
    assert (await c.post("/diagnose", json=BODY)).status_code == 401
    bad = {"Authorization": "Bearer wrong"}
    assert (await c.post("/diagnose", json=BODY, headers=bad)).status_code == 401
    r = await c.get("/healthz")
    assert r.status_code == 200 and r.json() == {"status": "ok"}


@pytest.mark.parametrize(
    "body",
    [
        {"source": "alert", "question": "q"},
        {"source": "question"},
        {"source": "detector", "alert": {}},
        {"source": "question", "question": "q", "alert": {"a": 1}},
    ],
)
async def test_source_mismatch_422(env, body):
    _, c, *_ = env
    assert (await c.post("/diagnose", json=body, headers=AUTH)).status_code == 422


async def test_diagnose_ok(env):
    _, c, pool, *_ = env
    r = await c.post("/diagnose", json=BODY, headers=AUTH)
    assert r.status_code == 200
    j = r.json()
    assert j["status"] == "diagnosed" and j["diagnosis"]["component"] == "batcherd"
    assert j["action_id"]
    assert len(pool.inserts("incidents_seen")) == 1
    assert pool.updates()[0][1][1] == "diagnosed"
    assert len(pool.inserts("actions")) == 1


async def test_needs_human_proposes_nothing(env):
    _, c, pool, state, _ = env
    state["result"] = InvestigationResult(diagnosis(needs_human=True))
    j = (await c.post("/diagnose", json=BODY, headers=AUTH)).json()
    assert j["status"] == "needs_human" and j["action_id"] is None
    assert not pool.inserts("actions")


async def test_model_unavailable_fallback(env):
    _, c, pool, state, _ = env
    ev = [Evidence(id="E1", kind="log", ref="1", excerpt="x")]
    state["result"] = InvestigationResult(None, ev)
    r = await c.post("/diagnose", json=BODY, headers=AUTH)
    assert r.status_code == 200
    j = r.json()
    assert j["status"] == "failed" and j["diagnosis"] is None
    assert j["fallback"]["evidence"][0]["id"] == "E1"
    assert j["fallback"]["runbook_matches"][0]["anchor"] == "a"
    assert pool.updates()[0][1][1] == "failed"


async def test_model_unavailable_exception_still_200(env):
    _, c, _, state, _ = env
    state["result"] = ModelUnavailable("down")
    r = await c.post("/diagnose", json=BODY, headers=AUTH)
    assert r.status_code == 200 and r.json()["status"] == "failed"


def firing(name, fp):
    return {"status": "firing", "fingerprint": fp, "labels": {"alertname": name}}


async def test_alerts_firing_only(env):
    app, c, pool, *_ = env
    body = {
        "alerts": [
            {"status": "firing", "labels": {"alertname": "A"}, "annotations": {}},
            {"status": "resolved", "labels": {"alertname": "B"}},
            {"status": "firing", "labels": {"alertname": "C"}},
        ]
    }
    r = await c.post("/alerts", json=body, headers=WEBHOOK)
    assert r.status_code == 202
    assert len(r.json()["incident_ids"]) == 2
    await asyncio.gather(*list(app.state.tasks))
    assert len(pool.inserts("incidents_seen")) == 2
    assert len(pool.updates()) == 2


async def test_rate_limit(env):
    _, c, _, _, clock = env
    for _ in range(30):
        assert (await c.get("/incidents/" + str(uuid.uuid4()), headers=AUTH)).status_code == 404
    r = await c.get("/incidents/" + str(uuid.uuid4()), headers=AUTH)
    assert r.status_code == 429 and "retry-after" in r.headers
    clock["t"] += 61
    assert (await c.get("/incidents/" + str(uuid.uuid4()), headers=AUTH)).status_code == 404


async def test_approve_not_proposed_409(env):
    _, c, pool, *_ = env
    pool.action_row = {"status": "executed"}
    r = await c.post(f"/actions/{uuid.uuid4()}/approve", headers=ALICE)
    assert r.status_code == 409
    assert "executed" not in r.text
    assert not env[2].inserts("actions")


async def test_webhook_token_alerts_only(env):
    app, c, *_ = env
    body = {"alerts": [firing("A", "f1")]}
    assert (await c.post("/alerts", json=body, headers=WEBHOOK)).status_code == 202
    await asyncio.gather(*list(app.state.tasks))
    assert (await c.post("/alerts", json=body, headers=AUTH)).status_code == 401
    r = await c.post(f"/actions/{uuid.uuid4()}/approve", headers=WEBHOOK)
    assert r.status_code == 403
    assert (await c.post("/diagnose", json=BODY, headers=WEBHOOK)).status_code == 401


async def test_api_token_cannot_approve(env):
    _, c, *_ = env
    r = await c.post(f"/actions/{uuid.uuid4()}/approve", headers=AUTH)
    assert r.status_code == 403
    r = await c.post(f"/actions/{uuid.uuid4()}/approve")
    assert r.status_code == 401


async def test_approver_decided_by_is_token_name(env):
    _, c, pool, *_ = env
    aid = str(uuid.uuid4())
    seen = {}

    async def fake_decide(p, action_id, approve, decided_by, api):
        seen["by"] = decided_by
        return "executed"

    orig = app_mod.k8s.decide
    app_mod.k8s.decide = fake_decide
    try:
        r = await c.post(f"/actions/{aid}/approve", json={"decided_by": "mallory"}, headers=ALICE)
    finally:
        app_mod.k8s.decide = orig
    assert r.status_code == 200 and r.json()["status"] == "executed"
    assert seen["by"] == "alice"


async def test_failed_action_result_is_fixed_message(env):
    _, c, pool, *_ = env

    async def fake_decide(*a):
        return "failed"

    async def fetchrow(sql, *args):
        return {"result": "ApiException: secret detail"}

    pool.fetchrow = fetchrow
    orig = app_mod.k8s.decide
    app_mod.k8s.decide = fake_decide
    try:
        r = await c.post(f"/actions/{uuid.uuid4()}/approve", headers=ALICE)
    finally:
        app_mod.k8s.decide = orig
    assert r.json()["result"] == "action execution failed; see logs"


async def test_bad_token_is_rate_limited(env):
    _, c, *_ = env
    bad = {"Authorization": "Bearer guess"}
    for _ in range(30):
        assert (await c.get("/incidents/" + str(uuid.uuid4()), headers=bad)).status_code == 401
    r = await c.get("/incidents/" + str(uuid.uuid4()), headers=bad)
    assert r.status_code == 429


async def test_duplicate_firing_alert_deduped(env):
    app, c, pool, state, _ = env
    gate = asyncio.Event()

    async def slow_investigate(trigger, **kw):
        await gate.wait()
        return state["result"]

    app_mod.investigate = slow_investigate  # env's monkeypatch restores the original
    body = {"alerts": [firing("A", "same")]}
    first = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    second = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    assert second == first  # still running: the re-send is deduped
    gate.set()
    await asyncio.gather(*list(app.state.tasks))
    assert len(pool.inserts("incidents_seen")) == 1


async def test_resolved_then_firing_starts_new_incident(env):
    app, c, pool, *_ = env
    body = {"alerts": [firing("A", "cycle")]}
    first = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))  # diagnosed
    resolved = {"alerts": [{"status": "resolved", "fingerprint": "cycle", "labels": {}}]}
    assert (await c.post("/alerts", json=resolved, headers=WEBHOOK)).status_code == 202
    second = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))
    assert second != first
    assert len(pool.inserts("incidents_seen")) == 2


async def test_diagnosed_alert_refires_without_resolve(env):
    app, c, pool, *_ = env
    body = {"alerts": [firing("A", "refire")]}
    first = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))
    second = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))
    assert second != first


async def test_many_random_tokens_one_address_throttled(env):
    _, c, *_ = env
    codes = []
    for _ in range(40):
        h = {"Authorization": f"Bearer random-{uuid.uuid4()}"}
        codes.append((await c.get("/incidents/" + str(uuid.uuid4()), headers=h)).status_code)
    assert codes[:30] == [401] * 30
    assert set(codes[30:]) == {429}
    # a valid token has its own bucket and is not locked out by the failures
    assert (await c.get("/incidents/" + str(uuid.uuid4()), headers=AUTH)).status_code == 404


async def test_rate_limiter_is_bounded_lru():
    lim = app_mod._RateLimiter(lambda: 0.0)
    for i in range(app_mod.RATE_MAX_KEYS + 500):
        lim.check(f"k{i}")
    assert len(lim._hits) == app_mod.RATE_MAX_KEYS
    assert "k0" not in lim._hits and f"k{app_mod.RATE_MAX_KEYS + 499}" in lim._hits


async def test_failed_incident_allows_retry(env):
    app, c, pool, state, _ = env
    state["result"] = InvestigationResult(None)
    body = {"alerts": [firing("A", "again")]}
    first = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))
    second = (await c.post("/alerts", json=body, headers=WEBHOOK)).json()["incident_ids"]
    await asyncio.gather(*list(app.state.tasks))
    assert second != first


async def test_propose_db_error_marks_failed(env):
    _, c, pool, *_ = env

    async def boom(*a, **k):
        raise RuntimeError("db down")

    orig = app_mod.k8s.propose
    app_mod.k8s.propose = boom
    try:
        j = (await c.post("/diagnose", json=BODY, headers=AUTH)).json()
    finally:
        app_mod.k8s.propose = orig
    assert j["status"] == "failed" and j["fallback"] is not None and j["action_id"] is None
    assert pool.updates()[0][1][1] == "failed"


async def test_verdict(env):
    _, c, pool, *_ = env
    iid = uuid.uuid4()
    pool.incident_row = {"id": iid}
    url = f"/incidents/{iid}/verdict"
    r = await c.post(url, json={"verdict": "partly", "verdict_by": "alice"}, headers=ALICE)
    assert r.status_code == 200
    sql, args = pool.calls_for("verdict=")[0]
    assert args[1:] == ("partly", "alice")
    bad = await c.post(url, json={"verdict": "meh", "verdict_by": "alice"}, headers=ALICE)
    assert bad.status_code == 422
    other = await c.post(url, json={"verdict": "wrong", "verdict_by": "bob"}, headers=ALICE)
    assert other.status_code == 403
    nonappr = await c.post(url, json={"verdict": "wrong", "verdict_by": "alice"}, headers=AUTH)
    assert nonappr.status_code == 403


async def test_verdict_missing_incident_404(env):
    _, c, *_ = env
    r = await c.post(
        f"/incidents/{uuid.uuid4()}/verdict",
        json={"verdict": "correct", "verdict_by": "alice"},
        headers=ALICE,
    )
    assert r.status_code == 404


async def test_trace_stored_and_returned(env):
    _, c, pool, state, _ = env
    trace = [{"step": 1, "agent": "logs", "tool": "t", "ms": 3}]
    state["result"] = InvestigationResult(diagnosis(), trace=trace)
    await c.post("/diagnose", json=BODY, headers=AUTH)
    assert json.loads(pool.updates()[0][1][7]) == trace
    iid = uuid.uuid4()
    pool.incident_row = {
        "id": iid, "status": "diagnosed", "trigger": "{}", "diagnosis": None,
        "fallback": None, "trace": json.dumps(trace),
    }  # fmt: skip
    j = (await c.get(f"/incidents/{iid}", headers=AUTH)).json()
    assert j["trace"] == trace


async def test_incident_404(env):
    _, c, *_ = env
    assert (await c.get(f"/incidents/{uuid.uuid4()}", headers=AUTH)).status_code == 404
    assert (await c.get("/incidents/not-a-uuid", headers=AUTH)).status_code == 404


async def test_get_incident(env):
    _, c, pool, *_ = env
    iid = uuid.uuid4()
    pool.incident_row = {
        "id": iid,
        "status": "diagnosed",
        "trigger": '{"a": 1}',
        "diagnosis": None,
        "fallback": None,
    }
    j = (await c.get(f"/incidents/{iid}", headers=AUTH)).json()
    assert j["status"] == "diagnosed" and j["trigger"] == {"a": 1} and j["actions"] == []
