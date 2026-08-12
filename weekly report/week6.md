# Week 6 — The Go ↔ C++ Bridge (gRPC, not cgo)

**Date:** Aug 13, 2026 · **Phase:** 2 (Go Bridge & State Ingestion) · **Status:** ✅ Complete

## What this week was about

Phase 1 built a brain. This week gives it a nervous system: Go drives the C++ engine, and the
two must not be able to kill each other.

## The decision that shapes everything: gRPC over cgo

| | cgo | gRPC to a separate process |
|---|---|---|
| Call latency | lowest, no serialisation | one local round trip |
| **A C++ segfault** | **kills the Go process** | **returns an error** |
| Build | complicates the Go build, scheduler, profiling | two clean binaries |
| Scaling | tied to the Go process | C++ tier scales as its own pool |

The resiliency tenet says a C++ crash must not lose ride requests. cgo welds the two lifetimes
together, so that tenet is unachievable with it — no amount of careful coding makes a segfault
recoverable in-process. The serialisation cost is real but is amortised by the 3-second batch
window ([ADR-0005](../docs/adr/0005-batch-in-3s-windows.md)).

[ADR-0002](../docs/adr/0002-grpc-over-cgo-for-go-cpp-bridge.md) is now **Accepted** rather than
Proposed, because this week actually tested the claim.

## What was built

| File | Role |
|------|------|
| `matching_service.{h,cpp}` | The gRPC service: validate → build cost matrix → solve → translate. No algorithms. |
| `graph_registry.{h,cpp}` | Road graphs loaded at startup, immutable afterwards. |
| `matching_server.cpp` | Standalone server binary; `--port`, `--graph <id>=<path>`. |
| `internal/engine/client.go` | Bounded, classified Go client. |
| `internal/engine/errors.go` | The retry-vs-poison taxonomy. |
| `internal/testutil/engineproc.go` | Starts, kills, and restarts the real binary for tests. |
| `Makefile` | `make proto` generates C++ *and* Go stubs from one `.proto`. |

## Fixing the contract before writing the code

`matching.proto` was written in Week 0 and had drifted from what the engine does after Week 4
added routing. Revising it first cost nothing; revising it after both sides were generated would
have cost a migration.

- **`double cost` was a trap.** Its *unit* changed with `cost_metric` — metres for euclidean,
  seconds for travel time. A single field whose meaning depends on an enum is exactly how a
  cross-language bug produces a plausible, wrong answer. Replaced with typed fields:
  `optional eta_seconds`, always-set `straight_line_meters`, `optional road_distance_meters`.
- **`optional` earns its keep.** `eta_seconds` is absent under euclidean because the engine has no
  basis for an ETA. Without presence semantics a caller cannot tell "no ETA" from "arrives now".
- **`unmatched_rider_ids` → `UnmatchedRider` + reason.** The solver already knows *why* a rider
  went unmatched; the old contract discarded it and made the caller guess. The batcher's
  re-queue-vs-reject decision depends entirely on that distinction.
- **`reserved` on the two removed tags.** Reusing a tag makes an old peer's bytes decode as a
  different field — silent corruption, not a parse error.

## Statelessness is what makes it simple

`MatchingService` holds no mutable state. The graph registry is populated before the server
starts serving and never mutated, so concurrent `SolveBatch` calls across gRPC's thread pool need
**no lock anywhere**. Immutability is a cheaper and more reliable guarantee than a mutex, and it
is also what lets the C++ tier scale as an anonymous worker pool.

Graphs load at **startup**, not on demand: a ~130 ms parse inside a request would blow the batch
deadline, and a server told to load a graph it cannot find **exits** rather than booting and
rejecting traffic one request at a time — an outage disguised as a high error rate is much harder
to diagnose than a refusal to start.

## Bounding every call

```go
ctx, cancel := context.WithTimeout(ctx, c.timeout)  // default 2s
```

A deadline and cancellation are **different things**, and the code needs both:

- the **deadline** stops a hung engine from pinning a goroutine forever;
- **cancellation** lets a caller abandon work it no longer needs without waiting the deadline out.

The 2 s default is sized against the 3 s batch window, not against the engine's speed — a call
that has not returned in 2 s has already lost its window. Against a ~0.4 ms solve it is a runaway
detector, not a performance budget.

## The error taxonomy (the part the rest of Phase 2 is built on)

The only question the service layer ever asks about a failure is *retry, or poison?*

| Error | Cause | Retryable |
|---|---|---|
| `ErrEngineUnavailable` | crashed, restarting, unreachable | ✅ |
| `ErrTimeout` | did not answer in time | ✅ |
| `ErrInvalidBatch` | duplicate ids, bad coordinates | ❌ |
| `ErrGraphNotLoaded` | travel time without that graph | ❌ |
| `ErrBatchTooLarge` | above the server limit | ❌ |
| *unrecognised* | unknown | ✅ (safe default) |

Unknown defaults to retryable because `SolveBatch` is stateless and side-effect free, so dropping
a real ride request is worse than attempting it twice.

## Checkpoint

> ✅ Go sends a batch, gets the match array back, and survives a deliberately-killed C++ worker
> with a clean error.

`TestSurvivesEngineCrash` **SIGKILLs** the engine — no cleanup, no closing handshake, the way a
segfault actually arrives — and asserts:

1. the Go process is still alive and making decisions;
2. the failure is a typed error, not a panic or a hang;
3. the batch is **retryable**, so the request is not lost;
4. the client **recovers by itself** when the engine restarts.

(4) matters as much as the rest. Isolation that requires restarting the Go layer to recover is not
isolation.

## Tests

- **15 C++ service cases** pinning contract semantics: error codes, unmatched reasons, which
  fields are set under which metric, oversized batches, `FAILED_PRECONDITION` never silently
  degrading to euclidean.
- **9 Go integration tests** against the **real binary** — no mocks at the boundary. A mock that
  returns `codes.Unavailable` proves only that the mock was written to; the claim under test is
  about what happens when a real process really dies.

## Things I got wrong

- **My own arg validation rejected `--port 0`**, which is the idiom for "let the OS pick a free
  port" that the tests rely on to run in parallel. Fixed to allow it.
- **A test fixture put a driver at the exact coordinate of a rider**, so `straight_line_meters` was
  legitimately 0 and my `> 0` assertion failed. The fixture was wrong, not the code — fixed the
  fixture rather than weakening the assertion.
- **A nested scan for the matched edge** was O(riders × edges): fine at N=100, ~125 billion
  comparisons on the 5000-rider batch the server explicitly allows. Replaced with one pass.

## Deliberately deferred

`road_distance_meters` is left unset. Recovering it means routing every matched pair a second time
(~0.6 ms each, doubling batch cost) for a number the caller does not need in order to dispatch.
The field is `optional` precisely so that absent is a legal, readable answer.

## Files touched

`matching_service.{h,cpp}`, `graph_registry.{h,cpp}`, `matching_server.cpp`,
`tests/test_matching_service.cpp`, `CMakeLists.txt`, `docs/api/matching.proto`,
`internal/engine/{client,errors,client_test}.go`, `internal/testutil/engineproc.go`, `Makefile`,
`infrastructure/go.mod`.
