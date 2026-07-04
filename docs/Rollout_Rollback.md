# Rollout & Rollback Strategy

**Status:** Draft · **Related:** [Runbook.md](Runbook.md), [Test_Strategy.md](Test_Strategy.md)

How changes reach production safely and how to undo them fast. The ability to roll back in minutes is what makes shipping frequently safe.

---

## 1. Principles
- **Every change is revertible.** If it can't be rolled back, it needs a migration plan and extra scrutiny.
- **Deploy small, deploy often.** Small diffs make failures easy to attribute and revert.
- **Automate the gate.** Humans decide *what* ships; CI decides *whether it's allowed to* (Week 17).

## 2. Pipeline (CI/CD — Week 17)
```
commit ─▶ build ─▶ unit + integration + sanitizers + lint ─▶ (merge gate)
       ─▶ build versioned image ─▶ deploy to staging ─▶ smoke tests
       ─▶ canary (small % / one tenant) ─▶ monitor ─▶ full rollout
```
A red gate blocks the merge. A failed canary blocks the full rollout.

## 3. Deployment strategy (Kubernetes — Week 16)
- **Stateless Go services & C++ workers:** rolling update with readiness probes; K8s shifts traffic only to ready pods.
- **Canary:** route a small fraction (or a single internal/pilot tenant) to the new version first; compare golden signals against the baseline before proceeding.
- **Versioned images:** every deploy is an immutable, tagged image → rollback = redeploy the previous tag.

## 4. Rollback triggers
Roll back immediately if, after a deploy:
- `ride_requests_dropped_total` goes above 0 (hard invariant), **or**
- error rate or p99 latency crosses SLO for >5 min, **or**
- the C++ worker enters a crash loop, **or**
- any cross-tenant isolation signal fires.

## 5. Rollback procedure
1. **Decide fast** — mitigation over diagnosis (see Runbook golden rule).
2. `kubectl rollout undo` the affected deployment (or redeploy the prior image tag).
3. Confirm golden signals return to baseline.
4. Verify the hard invariant: dropped requests back to 0.
5. Open a **COE** ([templates/postmortem-coe-template.md](templates/postmortem-coe-template.md)) — a rollback is an incident.

## 6. Special cases
- **Schema / data migrations:** must be **backward-compatible** (expand-then-contract): add new fields first, migrate, then remove old — so the previous app version still runs during rollback. Never a destructive migration in the same deploy that depends on it.
- **The gRPC contract (`matching.proto`):** additive changes only within a major version (new optional fields OK; removing/renumbering fields is a breaking change → new version). Go and C++ must be able to run one version apart during a rolling deploy.
- **Config changes:** treated as deploys — same canary + rollback discipline.

## 7. Pre-launch readiness checklist (PRR-lite)
Before the first production rollout, confirm:
- [ ] SLOs defined and measured ([SLO_SLA.md](SLO_SLA.md))
- [ ] Dashboards + alerts live, each alert links to a runbook ([Observability.md](Observability.md))
- [ ] Runbook covers the known failure modes ([Runbook.md](Runbook.md))
- [ ] Chaos test passed: kill-the-worker → 0 dropped requests
- [ ] Rollback tested in staging (not just documented)
- [ ] Multi-tenant isolation test passing ([Threat_Model.md](Threat_Model.md))
- [ ] Load test meets latency SLO at target scale
