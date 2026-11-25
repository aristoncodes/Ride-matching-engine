"""Approved Kubernetes actions (AC-1, AC-2, AC-3).

propose() only records a row. Nothing touches the cluster unless decide(approve=True) is
called by a human-identified caller. Every state change is its own UPDATE, so the audit log
survives a crash mid-action.
"""

from __future__ import annotations

import asyncio
import time
import uuid
from collections.abc import Callable
from datetime import timedelta
from typing import Any, Protocol

import asyncpg

from copilot.models import SCALE_LIMITS, ProposedAction

NAMESPACE = "ride-matching"
RESTART_WINDOW_MINUTES = 10
READY_TIMEOUT_S = 120
POLL_INTERVAL_S = 3
# deployment -> allowed action kinds (AC-1)
ALLOWED: dict[str, frozenset[str]] = {
    "matching-engine": frozenset({"restart_pods", "scale"}),
    "batcherd": frozenset({"scale"}),
}


class ActionError(Exception):
    """An action was refused: not allowed, out of bounds, rate limited, or in the wrong state."""


class RestartStalled(ActionError):
    """A replacement pod was not Ready in time; `restarted` pods were already replaced."""

    def __init__(self, restarted: int, total: int) -> None:
        super().__init__(f"restart stalled after {restarted} of {total} pods")
        self.restarted = restarted
        self.total = total


class K8sApi(Protocol):
    def restart_pods(self, namespace: str, deployment: str) -> str: ...

    def scale(self, namespace: str, deployment: str, replicas: int) -> str: ...


class DisabledK8sActions:
    """K8sApi for a deployment with no Kubernetes access (for example Cloud Run).

    Proposals and approvals still work; executing an approved action fails with a fixed message.
    """

    def restart_pods(self, namespace: str, deployment: str) -> str:
        raise ActionError("kubernetes actions are disabled in this deployment")

    def scale(self, namespace: str, deployment: str, replicas: int) -> str:
        raise ActionError("kubernetes actions are disabled in this deployment")


def validate(kind: str, deployment: str, replicas: int | None) -> None:
    """Raise ActionError unless this exact action is allowed. Pure; no I/O."""
    if kind not in ("restart_pods", "scale"):
        raise ActionError(f"action kind {kind!r} is not allowed")
    if deployment not in ALLOWED:
        raise ActionError(f"deployment {deployment!r} is not allowed")
    if kind not in ALLOWED[deployment]:
        raise ActionError(f"{kind} is not allowed on {deployment}")
    if kind == "scale":
        if replicas is None:
            raise ActionError("scale requires replicas")
        lo, hi = SCALE_LIMITS[deployment]
        if not lo <= replicas <= hi:
            raise ActionError(f"replicas {replicas} outside [{lo}, {hi}] for {deployment}")
    elif replicas is not None:
        raise ActionError("restart_pods takes no replicas")


class KubernetesActions:
    """Real K8sApi backed by kubernetes.client. Always operates in the ride-matching namespace."""

    def __init__(
        self,
        *,
        core: Any = None,
        apps: Any = None,
        sleep: Callable[[float], None] = time.sleep,
        timeout_s: float = READY_TIMEOUT_S,
        interval_s: float = POLL_INTERVAL_S,
    ) -> None:
        if core is None or apps is None:
            from kubernetes import client, config

            try:
                config.load_incluster_config()
            except config.ConfigException:
                config.load_kube_config()
            core = core or client.CoreV1Api()
            apps = apps or client.AppsV1Api()
        self._core = core
        self._apps = apps
        self._sleep = sleep
        self._timeout_s = timeout_s
        self._interval_s = interval_s

    def _check_ns(self, namespace: str) -> None:
        if namespace != NAMESPACE:
            raise ActionError(f"namespace {namespace!r} is not allowed")

    def restart_pods(self, namespace: str, deployment: str) -> str:
        # Restart = delete the deployment's pods; the ReplicaSet recreates them.
        # This matches the RBAC in k8s/base/copilot-rbac.yaml (pods get/list/delete).
        # A deployment patch would need a verb the copilot Role does not grant.
        self._check_ns(namespace)
        validate("restart_pods", deployment, None)
        selector = f"app.kubernetes.io/name={deployment}"  # the label used in k8s/base
        pods = self._core.list_namespaced_pod(namespace, label_selector=selector)
        names = [p.metadata.name for p in pods.items]
        target = sum(1 for p in pods.items if _ready(p))
        restarted = 0
        # One pod at a time: deleting them all at once would be a full outage.
        for name in names:
            self._core.delete_namespaced_pod(name, namespace)
            if not self._wait_ready(namespace, selector, target):
                raise RestartStalled(restarted, len(names))
            restarted += 1
        return f"restarted {restarted} {deployment} pods one at a time"

    def _wait_ready(self, namespace: str, selector: str, target: int) -> bool:
        waited = 0.0
        while True:
            pods = self._core.list_namespaced_pod(namespace, label_selector=selector)
            if sum(1 for p in pods.items if _ready(p)) >= target:
                return True
            if waited >= self._timeout_s:
                return False
            self._sleep(self._interval_s)
            waited += self._interval_s

    def scale(self, namespace: str, deployment: str, replicas: int) -> str:
        self._check_ns(namespace)
        validate("scale", deployment, replicas)
        body = {"spec": {"replicas": replicas}}
        self._apps.patch_namespaced_deployment_scale(deployment, namespace, body)
        return f"scaled {deployment} to {replicas} replicas"


