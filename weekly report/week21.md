# Week 21 — Traffic Blasting (repeatable, or it isn't a measurement)

**Date:** Nov 26, 2026 · **Phase:** 6 (Enterprise Hardening) · **Status:** ✅ Complete

## What this week was about

Turning "I hammered it for a bit" into three named, rerunnable load shapes whose
results can be compared to last month's.

## Three profiles, three different questions

| Profile | Shape | What only IT can find |
|---|---|---|
| **steady** | nominal, sustained | the baseline; the only one where a latency number means much |
| **spike** | surge on idle, far above window capacity | whether backpressure is *exercised*, not merely present |
| **soak** | low rate, long duration | **leaks, unbounded growth, TTL and reaper bugs** |

**Soak is the one people skip, and the only one that finds a class of bug that
volume cannot.** A goroutine leak of one per connection is invisible in a
30-second run and fatal after a day. Same for a buffer that grows slowly, or a
reaper that never fires.

Each run snapshots goroutines, heap and GC cycles **before and after**, so slow
growth shows up as a diff rather than requiring someone to be watching.

## Why not wrk or Locust

The TDD suggested them, and I used the existing `cmd/loadtest` instead.

wrk speaks HTTP. This system's ingestion path is **WebSocket**, and the
interesting behaviour — connection limits, heartbeats, coalescing — lives there.
A tool that can only hit the REST endpoint would measure a different path than
the one under test, and would still need a second tool for the driver side.

`cmd/loadtest` already drives both, reports exact nearest-rank percentiles, and
shares the project's own client code. Adding a tool would have added a dependency
and *reduced* coverage.

## Results are written to disk with their context

```
# Load profile: steady
# 2026-11-26T14:22:11Z  host=Darwin/arm64 cpus=10
# drivers=2000 riders=3000 rate=300/s
```

A number without the machine it was measured on cannot be checked. The header is
what makes a comparison next month legitimate rather than misleading.

## Checkpoint

> ✅ Each profile is a rerunnable script with recorded results.

```bash
./scripts/loadprofile.sh steady
./scripts/loadprofile.sh spike
./scripts/loadprofile.sh soak
./scripts/loadprofile.sh all
```

Results land in `profiles/results/<profile>-<timestamp>.txt`.

## Files touched

`scripts/loadprofile.sh`.
