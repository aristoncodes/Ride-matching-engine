# Week 24 — Documentation, and the project finished

**Date:** Dec 17, 2026 · **Phase:** 6 (Enterprise Hardening) · **Status:** ✅ Complete
**PROJECT COMPLETE — 24 of 24 weeks.**

## What this week was about

Making the repository readable by someone who has never seen it, and honest about
what it does and does not do.

## The README

Three things a newcomer needs, in this order:

1. **An architecture diagram** that shows the actual data flow — riders in via
   REST, drivers in via WebSocket, the durable queue between them, the C++ engine
   behind gRPC.
2. **A quickstart that works from `git clone`.** Prerequisite: Docker. Nothing
   else — the C++ toolchain, gRPC, protoc and Go all live inside the build images.
3. **Benchmarks with real numbers**, leading with ratios and stating the machine,
   because an absolute number from an unknown machine cannot be checked.

### One deliberate choice in the quickstart

It walks through the case where **nothing matches yet**, because that is what
actually happens on a first run: you submit a request, there are no drivers, and
nothing visibly occurs. Skipping that step would leave a newcomer thinking the
system is broken thirty seconds in.

### "Where the claims turned out to be wrong" is a section, not a footnote

The TDD claimed O(N log M). It is confirmed in M and **refuted in N**. That is in
the README, above the fold, alongside what it changed operationally.

A repository that only documents its successes is a sales page. The corrections
are the part that shows the work was actually done.

## What I did not do

Three gaps carried from earlier weeks, recorded rather than quietly closed:

- **Branch protection is not enabled.** CI gates report but do not literally
  block a merge — that is a repository *setting*, not a file.
- **WebSocket resume is implicit.** State survives a reconnect because positions
  live in Redis under a TTL, but there is no session token or explicit resume
  protocol.
- **Rider cancellations and driver rejections** were named in Week 19's
  one-liner. They are product state transitions rather than isolation, and are
  not built.

Each is in the TDD at the week that owns it.

## The project, end to end

| Phase | Weeks | What it produced |
|---|---|---|
| 1 · C++ core | 1–5 | quadtree, optimal MCMF matching, A* on a real road graph, Catch2 suite |
| 2 · Go bridge | 6–9 | gRPC bridge, Redis locations, WebSocket ingestion, 3s pipeline |
| 3 · Brokering | 10–13 | durable queue, REST front door, batcher, distributed locks |
| 4 · DevOps | 14–15 | Docker Compose, load testing |
| 5 · Orchestration | 16–19 | Kubernetes + chaos test, CI/CD, API keys, tenant isolation |
| 6 · Hardening | 20–24 | pprof, load profiles, optimisation, telemetry, docs |

**24 checkpoints, all met.** The ones worth remembering:

- **greedy = 101, optimal = 4** — why the solver exists
- **63% of pairings changed** when cost became road travel time
- **0 goroutines leaked** over 100 abrupt connect/disconnect cycles
- **200/200 requests survived** deleting every engine pod mid-traffic
- **exactly 1 winner of 100** goroutines racing for one driver
- **3/40 → 40/40** concurrent acquisitions with geohash partitioning
- **p99 8.35 ms** at 10,000 concurrent drivers

## The bugs that mattered

Every one was found by a test or a measurement, not by reading:

| Where | Bug |
|---|---|
| W2 | quadtree `remove()` lost points a node held before subdividing |
| W5 | static destruction order wrote an empty benchmark file |
| W8 | `Shutdown` hung — `ReadMessage` ignores context cancellation |
| W10 | `"BUSYGROUP"[:8]` — nine characters — broke every consumer after the first |
| W12 | lazy gRPC dial cost the batcher its first batch on every restart |
| **W16** | **unmatched riders dead-lettered as poison, at delivery 4 of 5** |
| W20 | a bool config whose zero value disabled the feature it documented |
| W22 | I inferred "too many round trips" from "time spent in Redis" |

The Week 16 one is the most important: two correct designs — a poison detector
and a retry-unmatched-riders policy — used the same signal, and the collision
silently discarded real customers. **No unit test could have found it.** It took
a real cluster, real load, and enough time for a counter to climb.

## Where the claims were wrong

Three, all corrected in place rather than quietly dropped:

1. **"O(N log M)"** — right about M (32× drivers → 2× time), wrong about N
   (32× riders → 100× time).
2. **ADR-0004's Redis TTLs** — Redis has no per-member expiry, which is why the
   companion ZSET exists.
3. **Week 22's pipelining** — a real improvement, but for a different reason than
   the profile led me to believe.

## Files touched

`Readme.md`, `docs/Technical_Design_Document.md`, `weekly report/*`.
