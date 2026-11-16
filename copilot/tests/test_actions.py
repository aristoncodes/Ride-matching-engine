"""Tests for approved Kubernetes actions (AC-1..AC-3)."""

from __future__ import annotations

import os
import uuid
from collections.abc import AsyncIterator

import asyncpg
import pytest
import pytest_asyncio

from copilot.actions.k8s import (
    ActionError,
    KubernetesActions,
    RestartStalled,
    decide,
    propose,
    validate,
)
from copilot.db import apply_schema, connect
from copilot.models import ProposedAction


class FakeApi:
    def __init__(self, fail: bool = False) -> None:
        self.calls: list[tuple] = []
        self.fail = fail

    def restart_pods(self, namespace: str, deployment: str) -> str:
        self.calls.append(("restart", namespace, deployment))
        if self.fail:
            raise RuntimeError("boom-secret-body")
        return "restarted"

    def scale(self, namespace: str, deployment: str, replicas: int) -> str:
        self.calls.append(("scale", namespace, deployment, replicas))
        return f"scaled to {replicas}"


def _act(kind="scale", deployment="matching-engine", replicas=2) -> ProposedAction:
    return ProposedAction(kind=kind, deployment=deployment, replicas=replicas)


# ---- pure validation ----


@pytest.mark.parametrize(
    ("dep", "n"),
    [("matching-engine", 0), ("matching-engine", 5), ("batcherd", 0), ("batcherd", 4)],
)
def test_scale_out_of_bounds_refused(dep, n):
    with pytest.raises(ActionError):
        validate("scale", dep, n)


@pytest.mark.parametrize(
    ("dep", "n"),
    [("matching-engine", 1), ("matching-engine", 4), ("batcherd", 1), ("batcherd", 3)],
)
def test_scale_bounds_inclusive(dep, n):
    validate("scale", dep, n)


def test_bad_deployment_refused():
    with pytest.raises(ActionError):
        validate("scale", "ingestd", 2)


def test_restart_on_batcherd_refused():
    with pytest.raises(ActionError):
        validate("restart_pods", "batcherd", None)


def test_scale_without_replicas_refused():
    with pytest.raises(ActionError):
        validate("scale", "batcherd", None)


@pytest.mark.asyncio
async def test_propose_none_refused():
    with pytest.raises(ActionError):
        await propose(None, str(uuid.uuid4()), _act("none", "none", None), "x")  # type: ignore[arg-type]


@pytest.mark.asyncio
@pytest.mark.parametrize("who", ["", "   "])
async def test_empty_decided_by_refused(who):
    api = FakeApi()
    with pytest.raises(ActionError):
        await decide(None, str(uuid.uuid4()), True, who, api)  # type: ignore[arg-type]
    assert api.calls == []


# ---- restart_pods, one pod at a time (fake cluster) ----


class _Obj:
    def __init__(self, **kw) -> None:
        self.__dict__.update(kw)


class FakeCore:
    """Fake pods. A deleted pod is replaced by a pod that turns Ready after `lag` lists."""

    def __init__(self, n=3, lag=1, stall=False) -> None:
        self.pods = {f"p{i}": True for i in range(n)}
        self.lag, self.stall = lag, stall
        self.pending: list[list] = []  # [name, lists_left]
        self.log: list[tuple] = []
        self.n = n

    def list_namespaced_pod(self, ns, label_selector=None):
        assert label_selector == "app.kubernetes.io/name=matching-engine"
        for p in self.pending:
            if p[1] > 0:
                p[1] -= 1
            elif not self.stall:
                self.pods[p[0]] = True
        items = [
            _Obj(
                metadata=_Obj(name=n, deletion_timestamp=None),
                status=_Obj(conditions=[_Obj(type="Ready", status="True" if r else "False")]),
            )
            for n, r in self.pods.items()
        ]
        self.log.append(("list", sum(self.pods.values())))
        return _Obj(items=items)

    def delete_namespaced_pod(self, name, ns):
        self.log.append(("delete", sum(self.pods.values())))
        del self.pods[name]
        self.n += 1
        new = f"p{self.n}"
        self.pods[new] = False
        self.pending.append([new, self.lag])


def _k8s(core, sleeps):
    return KubernetesActions(core=core, apps=object(), sleep=sleeps.append)


def test_restart_deletes_one_pod_at_a_time():
    core, sleeps = FakeCore(n=3), []
    msg = _k8s(core, sleeps).restart_pods("ride-matching", "matching-engine")
    assert "3" in msg
    deletes = [e for e in core.log if e[0] == "delete"]
    assert len(deletes) == 3
    # every delete happens only when all 3 pods were Ready again (never an outage)
    assert all(ready == 3 for _, ready in deletes)
    assert sleeps and set(sleeps) == {3}


def test_restart_stalled_stops_and_reports_progress():
    core, sleeps = FakeCore(n=3, stall=True), []
    with pytest.raises(RestartStalled) as ei:
        _k8s(core, sleeps).restart_pods("ride-matching", "matching-engine")
    assert ei.value.restarted == 0
    assert len([e for e in core.log if e[0] == "delete"]) == 1
    assert sum(sleeps) == 120


def test_restart_namespace_checked():
    with pytest.raises(ActionError):
        _k8s(FakeCore(), []).restart_pods("kube-system", "matching-engine")


# ---- Postgres paths ----

pg = pytest.mark.pg


