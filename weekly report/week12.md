# Week 12 — The Match Batcher (where everything converges)

**Date:** Sep 24, 2026 · **Phase:** 3 (Message Brokering & Batching) · **Status:** ✅ Complete

## What this week was about

The microservice that turns a stream of individual ride requests into the batched matrices the C++
engine wants. It is where every previous week meets:

```
queue (W10)      -> pops durable ride requests
locations (W7)   -> finds candidate drivers near each rider
engine (W6)      -> solves the batch optimally in C++
taxonomy (W6)    -> decides ack / requeue / dead-letter per failure
```

Its own process (`cmd/batcherd`), horizontally scalable: instances share one consumer group, so each
request goes to exactly one of them.

## Dual-trigger flush

```
flush when:  window elapsed  OR  MaxBatchSize reached
```

Both are needed and they protect **different** things:

- **Time alone** → a sudden spike builds an enormous batch that blows the solve budget and the memory
  bound.
- **Size alone** → a quiet period leaves a lone rider waiting indefinitely for company that never
  arrives.

`FlushReason` is recorded per batch, because the ratio is diagnostic: consistently size-triggered
means the window is too long for the load; consistently time-triggered means there is spare capacity.

## The correctness property everything rests on

**A request is acked only after it has been matched.**

Acking on receipt is simpler and silently drops every in-flight request on a crash — precisely the
failure ADR-0002 and ADR-0006 exist to prevent.

And the subtler half: **an unmatched rider is not acked either.** They are still waiting. Leaving
the message pending means it is retried in a later window when more drivers may be on shift. That is
the "retain and retry" tenet made concrete rather than assumed.

## Failure handling, driven by the Week 6 taxonomy

| Failure | Action | Why |
|---|---|---|
| Engine crashed / timed out (**retryable**) | leave the whole batch unacked | nothing is wrong with these requests |
| Malformed / missing graph / too large (**not retryable**) | dead-letter | retrying is futile and blocks the queue forever |
| Driver store down | requeue | the outage is not the riders' fault |
| Zero candidate drivers | requeue | a driver may come on shift in seconds |

Three weeks after writing `Retryable(err)`, it is doing exactly the job it was designed for.

## Drivers deduplicated across riders

Each rider gets its own radius query; the union is deduplicated by driver id. A driver near two
riders must appear **once** — sending them twice lets the solver believe there are two cars where
there is one, and it will happily assign both.

## Per-batch metrics

Size, driver count, matched/unmatched, **match rate**, **queue-wait p50**, solve duration, total
duration. Emitted through a hook so tests observe batches deterministically instead of scraping logs.

**p50 rather than mean** for queue wait: one very old reclaimed request would drag an average and
hide what a typical rider actually experiences.

## Checkpoint

> ✅ Batches form correctly under both light and heavy load, with metrics visible.

**Light load** — 3 riders, `MaxBatchSize` 100:
```
reason=timer riders=3 matched=3 match_rate=1.00
```

**Heavy load** — 40 riders, `MaxBatchSize` 10, window 10s (so the timer *cannot* be what fires):
```
reason=size riders=10
```

**End to end, all four services running:**
```
riders=30 drivers=44 matched=17 unmatched=13 match_rate=0.57
queue_wait_p50_ms=716 solve_ms=1 total_ms=25
```
The 13 unmatched stayed correctly `pending` for a later window. (57% rather than higher because
`k=8` and the riders were clustered along a line, so their shortlists overlapped heavily — the
documented sparse tradeoff from Week 3, visible in production shape.)

## The bug that only running it could find

The **first batch after every batcher start timed out.**

`grpc.NewClient` dials lazily, so the first RPC pays TCP setup plus the HTTP/2 handshake — measured
at over a second on a loaded machine, against a `SolveTimeout` of window/2. The solve itself takes
**6 ms**.

```
batch 1: solve_ms=1000  -> DEADLINE_EXCEEDED, requeued
batch 2: solve_ms=6     -> matched
```

Every batcher restart would have lost its first batch to a full reclaim cycle (60s+). Unit tests
never caught it because their engine connection was already warm from setup.

Fixed by probing `Health` at startup, which both warms the connection and fails fast on a bad
address. **Some bugs are only visible in the shape of a real deployment.**

## Files touched

`internal/batcher/{batcher,batcher_test}.go`, `cmd/batcherd/main.go`, `cmd/requestd/main.go`.
