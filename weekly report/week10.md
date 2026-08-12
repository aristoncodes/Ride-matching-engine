# Week 10 — Durable Ride-Request Queue (Redis Streams)

**Date:** Sep 10, 2026 · **Phase:** 3 (Message Brokering & Batching) · **Status:** ✅ Complete

## What this week was about

Week 6 made a C++ crash *survivable* — the Go process stays up and gets a retryable error. Survival
is not durability: a ride request held only in Go's memory when that process dies is gone. This
week puts the requests somewhere they outlive any process.

## The decision: Redis Streams over Kafka ([ADR-0006](../docs/adr/0006-redis-streams-over-kafka.md))

| | Kafka | Redis Streams |
|---|---|---|
| Consumer groups + acks | ✅ | ✅ |
| Redelivery after a crash | ✅ | ✅ (XPENDING/XCLAIM) |
| Retention / arbitrary replay | ✅ strong | ❌ capped stream |
| Throughput ceiling | enormous | tens of thousands/s |
| **New infrastructure** | **a broker in every environment** | **none — Redis is already here** |

Redis is already a hard dependency for driver locations. Adding Kafka means a second stateful
system in local dev, CI, and production, paid again in Week 14 (Compose), Week 16 (K8s), and Week
17 (CI). Streams provide exactly the semantics the requirement names; Kafka provides those *plus*
capabilities nothing here uses.

The honest counter-argument is that Kafka is the stronger CV signal. A crisp ADR explaining the
tradeoff is a better answer than picking the bigger dependency by default — and the escape hatch is
a `queue.Queue` interface, so a Kafka implementation would be one package.

## The asymmetry that defines the design

| | Driver GPS pings (W7/W9) | Ride requests (W10) |
|---|---|---|
| Nature | **state** | **events** |
| Only the latest matters? | yes | no |
| May be coalesced? | yes | **never** |
| May be shed under load? | yes | **never** |

Losing a driver's position for one window is invisible — they ping again in 3 seconds. Losing a ride
request means a customer standing on a street corner who was never told no. **Getting these
backwards is a serious design error in either direction.**

## The three commands that make durability real

```
XADD                          append; the request is now the broker's problem
XREADGROUP ... NOACK=false    claim into THIS consumer's Pending Entries List
XACK                          done — remove from pending
```

A claimed-but-unacked message stays in the consumer's PEL indefinitely. **That is the whole
mechanism.**

## The failure mode I had to design against

**Durability without reclaim is worthless.**

A crashed consumer's messages are safely stored and delivered to *nobody* — from the rider's point
of view, identical to having lost them. So `Reclaim` (XPENDING + XCLAIM) is not an optimisation; it
is the other half of the guarantee, and something must call it on a schedule.

`XPENDING` before `XCLAIM` specifically because it reports the **delivery count**, which XCLAIM
alone does not surface. Without that count there is no way to tell a message that failed once from
one that has failed fifty times — and therefore no basis for a dead-letter decision.

## `minIdle`: the line between "crashed" and "slow"

`Reclaim` only takes messages idle longer than `minIdle`. Set it too low and you steal work from a
consumer that is merely slow, and **two consumers process the same ride request concurrently** —
which means dispatching two cars. There is a test that a live consumer's in-flight work is
protected.

## Dead-letter path

Two routes in, for different reasons:

1. **Exceeded `MaxDeliveries`** — poison that keeps coming back and would occupy a consumer slot
   forever.
2. **Undecodable payload** — will fail identically on every redelivery, so it is dead-lettered
   immediately rather than looping.

Dead-lettering **acks in the same operation**, so a poison message stops blocking the queue. The
pipeline ordering is chosen so the surviving failure is safe: if the ack fails after the write-aside,
the message is dead-lettered twice — a harmless, inspectable duplicate. The reverse ordering could
lose it entirely.

## Small decisions with real consequences

- **Group created at `"0"`, not `"$"`.** `"$"` starts at the current end and silently skips every
  request already waiting — a data-loss bug that only appears on a cold start. There is a test.
- **`MAXLEN ~` is mandatory.** An untrimmed stream is the same slow-motion OOM as the Week 9
  unbounded buffer. `NewRedisStream` refuses a zero `MaxLen`.
- **Unique consumer name per process, enforced.** Two consumers sharing a name share a pending list,
  and each treats the other's in-flight work as abandoned.
- **Flat fields, not a JSON blob.** Redis Streams are natively field-based, so a human debugging a
  stuck queue can read an entry with `XRANGE` without decoding anything.
- **Broker id ≠ business id.** Acking needs the broker's id; deduplication needs `RequestID`.
  Conflating them breaks the moment a message is redelivered.

## Checkpoint

> ✅ Kill a consumer mid-process and confirm the request is redelivered, not dropped.

`TestUnackedMessageIsRedelivered`:

1. Consumer A claims a request and **dies without acking**.
2. The message is still `Pending` — it survived.
3. A plain `Consume` from consumer B correctly returns **nothing**: `">"` yields only
   never-delivered messages, and this one was delivered to A. **Reclaim is the only way back.**
4. `Reclaim` recovers it intact, with an incremented delivery count.
5. B acks, and pending drops to 0.

Step 3 is the one that matters. It is exactly the trap: the message *looks* gone to normal
consumption, and without a reclaimer it would stay that way forever.

## Tests

9 cases against a real redis-server: round trip, the checkpoint, `minIdle` protection, poison
dead-lettering, undecodable-payload dead-lettering, **consumer groups splitting work without
duplication** (two batchers, 50 requests, no message to both), cold-start delivery, publish
validation, and depth/pending health reporting.

## The bug

Every consumer after the first failed to start:

```go
err.Error()[:8] == "BUSYGROUP"   // "BUSYGROUP" is NINE characters
```

The slice compared `"BUSYGROU"`, so the "group already exists" case was never recognised and became
fatal. Invisible until a second process existed. Replaced with `strings.HasPrefix`.

## Files touched

`internal/queue/{queue,redis_stream,redis_stream_test}.go`, `internal/testutil/redisproc.go`
(raw-command helper), `docs/adr/0006-redis-streams-over-kafka.md`.