@pytest_asyncio.fixture
async def pool() -> AsyncIterator[asyncpg.Pool]:
    p = await connect(os.environ["COPILOT_TEST_DSN"])
    await apply_schema(p)
    await p.execute("DELETE FROM actions")
    yield p
    await p.execute("DELETE FROM actions")
    await p.close()


async def _incident(pool: asyncpg.Pool) -> str:
    iid = uuid.uuid4()
    await pool.execute(
        "INSERT INTO incidents_seen (id, opened_at, source, trigger, status) "
        "VALUES ($1, now(), 'question', '{}', 'running')",
        iid,
    )
    return str(iid)


@pg
@pytest.mark.asyncio
async def test_propose_does_not_execute(pool):
    aid = await propose(pool, await _incident(pool), _act(), "load")
    row = await pool.fetchrow("SELECT * FROM actions WHERE id=$1", uuid.UUID(aid))
    assert row["status"] == "proposed"
    assert row["decided_by"] is None


@pg
@pytest.mark.asyncio
async def test_approve_scale_executes_and_audits(pool):
    api = FakeApi()
    aid = await propose(pool, await _incident(pool), _act(replicas=3), "load")
    assert await decide(pool, aid, True, "alice", api) == "executed"
    assert api.calls == [("scale", "ride-matching", "matching-engine", 3)]
    row = await pool.fetchrow("SELECT * FROM actions WHERE id=$1", uuid.UUID(aid))
    assert row["status"] == "executed"
    assert row["decided_by"] == "alice"
    assert row["decided_at"] and row["executed_at"] and row["result"] == "scaled to 3"
    with pytest.raises(ActionError):  # cannot decide twice
        await decide(pool, aid, True, "alice", api)
    assert len(api.calls) == 1


@pg
@pytest.mark.asyncio
async def test_reject_never_calls_api(pool):
    api = FakeApi()
    aid = await propose(pool, await _incident(pool), _act(), "load")
    assert await decide(pool, aid, False, "bob", api) == "rejected"
    assert api.calls == []
    row = await pool.fetchrow("SELECT * FROM actions WHERE id=$1", uuid.UUID(aid))
    assert row["status"] == "rejected" and row["decided_by"] == "bob" and row["decided_at"]


@pg
@pytest.mark.asyncio
async def test_failure_recorded(pool):
    api = FakeApi(fail=True)
    aid = await propose(pool, await _incident(pool), _act("restart_pods", replicas=None), "hung")
    assert await decide(pool, aid, True, "alice", api) == "failed"
    row = await pool.fetchrow("SELECT * FROM actions WHERE id=$1", uuid.UUID(aid))
    assert row["status"] == "failed" and row["decided_by"] == "alice"
    assert row["result"] == "failed: RuntimeError"
    assert "boom" not in row["result"]


@pg
@pytest.mark.asyncio
async def test_restart_rate_limited(pool):
    api = FakeApi()
    iid = await _incident(pool)
    a1 = await propose(pool, iid, _act("restart_pods", replicas=None), "r1")
    a2 = await propose(pool, iid, _act("restart_pods", replicas=None), "r2")
    assert await decide(pool, a1, True, "alice", api) == "executed"
    with pytest.raises(ActionError):
        await decide(pool, a2, True, "alice", api)
    assert len(api.calls) == 1
    row = await pool.fetchrow("SELECT status, decided_by FROM actions WHERE id=$1", uuid.UUID(a2))
    assert row["status"] == "proposed" and row["decided_by"] is None


@pg
@pytest.mark.asyncio
async def test_bad_row_not_approved(pool):
    """A row with out-of-bounds replicas (bypassing propose) is refused before approval."""
    api = FakeApi()
    aid = uuid.uuid4()
    await pool.execute(
        "INSERT INTO actions (id, incident_id, kind, deployment, replicas, reason, status, "
        "proposed_at) VALUES ($1,$2,'scale','batcherd',9,'x','proposed',now())",
        aid,
        uuid.UUID(await _incident(pool)),
    )
    with pytest.raises(ActionError):
        await decide(pool, str(aid), True, "alice", api)
    assert api.calls == []
    status = await pool.fetchval("SELECT status FROM actions WHERE id=$1", aid)
    assert status == "proposed"


@pg
@pytest.mark.asyncio
async def test_failed_restart_counts_toward_rate_limit(pool):
    iid = await _incident(pool)
    a1 = await propose(pool, iid, _act("restart_pods", replicas=None), "r1")
    a2 = await propose(pool, iid, _act("restart_pods", replicas=None), "r2")
    assert await decide(pool, a1, True, "alice", FakeApi(fail=True)) == "failed"
    api = FakeApi()
    with pytest.raises(ActionError):
        await decide(pool, a2, True, "alice", api)
    assert api.calls == []


@pg
@pytest.mark.asyncio
async def test_copilot_app_cannot_delete_actions():
    dsn = os.environ["COPILOT_TEST_DSN"]
    p = await connect(dsn)
    try:
        await apply_schema(p)
        has_role = await p.fetchval("SELECT 1 FROM pg_roles WHERE rolname='copilot_app'")
        if not has_role:
            pytest.skip("copilot_app role not present")
        can_delete = await p.fetchval(
            "SELECT has_table_privilege('copilot_app', 'actions', 'DELETE')"
        )
        can_insert = await p.fetchval(
            "SELECT has_table_privilege('copilot_app', 'actions', 'INSERT')"
        )
        assert can_insert and not can_delete
    finally:
        await p.close()
