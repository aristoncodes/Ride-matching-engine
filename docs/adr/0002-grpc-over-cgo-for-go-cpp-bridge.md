# ADR-0002: gRPC-over-process for the Go↔C++ bridge

## Status
🟢 **Accepted** — built and validated in Week 6 (Aug 13, 2026).

**Validation:** `TestSurvivesEngineCrash` (`infrastructure/internal/engine/client_test.go`) SIGKILLs the running `matching_server` mid-session — no cleanup, no closing handshake, the way a segfault or an OOM kill actually arrives — and asserts that the Go process (1) stays up, (2) receives a typed `ErrEngineUnavailable` rather than a panic or a hang, (3) classifies the batch as **retryable** so the request is not lost, and (4) **recovers by itself** when the engine restarts, with no code in the service layer aware that a crash happened. Point (4) is the one that makes this real: isolation that requires restarting the Go layer to recover is not isolation.

## Context
The Go service layer must invoke the C++ engine for every batch. Two languages must exchange a batch of riders/drivers and get back an assignment. The "Fail-Safe Orchestration" tenet requires that a C++ crash must NOT lose ride requests or take down the ingestion layer.

## Options considered
1. **cgo (in-process FFI)** — lowest call latency, no serialization. But: a C++ segfault crashes the *whole Go process*, coupling their lifetimes and violating the resiliency tenet. cgo also complicates the Go build, scheduler interaction, and profiling.
2. **gRPC to a separate C++ process** — the C++ engine runs as its own service. A crash returns a gRPC error; Go stays up and simply doesn't ack the batch (so it's redelivered). Clean process isolation, language-agnostic contract, easy to scale the C++ tier as a pool.
3. **Shared memory / pipes with a custom protocol** — fast, but we'd be reinventing serialization, framing, and error handling that gRPC gives for free.

## Decision
Use **gRPC + Protobuf between Go and a standalone C++ process** (see [../api/matching.proto](../api/matching.proto)).

## Consequences
- ➕ Failure isolation: a C++ crash is an error, not an outage — directly satisfies the resiliency tenet. **Verified, not assumed** (see Status).
- ➕ The `.proto` is an explicit, versioned contract, definable before either side is coded.
- ➕ The C++ tier scales independently as a stateless worker pool.
- ➖ Per-call serialization + network overhead vs. cgo. **Mitigation:** batch requests into 3-second windows (ADR-0005) so cross-language calls are amortized; this is also logged as the top risk in the TDD.
- ➖ One more process to deploy and health-check (handled by the Kubernetes milestone).

## What building it actually taught us

- **Serialisation cost was not the interesting one.** The measured overhead of a local gRPC round trip is small against a batch that already takes milliseconds to solve, and it is amortised further by the 3-second window (ADR-0005). The cost that mattered was *operational*: a second binary to build, ship, and version, and a generated-code step that must stay in lockstep across two languages. That is why codegen is driven from a single `make proto` rather than committed twice.
- **The contract needed more than the happy path.** A `.proto` that only describes success is not a contract. The revision before implementation added typed cost units, explicit error-code semantics, and per-rider *reasons* for going unmatched — because the caller's real question is never "did it work" but "do I retry this".
- **Health has to mean readiness.** "The process is up" is not the same as "this server can price travel-time batches", since road graphs load at startup and take ~130 ms. `Health` reports its loaded graphs so a readiness probe can tell the difference, instead of traffic arriving and failing one request at a time.
