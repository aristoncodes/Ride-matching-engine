# Week 22 — Optimising, and being wrong about why

**Date:** Dec 3, 2026 · **Phase:** 6 (Enterprise Hardening) · **Status:** ✅ Complete

## What this week was about

Fixing what the profiler flagged — and, as it turned out, learning the difference
between what a profile *shows* and what it *means*.

## The change

Week 20's baseline: `batcherd` spending **~59% of CPU inside Redis calls from
`processBatch`**. The cause was visible in the code — one `Nearby` per rider, in
a loop:

```go
for _, m := range msgs {
    near, err := b.drivers.Nearby(ctx, ...)   // 500 riders = 500 round trips
}
```

Replaced with one pipelined call: all the `GEOSEARCH`es in a single round trip,
then **one** `ZMScore` over the union of candidates for the freshness filter.

## The correctness guard came first

```go
// NearbyMany must return exactly what N sequential Nearby calls return,
// including the freshness filter.
```

An optimisation that quietly returns different drivers is a **matching bug**, not
a performance win. The test compares both paths across four query shapes and
asserts identical drivers in identical order, plus a separate test that the
batched path still excludes stale drivers — that filter has a different shape in
the batched version (union ZMScore vs per-query), which is exactly where it would
be lost.

## The result: 1.2–1.5×, not 10×

I wrote the regression test asserting `>1.5×`. **It failed at 1.2×.**

That failure taught me what the profile alone had not:

```
loopback RTT                    ~0.10 ms   (measured: redis-cli --latency)
500 sequential queries           131 ms
  of which round trips          ~50 ms
  of which Redis executing      ~80 ms     ← pipelining cannot touch this
500 pipelined queries            110 ms
```

Redis is **single-threaded**. Pipelining removes the round trips; it does not
make the server execute 500 `GEOSEARCH`es any faster, and the client still parses
500 result sets. On loopback that caps the win near 1.4×.

## What I actually got wrong

The profile said *"59% of CPU is inside Redis calls."* I read that as *"too many
round trips."*

Those are different claims. Most of that time was the process **waiting on
syscalls for query execution** — real work, correctly attributed, that I
misdiagnosed as overhead.

**A profiler tells you WHERE time goes. It does not tell you WHY, and the
inference in between is yours to get wrong.**

## The change is still right — for a different reason than I thought

The ratio is a property of the **deployment**, not the code. At a realistic
0.5 ms RTT (Redis on another host), those 500 round trips cost 250 ms rather than
50 ms, and removing them dominates everything else. The optimisation matters more
in production than it does on my laptop.

So the assertion is now a **revert guard** (`>1.05×`) with the reasoning written
into the test, rather than a performance claim that only holds on one machine.

## Checkpoint, honestly assessed

> ✅ Routing decisions stay sub-millisecond under stress.

**Routing decisions: yes.** A* is ~0.6 ms per query; a 100-rider solve is 0.4 ms.

**End-to-end match latency: no, and it cannot be.** It is bounded by the
3-second batch window (ADR-0005) — a request arriving just after a flush waits
nearly a full window before it is even considered.

Both numbers are true, and quoting the first as the rider's experience would be
dishonest. Week 23's SLOs record them as separate metrics for exactly this
reason.

## The other thing the data already told us

Week 15 measured solve cost as **superlinear in N** (~O(N^1.33)), so
`MAX_BATCH` is a latency control: four 800-rider batches total ~158 ms against
~328 ms for one 3200-rider batch. That remains the largest available win, and it
is a *configuration* change rather than a code one — which is why it was left as
a documented lever rather than hard-coded.

## Files touched

`internal/locations/{repository,redis,nearby_bench_test,redis_test}.go`,
`internal/batcher/batcher.go`, `internal/pipeline/pipeline.go`.
