# Week 13 — Distributed Locking (the named top risk)

**Date:** Oct 1, 2026 · **Phase:** 3 (Message Brokering & Batching) · **Status:** ✅ Complete
**Phase 3 complete.**

## What this week was about

Guaranteeing two riders are never matched to the same driver at once — and proving the mechanism
does not become the bottleneck, which the TDD names as a top risk.

## Why the C++ solver is not enough

This is the part worth understanding properly.

The engine's unit-capacity flow model guarantees **no driver is used twice within one batch**. That
is a real guarantee and it is the wrong scope. Two batcher instances solving two *different* batches
concurrently can both include the same nearby driver. Each solve is internally correct, and the
combination dispatches one car to two riders.

**A per-batch invariant says nothing about concurrent batches.** The fix has to live outside the
solver.

## Leases, not locks

Every acquisition carries a TTL.

A lock without one is a deadlock waiting for a crash: the holder dies mid-batch and that driver
becomes permanently unmatchable, with nothing in the system able to distinguish "in use" from
"abandoned". A lease expires on its own, so the worst case of a crash is a few seconds of
unavailability.

`Extend` renews for legitimately long work — which is what lets the TTL stay *short*. The
alternative, a TTL sized for the worst case, strands drivers for exactly that long after every crash.

## Atomic acquire

```
SET key token NX PX ttl
```

One command. The tempting GET-then-SET version has a race between the two calls wide enough for two
batchers to both observe "free" and both write — exactly the double-booking this exists to prevent.

## Fencing tokens, and the bug they prevent

This is the subtlest thing in the package:

```
holder A: acquires, token=abc
holder A: stalls...
          lease expires
holder B: acquires, token=xyz      (legitimately!)
holder A: wakes up, calls Release
          -> DEL deletes B's lease
          -> driver is now double-bookable
```

The mechanism meant to prevent double-booking causes it. So Release and Extend run a **Lua script**
that checks the token before acting:

```lua
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
```

Lua because the check and the delete must be **atomic** — as two Go statements, the same race
reopens between them. There is a test that walks exactly this sequence.

## Geohash partitioning — and why geohash specifically

The named risk is contention. One global lock serialises every batcher in the fleet: throughput
becomes one batch at a time regardless of instance count, and adding instances makes it *worse*.

**Contention is spatial.** Two batchers collide precisely when working the same neighbourhood,
because that is when their candidate driver sets overlap.

- Hashing by **driver id** would scatter one neighbourhood's drivers across every partition, so a
  busy area touches all of them and everything re-serialises. Statistically uniform, and useless for
  the actual problem.
- A **geohash** is a Z-order curve over interleaved lat/lng bits, so a shared prefix means physical
  proximity. A concert letting out in one district contends only with itself while the rest of the
  city proceeds untouched.

Precision 5 (~4.9 km cells) is chosen to match the candidate search radius, so a batch usually
touches one or two cells.

## Checkpoint

> ✅ Concurrent match attempts on the same driver resolve to exactly one winner, and a crashed
> holder's lock self-releases.

**One winner:** 100 goroutines released simultaneously by a barrier, all racing for one driver →
**exactly 1 winner, 99 clean `ErrNotAcquired`, 0 errors.** The barrier matters: without it the
goroutines trickle in and the race is never actually exercised.

**Self-release:** a holder "crashes" without releasing; a second manager is correctly refused, then
acquires successfully once the lease expires.

## "Prove it scales" — measured, not asserted

40 concurrent workers spread across a metropolitan area:

| Strategy | Acquired concurrently | Wall clock |
|---|---|---|
| One global lock | **3 / 40** | 11.0 ms |
| Geohash partitions | **40 / 40** | 3.1 ms |

**~13× the concurrency and 3.5× faster**, with a partition spread of 0.90. The claim in the TDD is
now a number rather than an intuition.

## The test bug worth recording

The contention test first failed with a spread of 0.28.

The riders were spaced 0.02° (~2.2 km) apart, while a precision-5 geohash cell is ~4.9 km — so most
of those "distinct" riders landed in the *same* partition.

That was **the test being unrealistic, not the partitioning being broken**: two riders 2 km apart
genuinely *are* in one neighbourhood and *should* contend, because their candidate sets overlap.
Fixing the test meant spacing riders 0.06° apart — genuinely different neighbourhoods — after which
the spread was 0.90.

It would have been easy and wrong to "fix" this by lowering the threshold or shrinking the cell size.

## Phase 3 is complete

| Week | Deliverable | Checkpoint |
|---|---|---|
| 10 | Durable ride-request queue | request redelivered after a consumer crash |
| 11 | Ride Request REST service | malformed → 4xx with request id, never 500 |
| 12 | Match Batcher microservice | batches form under light *and* heavy load |
| 13 | Distributed locking | exactly 1 winner of 100; 40/40 vs 3/40 partitioned |

The full pipeline now runs end to end:

```
rider  --REST-->  requestd  --stream-->  batcherd  --gRPC-->  C++ engine
driver --WS---->  ingestd   --3s window-->  Redis GEO  ------^
```

Phase 4 (Week 14, Oct 8) is DevOps: Docker Compose, load testing, Kubernetes, CI/CD — which is
where the Docker daemon finally has to be running.

## Files touched

`internal/locks/{lease,lease_test}.go`.
