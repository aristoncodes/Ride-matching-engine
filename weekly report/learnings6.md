# Learnings — Week 6 (gRPC, Protobuf, Process Isolation)

The polyglot-bridge week. Concepts and interview-ready takeaways.
Report: [week6.md](week6.md)

## 1. cgo vs gRPC — the answer is about blast radius, not latency

Everyone reaches for latency first. The real argument is **failure isolation**.

- **cgo** puts C++ in your address space. A segfault kills the *Go process*. It also complicates
  the build, confuses the scheduler (a cgo call occupies an OS thread), and breaks profiling.
- **gRPC to a separate process**: a crash is an `Unavailable` error. Go stays up, doesn't ack the
  batch, and it gets redelivered.

**Interview soundbite:** "cgo welds the two lifetimes together. If your resiliency requirement is
'a crash in the C++ tier must not lose requests', cgo can't satisfy it at any level of care — so
the decision is made for you before latency enters the conversation."

**When cgo IS right:** microsecond-scale calls where serialisation dominates, and the native code
is trusted and mature (e.g. SQLite). Know both sides.

## 2. Amortising the boundary

Per-call overhead only matters relative to the work per call. This engine batches into 3-second
windows, so one round trip carries hundreds of riders and the serialisation cost disappears into a
solve that already takes milliseconds.

**The general principle: make the unit of work bigger than the unit of overhead.**

## 3. Protobuf field design — the mistakes are semantic, not syntactic

**A field whose *unit* depends on another field is a bug waiting to happen.** My original
`double cost` meant metres under one metric and seconds under another. Both sides compile, both
sides run, and the number is wrong. Fixed by splitting into separately typed fields.

**`optional` in proto3 gives you presence.** Without it, a missing `int32` and a real `0` are
indistinguishable. `eta_seconds` must be absent under euclidean pricing — otherwise a caller reads
0 as "arrives immediately".

**Return *reasons*, not just outcomes.** `repeated string unmatched_rider_ids` told the caller
*that* a rider was unmatched. What it needed was *why* — "no drivers left" is worth retrying next
window, "unreachable" never will be. The server already knew; the contract threw it away.

**Echo back what actually happened.** The request states intent; the response should state outcome
(`cost_metric_used`, effective `k`). A caller tracking an SLO against its own assumption is
measuring nothing.

## 4. Wire compatibility rules (know these cold)

Two processes are never upgraded at the same instant.

- **Never reuse a field tag.** Deleting means `reserved`, not silence. A reused tag makes an old
  peer's bytes decode as a *different field* — silent corruption, not a parse error.
- Adding an optional field is safe. Adding required *behaviour* is not: old clients won't send it.
  New semantics need a new field with a safe default.
- Adding an enum value is safe only if receivers treat unknown values as UNSPECIFIED.
- Breaking changes get a new package (`v2`) served **alongside** v1, never an edit in place.

## 5. gRPC status codes are an API, not decoration

The codes are the contract for what the caller should *do*:

| Code | Meaning | Caller |
|---|---|---|
| `INVALID_ARGUMENT` | malformed request | fix it; never retry |
| `FAILED_PRECONDITION` | system state wrong (graph not loaded) | needs an operator |
| `RESOURCE_EXHAUSTED` | too big / rate limited | split or back off |
| `UNAVAILABLE` | server down or restarting | **retry** |
| `DEADLINE_EXCEEDED` | too slow | retry if idempotent |

**Interview soundbite:** "I map status codes to a retry-vs-poison decision once, at the boundary,
instead of re-deriving it — differently and eventually wrongly — at every call site."

## 6. Deadline ≠ cancellation

Both are needed, and they solve different problems:

- **Deadline** — an upper bound so a hung server can't pin your goroutine forever.
- **Cancellation** — the *caller* abandoning work it no longer needs, without waiting the deadline
  out.

`context.WithTimeout` never *extends* an existing deadline, so a caller's earlier deadline always
wins. That composition is why you can safely impose a default at a library boundary.

**Size deadlines against the business cycle, not the code.** 2 s here because a call that misses
its 3 s batch window is worthless — not because the solve takes 0.4 ms.

## 7. Statelessness buys you concurrency for free

`MatchingService` has no mutable state; the graph registry is immutable after startup. So gRPC's
thread pool can call it concurrently with **no locks at all**.

**Immutability is a stronger and cheaper guarantee than a mutex**, and it is also what lets the
C++ tier scale as an anonymous worker pool — any instance can serve any request.

## 8. Load expensive things at startup, and fail loudly

Road graphs take ~130 ms to parse. Loading on demand would blow a batch deadline; loading lazily
would make the first request after every restart slow.

And if a graph fails to load, **exit**. A server that boots without what it needs accepts traffic
and rejects it one request at a time — an outage disguised as a high error rate, which is far
harder to diagnose than a refusal to start.

**Health must mean readiness.** "Process up" ≠ "can serve travel-time batches". `Health` reports
which graphs are loaded so a readiness probe can tell the difference.

## 9. Test the boundary with the real thing

The claim was "a C++ crash becomes an error value". A mock returning `codes.Unavailable` proves
only that the mock was written to. So the test starts the **real binary** and **SIGKILLs** it.

And it asserts **recovery**, not just failure. gRPC reconnects underneath; no service-layer code
needs to know a crash happened. **Isolation that requires restarting your process to recover is not
isolation.**

**Test-infrastructure tricks worth stealing:** bind port 0 and let the OS choose (hardcoded ports
break parallel tests); wait for the server's readiness *line* rather than sleeping (a fixed sleep is
a race that goes red in CI); `t.Skip` when the binary isn't built (a test that *can't* run is not a
test that *failed*).

---

## Self-test

1. Give the argument for gRPC over cgo without mentioning latency. When is cgo the right call?
2. Why is a `double cost` field whose unit depends on an enum dangerous?
3. What does `optional` buy you in proto3, and where does it matter here?
4. What happens if you reuse a deleted field's tag number?
5. Map `FAILED_PRECONDITION`, `UNAVAILABLE`, and `INVALID_ARGUMENT` to retry decisions.
6. Deadline vs cancellation — why do you need both?
7. Why does the service need no mutex despite serving concurrent requests?
8. Your health check returns 200 but every travel-time request fails. What did you get wrong?
9. How would you prove a crash in a C++ dependency can't take your Go service down?
