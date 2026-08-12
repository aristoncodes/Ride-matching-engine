# Learnings — Week 12 (Batching, Ack Semantics, Microservice Boundaries)

The integration week. Concepts and interview-ready takeaways.
Report: [week12.md](week12.md)

## 1. Dual-trigger batching (know why both halves)

```
flush when:  window elapsed  OR  max size reached
```

| Trigger alone | Failure |
|---|---|
| Time only | a spike builds an enormous batch → blows the solve budget and the memory bound |
| Size only | a quiet period leaves one rider waiting for company that never arrives |

Time bounds **latency**; size bounds **memory and compute**. They are not redundant.

**Record which one fired.** Consistently size-triggered means the window is too long for the load;
consistently time-triggered means there is spare capacity. That ratio is a tuning signal you get for
free.

## 2. Ack only after the work is done

**The single most important line in the package.**

Acking on receipt is simpler and silently drops every in-flight message on a crash. Acking after
means a crash causes *redelivery* — duplicate work, which is recoverable — instead of *loss*, which
is not.

**The general rule: acknowledge after the side effect, not before.** Then make the side effect
idempotent, because at-least-once guarantees you will sometimes do it twice.

## 3. "Failed" and "not yet succeeded" are different

An **unmatched** rider is not an error and not a completed request. They are still waiting.

So the message is deliberately left **unacked**, and is retried in a later window when more drivers
may be on shift. Acking it would silently drop a customer who never got a car and never got a no.

**Ask of every terminal state: is this genuinely final, or merely not-yet?**

## 4. The error taxonomy pays off three weeks later

`Retryable(err)` from Week 6 is the whole decision procedure here:

| | Action |
|---|---|
| retryable (engine crashed/timed out) | leave unacked → redelivered |
| not retryable (malformed, missing graph, too big) | dead-letter → stops blocking the queue |

**Interview soundbite:** "I classify failures once at the boundary into retryable and poison. Every
consumer then has one rule instead of re-deriving it — differently — at each call site."

## 5. Deduplicate the fan-in

Each rider gets its own candidate query; the union is deduplicated by driver id. A driver near two
riders must appear **once**, or the solver believes there are two cars where there is one.

**Whenever you union per-item lookups into one batch, ask what happens to an item that appears
twice.**

## 6. Measure what the user experiences

Per batch: size, driver count, matched/unmatched, **match rate**, **queue-wait p50**, solve duration.

- **Match rate** says whether the system is doing its job — distinct from whether it is fast. It is
  easy to build something with beautiful latency that matches nobody.
- **Queue wait** is the rider-facing number. Solve time is 6 ms; the rider waits seconds. Optimising
  the 6 ms is not where the experience lives.
- **p50, not mean.** One reclaimed request from minutes ago drags an average and hides the typical
  case.

## 7. Microservice boundaries follow failure domains

`requestd` and `batcherd` are separate processes so that a matching outage does not become a
front-door outage: a rider can still submit while the engine restarts.

**Split where you want independent failure and independent scaling** — not by layer, and not because
splitting is fashionable.

## 8. The bug: lazy connections make the first request special

`grpc.NewClient` dials lazily. The first RPC pays TCP setup plus the HTTP/2 handshake.

```
batch 1: solve_ms=1000  -> DEADLINE_EXCEEDED
batch 2: solve_ms=6     -> fine
```

Every restart lost its first batch. Unit tests never caught it — their connection was warm from
setup.

**Warm connection pools at startup**, which also fails fast on a bad address. The same applies to
database pools, HTTP clients with connection reuse, and anything else that connects lazily.

**Interview soundbite:** "Anything lazily initialised makes your first request pathological. If you
have a timeout sized for steady state, the first request after every deploy violates it."

**And the broader lesson: some bugs are only visible in the shape of a real deployment.** No amount
of unit testing would have found this one.

---

## Self-test

1. Why do you need both a time trigger and a size trigger?
2. Why ack after processing rather than on receipt? What does that cost you?
3. A rider goes unmatched. Ack or not? Why?
4. How do you decide between requeueing a batch and dead-lettering it?
5. Two riders are near the same driver. What must the batch look like?
6. Which single metric tells you the system is doing its job, as opposed to being fast?
7. Why p50 rather than mean for queue wait?
8. Your first request after every deploy times out but everything else is fast. What would you check?
