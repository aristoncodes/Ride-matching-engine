# Week 7 — Live Driver Locations in Redis (GEOADD / GEOSEARCH)

**Date:** Aug 20, 2026 · **Phase:** 2 (Go Bridge & State Ingestion) · **Status:** ✅ Complete

## What this week was about

A fast, shared store of where every driver is *right now*. The matcher needs to ask "who is near
this rider?" thousands of times a minute, and the answer must be both quick and **true** — a
driver who stopped pinging ten minutes ago is still sitting in the index at their last known spot,
and matching a rider to them means dispatching a car that is not there.

## Interface first, Redis second

`locations.Repository` is defined before the implementation. Everything upstream depends on the
interface, so the pipeline (Week 9) is tested against an in-memory fake with no Redis at all, and
the store can be swapped later without those callers noticing.

```go
type Repository interface {
    UpsertDriver(ctx, driverID string, lat, lng float64) error
    UpsertMany(ctx, locations []DriverLocation) error
    Nearby(ctx, q Query) ([]DriverLocation, error)
    RemoveDriver(ctx, driverID string) error
    Reap(ctx) (int, error)
    Count(ctx) (int, error)
    Close() error
}
```

## The problem that shapes the design: Redis has no per-member TTL

A Redis geo set **is a sorted set** whose score encodes a geohash. There is no per-member expiry —
only per-key. So `EXPIRE` on the geo key would drop **every driver in the city at once**, which is
useless.

The fix is a companion sorted set holding each driver's last-ping timestamp as its score:

```
drivers:geo:<tenant>    ZSET, score = geohash    (GEOADD)
drivers:seen:<tenant>   ZSET, score = unix ms    (ZADD)
```

Freshness becomes a score range, which Redis answers in O(log N).

**Both a read filter and a reaper are required, and they do different jobs:**

- **Filtering on read** keeps stale drivers out of *answers*.
- **Reaping on a ticker** keeps them out of *memory*.

Filtering alone leaves dead drivers in the index forever. Reaping alone serves stale drivers in the
window between sweeps. This is asserted directly: after a driver goes stale, `Nearby` excludes it
**and** `Count` proves it is still resident until reaped.

## Details that would be bugs if skipped

**One `ZMSCORE`, not N `ZSCORE`s.** The freshness check needs a score per candidate. Doing it per
driver would be N round trips inside a 3-second window — how a fast store becomes a slow one.

**Over-fetch before filtering.** `Nearby` asks Redis for 3× the requested limit, because some
results will be dropped as stale. Asking for exactly `limit` would silently return short lists
precisely when the fleet is churning and the caller most needs a full shortlist.

**Bounded reaping.** The reaper deletes at most 10,000 per sweep. An unbounded reap after an
outage would pull millions of ids into one command and stall Redis, which is single-threaded — the
reaper would cause the outage it exists to clean up after.

**Fail closed.** A driver present in the geo index with no freshness score is treated as stale. It
is exactly as untrustworthy as one whose ping is ancient.

**`ZREM`, not `GEOREM`.** There is no `GEOREM`. A geo set is a sorted set, so you delete from it
with `ZREM` — which surprises most people exactly once.

## Resilience

- **Pooling** (`PoolSize`), so thousands of ingestion goroutines share connections instead of each
  opening a socket.
- **Dial/read/write timeouts.** Without them a network partition leaves a goroutine blocked on a
  socket read for minutes. The whole point of a *live* location store is that it answers now or
  not at all.
- **Retry with exponential backoff *and jitter*.** The jitter is not decoration: without it, every
  goroutine that failed at the same instant — which is what a Redis restart produces — retries at
  the same instant, and the recovering server takes a synchronised thundering herd exactly when it
  is least able to cope.
- **Retry only what is transient.** A dropped connection is worth retrying; a `WRONGTYPE` is a bug
  and retrying it just fails three times as slowly. Context cancellation is never retried — the
  caller has already given up.

## TTL sizing

30 seconds, against a ~3-second ping interval — roughly ten consecutive losses tolerated. Too
short and a driver in a tunnel drops out of the pool; too long and the matcher dispatches cars that
left minutes ago.

## Checkpoint

> ✅ Driver locations update in Redis and a radius query returns only recently-seen drivers.

`TestStaleDriversAreNotReturned` uses an **injected clock** rather than sleeping: at t=0 two
drivers are visible; at t=20s both are still fresh; at t=40s the one that kept pinging survives and
the silent one is gone — while `Count` proves it still occupies memory until the reaper runs.

A TTL test that sleeps 30 real seconds is a test nobody runs; one that shortens the TTL to 50 ms
tests a different system. An injectable clock is the way out of that trap.

## Tests

10 cases against a **real redis-server**, one private instance per test on a unix socket with
persistence disabled. Covers: radius correctness and nearest-first ordering, staleness, reaping,
upsert-replaces-not-appends (a driver who moves must not leave a ghost at every old position),
limits, batch validation before any write, **concurrent writers under `-race`**, Redis killed
mid-flight, and the reaper's clean shutdown on context cancel.

## The bug worth remembering

Tests failed — but only the ones with **long names**.

A unix socket address is a fixed-size array in the kernel (`sun_path`, 104 bytes on macOS), and Go's
`t.TempDir()` embeds the test's name in the path:

```
/var/folders/.../T/TestStaleDriversAreNotReturned2114060021/001/redis.sock   = exactly 104 chars
```

So whether the test passed was a function of **how long its name was**, which is about as
misleading a symptom as it gets. Fixed by creating the socket under a short temp path.

## Files touched

`internal/locations/{repository,redis,redis_test}.go`, `internal/testutil/redisproc.go`,
`infrastructure/go.mod`.
