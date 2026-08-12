# Week 9 — Pipeline Integration (and the first thing you can actually run)

**Date:** Sep 3, 2026 · **Phase:** 2 (Go Bridge & State Ingestion) · **Status:** ✅ Complete
**Phase 2 complete.**

## What this week was about

Wiring ingestion into the cache as one flowing pipe on the 3-second cadence — and making it stay
bounded when more traffic arrives than it can handle.

```
drivers --WebSocket--> ingest --coalesce--> pipeline --3s window--> Redis
```

## The core idea: coalescing

A driver pings every ~3 seconds, and **only their latest position matters**. An older fix is not
partial information — it is *wrong* information that a newer one has already superseded.

So the window buffer is a map keyed by driver id:

```go
buffer map[string]locations.DriverLocation
```

Ten pings from one driver in a window become one write. The reduction costs nothing, because
everything discarded was already worthless. A slice would preserve an order nobody wants and grow
without bound in the process.

Measured live: **5,438 pings → 2,000 writes**.

## Bounded, deliberate shedding

Under overload the choice is never "drop nothing". It is **drop something you chose**, or **drop
everything when the process OOMs**.

The bound is on **distinct drivers**, not raw pings:

- a **new** driver past the cap is shed and counted;
- an **update** to an already-buffered driver is always accepted, because it reuses an existing
  entry, costs no memory, and refusing it would throw away fresher data for nothing.

That asymmetry is the whole design. Memory is bounded by driver count; freshness is never
sacrificed for a saving that doesn't exist.

**A failed flush drops the window rather than retrying it into the next one.** Re-queueing would
grow the buffer during exactly the outage that caused the failure — and the data is superseded
within seconds anyway, because every driver pings again. Losing a window of positions during a
Redis outage is cheap; OOMing the ingestion layer is not.

## The lock is never held across the store write

```go
p.mu.Lock()
batch := p.buffer
p.buffer = make(map[string]locations.DriverLocation, len(batch))
p.mu.Unlock()          // swap under the lock

err := p.store.UpsertMany(ctx, locs)   // write OUTSIDE it
```

Holding the lock across a Redis round trip would block **every connection goroutine** for its
duration, turning one slow dependency into a stalled ingestion layer.
`TestSlowStoreCannotBlockIngestion` asserts `Accept` stays fast while a 300 ms flush is in flight.

## Configuration that is refused rather than accepted

`FlushTimeout > Window` fails at construction. A flush slower than the window means the next window
begins before the previous one finished, and queued flushes are precisely the unbounded growth this
component exists to prevent. Better a startup error than a slow leak.

## Runnable services

| Command | Role |
|---|---|
| `cmd/ingestd` | WebSocket → pipeline → Redis, with `/healthz` and `/stats` |
| `cmd/mockdrivers` | Load generator: N simulated drivers streaming pings |

`ingestd` shuts down in a specific order, and the order is the point:

1. stop accepting new connections;
2. close existing ones and **wait** for their goroutines;
3. let the pipeline perform its **final flush**.

Draining before the last flush is what stops a "clean" exit from silently discarding the final
window of driver positions.

`mockdrivers` staggers each driver's first ping. Without that, every driver fires on the same
millisecond and the load arrives as a spike per interval rather than a stream — not what a real
fleet looks like.

## Checkpoint

> ✅ Mock pings flow in every 3s, Redis reflects them, and the system stays bounded under overload.

**Normal load** — 500 drivers, 1 ping/second, 12 seconds:

```
total: sent=5438 failed=0 refused=0

pings_received 5438        pipeline_flushed 2000      ← coalescing: 2.7x fewer writes
pipeline_shed 0            pipeline_windows 7
pipeline_flush_failures 0  drivers_indexed 500

GEOSEARCH drivers:geo:demo FROMLONLAT 77.5946 12.9716 BYRADIUS 3 km ASC
  D-00329  0.1022 km
  D-00062  0.2552 km
  D-00015  0.5363 km
```

**Overload** — 600 drivers at 5 pings/second, against a 200-connection and 50-driver-buffer limit:

```
total: sent=9767 failed=0 refused=400

connections_total 200      connections_rejected 400   ← limit enforced, 503 + Retry-After
pings_received 9767        pipeline_shed 7287         ← deliberate, counted
pipeline_flush_failures 0  pipeline_buffered 30       ← never above the cap of 50
```

**Zero failures, zero crashes, nothing unbounded.** The system degraded exactly where it was told
to and stayed up.

## Bug worth recording

A test compared `12.97 + 9*0.001` against a runtime-computed `12.97 + float64(9)*0.001` and failed
while printing two **identical** numbers:

```
lat = 12.979000, want 12.979000
```

Go evaluates untyped constant arithmetic at arbitrary precision and rounds **once**; the runtime
version rounds at **every step**. Different bits, same display. The fix was to compute the expected
value the same way the code does.

## Phase 2 is complete

| Week | Deliverable | Checkpoint |
|---|---|---|
| 6 | gRPC bridge to the C++ engine | survives a SIGKILLed engine, recovers alone |
| 7 | Redis live location store | stale drivers excluded from radius queries |
| 8 | WebSocket ingestion | 0 goroutines leaked over 100 cycles |
| 9 | Pipeline integration | bounded under 5× overload |

Phase 3 (Week 10, Sep 10) starts message brokering: Kafka / Redis Streams, consumer groups with
explicit acks, and a dead-letter path — where the Week 6 retryable-vs-poison taxonomy finally gets
used for what it was designed for.

## Files touched

`internal/pipeline/{pipeline,pipeline_test}.go`, `cmd/ingestd/main.go`,
`cmd/mockdrivers/main.go`.
