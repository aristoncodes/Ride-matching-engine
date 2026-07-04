# Operational Runbook

**Status:** Draft (populate as components ship) · **Related:** [Observability.md](Observability.md), [Rollout_Rollback.md](Rollout_Rollback.md)

Step-by-step responses to the failures we expect. Written before on-call, not during the incident. Each section is linked from an alert in the observability plan.

> **Golden rule during an incident:** stop the bleeding first (mitigate), diagnose second. A rollback or restart that restores service beats a perfect root-cause found 40 minutes later.

---

## General first steps
1. Check the **golden-signals dashboard** — is this latency, errors, traffic, or saturation?
2. Check **recent deploys** — did an incident start right after a rollout? If so, consider rollback ([Rollout_Rollback.md](Rollout_Rollback.md)).
3. Identify **blast radius** — one tenant or all? one component or the pipeline?

---

## <a id="dropped-requests"></a>Dropped requests (Sev-1)
**Symptom:** `ride_requests_dropped_total > 0`. This is a hard-invariant breach.
1. Confirm via logs: search `request_id` for requests ingested but never reaching a terminal state.
2. Check the **queue**: are acks happening *before* a successful match? (They must happen *after* — see ADR-0005 / Architecture §4.)
3. Check for a **poison message** stuck in redelivery — inspect the dead-letter path.
4. Mitigate: if a bad deploy changed ack timing, **roll back**.
5. This always triggers a **COE** ([templates/postmortem-coe-template.md](templates/postmortem-coe-template.md)).

## <a id="high-latency"></a>High match latency (Sev-3)
**Symptom:** compute p99 > 1ms or end-to-end p99 > 3.5s.
1. Trace a slow request (OpenTelemetry) — which span dominates?
2. If **`solve-assignment`** dominates: batch sizes too large? Sparse cost matrix disabled? (see ADR-0003).
3. If **`gRPC SolveBatch`** dominates: network, worker CPU saturation, or serialization — check worker CPU.
4. If **`batch-window`** dominates: expected (intentional 3s); confirm it's not stacking.
5. Longer term: feed into the Week 22 profiling/optimization loop.

## <a id="queue-backlog"></a>Queue backlog (Sev-2)
**Symptom:** `queue_depth` rising steadily.
1. Are consumers alive and keeping up? Scale the batcher/worker pool.
2. Is the C++ tier the bottleneck? Scale C++ workers (stateless pool).
3. If ingestion > capacity: confirm **backpressure/load-shedding** is engaging rather than growing memory unbounded (Week 9).

## <a id="worker-crash-loop"></a>C++ worker crash loop (Sev-2)
**Symptom:** worker restarts > 3 in 5m.
1. Requests should still be **safe** (redelivery) — confirm dropped count is still 0.
2. Pull crash logs / core dump — is it a specific input (e.g., a degenerate batch)?
3. If input-triggered: capture the batch, roll back if recent deploy, add a regression test.
4. Recall the design: an isolated worker crash is *contained* by gRPC-over-process (ADR-0002) — the Go layer stays up.

## <a id="ingestion-down"></a>Ingestion down (Sev-1)
**Symptom:** API/WebSocket uptime probe failing.
1. Is it the app or the infra (LB, ingress, node)? Check K8s pod status.
2. Are readiness probes failing? Check dependencies (Redis/DB reachable?).
3. Mitigate: restart/scale pods; if deploy-related, roll back.
4. Communicate to affected tenants per SLA.

## <a id="tenant-isolation"></a>Suspected cross-tenant leak (Sev-1, security)
**Symptom:** a tenant reports seeing another's data, or `cross_tenant_denials` behaves unexpectedly in prod.
1. **Contain first:** if confirmed, consider suspending the affected endpoint.
2. Identify which layer failed to scope by `tenant_id` (API / cache / queue / DB).
3. Preserve logs for the COE; notify security owner.
4. This is always a COE and may be client-reportable.

---

## Escalation
- Sev-1: mitigate immediately; COE required within 48h.
- Sev-2/3: handle in hours; COE at discretion.
- Unknown/novel failure: mitigate (rollback/restart), then investigate with the dashboards + traces.
