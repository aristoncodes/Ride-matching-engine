# ADR-0002: gRPC-over-process for the Go↔C++ bridge

## Status
🟡 Proposed — decided in principle in the TDD; to be validated when the bridge is built (Week 6).

## Context
The Go service layer must invoke the C++ engine for every batch. Two languages must exchange a batch of riders/drivers and get back an assignment. The "Fail-Safe Orchestration" tenet requires that a C++ crash must NOT lose ride requests or take down the ingestion layer.

## Options considered
1. **cgo (in-process FFI)** — lowest call latency, no serialization. But: a C++ segfault crashes the *whole Go process*, coupling their lifetimes and violating the resiliency tenet. cgo also complicates the Go build, scheduler interaction, and profiling.
2. **gRPC to a separate C++ process** — the C++ engine runs as its own service. A crash returns a gRPC error; Go stays up and simply doesn't ack the batch (so it's redelivered). Clean process isolation, language-agnostic contract, easy to scale the C++ tier as a pool.
3. **Shared memory / pipes with a custom protocol** — fast, but we'd be reinventing serialization, framing, and error handling that gRPC gives for free.

## Decision
Use **gRPC + Protobuf between Go and a standalone C++ process** (see [../api/matching.proto](../api/matching.proto)).

## Consequences
- ➕ Failure isolation: a C++ crash is an error, not an outage — directly satisfies the resiliency tenet.
- ➕ The `.proto` is an explicit, versioned contract, definable before either side is coded.
- ➕ The C++ tier scales independently as a stateless worker pool.
- ➖ Per-call serialization + network overhead vs. cgo. **Mitigation:** batch requests into 3-second windows (ADR-0005) so cross-language calls are amortized; this is also logged as the top risk in the TDD.
- ➖ One more process to deploy and health-check (handled by the Kubernetes milestone).
