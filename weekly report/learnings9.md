# Learnings — Week 9 (Batching, Backpressure, Graceful Shutdown)

The systems-thinking week. Concepts and interview-ready takeaways.
Report: [week9.md](week9.md)

## 1. Coalescing: when dropping data is CORRECT

The most valuable idea this week.

A driver pings every ~3 seconds and only their **latest** position matters. An older fix is not
partial information — it is *wrong* information that a newer one has already superseded.

```go
buffer map[string]DriverLocation   // keyed by driver: a new ping overwrites, never queues
```

Ten pings in a window become one write, and nothing of value is lost. Measured: **5,438 pings →
2,000 writes.**

**The general rule:** for **state** (where is this driver?), only the latest matters, so coalesce.
For **events** (this rider requested a ride), every one matters, so queue and never drop. Getting
these backwards is a serious design error in either direction.

**Interview soundbite:** "GPS pings are state, not events. Keying the buffer by driver id gives
coalescing for free and turns an unbounded queue into a bounded map."

## 2. Backpressure: you are always choosing what to drop

Under overload the choice is never "drop nothing". It is:

1. **drop something you chose**, or
2. **drop everything, when the process OOMs.**

An unbounded queue is not "no backpressure" — it is deferred, catastrophic backpressure.

**The options, and when each is right:**

| Strategy | Right for |
|---|---|
| **Reject new** (503, `Retry-After`) | connections; lets clients back off |
| **Drop oldest** | state where newest wins |
| **Coalesce** | state keyed by an entity — best of all when available |
| **Block the producer** | when the producer *can* slow down (it can't here: phones keep moving) |

**The asymmetry that makes my design work:** new drivers past the cap are shed, but **updates to
already-buffered drivers are always accepted** — they reuse an entry, cost no memory, and refusing
them would discard fresher data for zero saving. Memory is bounded by driver count; freshness is
never sacrificed.

**Always count what you shed.** Silent shedding is indistinguishable from a bug.

## 3. Don't retry into the next window

When a flush fails, the window is **dropped**, not carried forward.

Re-queueing would grow the buffer during exactly the outage that caused the failure — the classic
retry-amplification death spiral. And the data is superseded within seconds anyway, because every
driver pings again.

**Losing a window of positions during a Redis outage is cheap. OOMing the ingestion layer is not.**

## 4. Never hold a lock across I/O

```go
mu.Lock()
batch := buffer
buffer = make(map[string]DriverLocation)  // swap
mu.Unlock()

store.UpsertMany(ctx, batch)              // I/O outside the lock
```

Holding the lock across a Redis round trip blocks **every connection goroutine** for its duration —
one slow dependency stalls the entire ingestion layer. The swap-and-release pattern is the standard
fix, and it's worth having a test that asserts it (`Accept` must stay fast while a slow flush is in
flight).

**Interview soundbite:** "Lock to swap a pointer, not to do the work."

## 5. Refuse impossible configuration at construction

`FlushTimeout > Window` is rejected outright: a flush slower than the window means windows overlap
and queue, which is the unbounded growth the component exists to prevent.

**Validate invariants where they're cheap to check.** A startup error is infinitely better than a
slow leak nobody attributes to config for three days.

## 6. Graceful shutdown has an ORDER

1. Stop accepting new connections.
2. Close existing ones and **wait** for their goroutines.
3. **Then** flush the final window.

Flushing before draining loses whatever arrives during the drain. Skipping the final flush loses
the last 3 seconds of driver positions — a "clean" exit that silently discards data.

`signal.NotifyContext` gives one root context cancelled by SIGINT/SIGTERM, so every component stops
from a single source instead of each inventing its own shutdown path.

## 7. Make it runnable, and make it observable

`cmd/ingestd` + `cmd/mockdrivers` exist because the checkpoint isn't "the code compiles" — it's
"pings flow, Redis reflects them, and it stays bounded under overload". You cannot observe any of
that without traffic.

The `/stats` endpoint (connections, pings, shed, flushed, buffered, indexed) is what turns "it
seems fine" into evidence. **The most valuable number is a ratio:** pings-in vs writes-out shows
what coalescing buys; shed vs received shows how deep into overload you are.

## 8. Load generator details that matter

- **Stagger the first ping.** Otherwise every client fires on the same millisecond and you get a
  spike per interval, not a stream — not what a real fleet looks like.
- **Per-goroutine RNG.** Sharing `math/rand`'s global source serialises every goroutine on its
  internal lock, and you end up measuring that instead of the server.
- **Ramp connections.** A thundering herd of connects measures the accept queue, not the pipeline.
- **Count refusals separately from failures.** A 503 is the server *correctly* enforcing its limit
  — a successful test of backpressure, not a failure of the tool.

## 9. Never trust a client's clock

The ping carries `sent_at_ms` and it is **never** used for freshness. Phones have wrong clocks, and
a driver whose clock runs an hour fast would look permanently fresh and never age out of the
matching pool. The server stamps arrival time.

**General rule: client-supplied timestamps are data, not truth.**

## 10. The Go trap: constant vs runtime float arithmetic

```go
12.97 + 9*0.001          // untyped constants: arbitrary precision, rounded ONCE
12.97 + float64(9)*0.001 // float64: rounded at EVERY step
```

Different bits. **Identical when printed.** My test failed with
`lat = 12.979000, want 12.979000`, which is a genuinely disorienting message.

Go evaluates untyped constant expressions at arbitrary precision and converts once at the end.
Compare floats with a tolerance, or compute the expected value exactly the way the code does.

---

## Self-test

1. When is dropping data correct, and when is it a bug? What distinguishes the two cases?
2. Why is an unbounded queue not "no backpressure"?
3. Name three shedding strategies and when each is appropriate.
4. Why are updates to already-buffered drivers accepted even when the buffer is full?
5. Why drop a failed flush instead of retrying it in the next window?
6. Why must the lock be released before the store write, and how would you test that it is?
7. What is the correct ORDER of operations in a graceful shutdown, and what breaks at each step if
   you get it wrong?
8. Why should you never use a client's timestamp for freshness?
9. Your test prints `want 12.979000, got 12.979000` and fails. What's going on?