def _ready(pod: Any) -> bool:
    """A pod counts as Ready only if it is not terminating and its Ready condition is True."""
    if getattr(pod.metadata, "deletion_timestamp", None):
        return False
    conds = getattr(pod.status, "conditions", None) or []
    return any(c.type == "Ready" and c.status == "True" for c in conds)


async def propose(pool: asyncpg.Pool, incident_id: str, action: ProposedAction, reason: str) -> str:
    """Record a proposed action. Never executes anything. Returns the action id."""
    if action.kind == "none":
        raise ActionError("a 'none' action cannot be proposed")
    validate(action.kind, action.deployment, action.replicas)
    action_id = str(uuid.uuid4())
    await pool.execute(
        """INSERT INTO actions (id, incident_id, kind, deployment, replicas, reason,
                                status, proposed_at)
           VALUES ($1, $2, $3, $4, $5, $6, 'proposed', now())""",
        uuid.UUID(action_id),
        uuid.UUID(incident_id),
        action.kind,
        action.deployment,
        action.replicas,
        reason or action.reason,
    )
    return action_id


async def decide(
    pool: asyncpg.Pool, action_id: str, approve: bool, decided_by: str, api: K8sApi
) -> str:
    """Approve or reject a proposed action. Returns the final status."""
    if not isinstance(decided_by, str) or not decided_by.strip():
        raise ActionError("decided_by must be a non-empty string")
    aid = uuid.UUID(action_id)

    async with pool.acquire() as conn:
        async with conn.transaction():
            row = await conn.fetchrow("SELECT * FROM actions WHERE id = $1 FOR UPDATE", aid)
            if row is None:
                raise ActionError(f"action {action_id} not found")
            if row["status"] != "proposed":
                raise ActionError(f"action is {row['status']}, only 'proposed' can be decided")

            if not approve:
                await conn.execute(
                    "UPDATE actions SET status='rejected', decided_by=$2, decided_at=now() "
                    "WHERE id=$1",
                    aid,
                    decided_by,
                )
                return "rejected"

            # Checked before the approval is recorded: a bad action is never approved.
            validate(row["kind"], row["deployment"], row["replicas"])
            if row["kind"] == "restart_pods":
                # Serialise concurrent restart approvals for one deployment.
                await conn.execute(
                    "SELECT pg_advisory_xact_lock(hashtext($1))", "restart:" + row["deployment"]
                )
                recent = await conn.fetchval(
                    """SELECT count(*) FROM actions
                       WHERE kind='restart_pods' AND deployment=$1 AND id <> $2
                         AND ((status IN ('executed', 'failed')
                               AND executed_at > now() - $3::interval)
                           OR (status='approved' AND decided_at > now() - $3::interval))""",
                    row["deployment"],
                    aid,
                    timedelta(minutes=RESTART_WINDOW_MINUTES),
                )
                if recent:
                    raise ActionError(
                        f"restart of {row['deployment']} rate limited: one per "
                        f"{RESTART_WINDOW_MINUTES} minutes"
                    )
            await conn.execute(
                "UPDATE actions SET status='approved', decided_by=$2, decided_at=now() "
                "WHERE id=$1",
                aid,
                decided_by,
            )

        # Approval is committed. Execute outside the transaction; record each outcome.
        try:
            result = await asyncio.to_thread(_execute, api, row)
        except Exception as exc:  # noqa: BLE001 - any failure is recorded, then reported
            await conn.execute(
                "UPDATE actions SET status='failed', executed_at=now(), result=$2 WHERE id=$1",
                aid,
                _failure_text(exc),
            )
            return "failed"
        await conn.execute(
            "UPDATE actions SET status='executed', executed_at=now(), result=$2 WHERE id=$1",
            aid,
            result,
        )
        return "executed"


def _failure_text(exc: Exception) -> str:
    """Fixed text plus the class name. Never str(exc): API errors carry bodies and headers."""
    text = f"failed: {type(exc).__name__}"
    if isinstance(exc, RestartStalled):
        text += f" (restarted {exc.restarted} of {exc.total} pods)"
    return text


def _execute(api: K8sApi, row: Any) -> str:
    if row["kind"] == "restart_pods":
        return api.restart_pods(NAMESPACE, row["deployment"])
    return api.scale(NAMESPACE, row["deployment"], row["replicas"])
