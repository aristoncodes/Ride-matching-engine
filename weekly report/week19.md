# Week 19 — Data Segregation

**Date:** Nov 12, 2026 · **Phase:** 5 (Production Orchestration) · **Status:** ✅ Complete
**Phase 5 complete.**

## What this week was about

Making institutional clients genuinely invisible to one another — and, more importantly, **proving
it** rather than asserting it.

> "Isolation you haven't tested is isolation you don't have."

## The line everything rests on

```go
tenantID := s.cfg.TenantID
if authenticated, ok := auth.TenantFromContext(r.Context()); ok {
    tenantID = authenticated
}
```

**The tenant comes from the authenticated key, never from the request.** If a client can name its own
tenant, every downstream key prefix is decoration.

`Config.TenantID` survives only as a local-development fallback, and there is deliberately **no
per-request override** — no header, no query parameter, no body field. The absence of that feature
*is* the security control.

## Threading it through every layer

| Layer | Scoping |
|---|---|
| API auth | tenant read off the verified key |
| Queue | `requests:stream:<tenant>` |
| **Dead-letter** | `requests:dead:<tenant>` |
| Cache | `drivers:geo:<tenant>`, `drivers:seen:<tenant>` |
| Locks | `lock:driver:<tenant>:<id>` |
| Engine | `tenant_id` on every batch |
| Logs | tenant on every structured line |

**The dead-letter queue is the one people forget.** The main stream gets scoped and the failure path
doesn't — and dead-lettered requests contain rider ids and pickup coordinates. A competitor reading
your failures is as bad as reading your successes.

## The layer a key prefix cannot protect

The pipeline buffer lives in **memory**, shared across tenants, and it was keyed by driver id alone.

Two operators can both legitimately have a driver called `D-001`. With a driver-only key, one
tenant's ping **silently relocates the other's driver** — corruption that would present as a flapping
GPS bug and would take a very long time to trace back to multi-tenancy.

Now keyed by `tenant + driver`, with each window's writes routed to that tenant's own store.

## The checkpoint: testing from the attacker's side

`internal/tenancy` is a **separate package**, deliberately. Isolation is a property of the
**composition**, not of any component — each package's own tests can only check its own layer, and a
leak *between* two individually-correct layers is exactly the kind nobody notices.

Every test is written as an attack. One client, holding only tenant A's key, tries:

| Attack | Result |
|---|---|
| Spoof `X-Tenant-ID: rival-cabs` on a request | request lands in A's queue; B's untouched |
| Read a competitor's drivers via a radius query | only A's drivers returned |
| Consume another tenant's queue | only A's requests delivered |
| Read another tenant's **dead-letter** stream | depth 0 |
| Act with no key at all | 401, nothing written |

Plus the subtler ones:

- **The same driver id in two tenants stays separate** — A's `D-001` in Bengaluru is not moved to
  Mumbai by B's write. Both fleets are placed at the *same coordinates* in one test so that only the
  tenant scoping can separate them; geography cannot.
- **Revoking one tenant's key** does not affect another's.
- **One tenant exhausting its rate limit** does not starve another.

## A test bug worth recording

`TestRevokingOneTenantsKeyDoesNotAffectAnother` first reported *"tenant A's revoked key still works"*.

It didn't. I captured `time.Now()` **before** revoking, and `Active()` compares the supplied clock
against `RevokedAt` — so a timestamp from before the revocation is legitimately still inside the
key's valid window. The middleware calls `now()` per request, so this was a property of my test.

Worth knowing anyway: **revocation is instant with respect to request time**, not to whatever clock a
caller happens to be holding.

## Not done

The original one-line deliverable also mentioned **rider cancellations and driver rejections**. Those
are product state transitions rather than isolation, and they are deferred rather than quietly
dropped from the plan.

## Phase 5 is complete

| Week | Deliverable | Checkpoint |
|---|---|---|
| 16 | Kubernetes | pod deleted mid-traffic → **zero dropped requests** |
| 17 | CI/CD | a red test blocked a merge (it caught a real linter break) |
| 18 | API keys | revoked key rejected instantly |
| 19 | Data segregation | cross-tenant access denied at every layer |

Phase 6 (Week 20, Nov 19) is enterprise hardening: pprof profiling, traffic blasting, GC tuning, and
structured telemetry — with the Week 15 finding that solve cost is superlinear in N as the obvious
first thing to attack.

## Files touched

`internal/tenancy/isolation_test.go` (new), `internal/api/server.go`,
`internal/ingest/server.go`, `internal/pipeline/pipeline.go`, `internal/locations/repository.go`.
