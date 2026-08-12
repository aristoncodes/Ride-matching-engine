# Learnings — Week 10 (Message Queues, At-Least-Once, Dead Letters)

The durability week. Concepts and interview-ready takeaways.
Report: [week10.md](week10.md)

## 1. State vs events — the classification that drives everything

| | State | Events |
|---|---|---|
| Example | driver GPS position | ride request |
| Only the latest matters? | ✅ | ❌ |
| Coalesce? | ✅ | **never** |
| Shed under load? | ✅ | **never** |

Losing a driver's position for one window is invisible — they ping again in 3 seconds. Losing a ride
request means a customer on a street corner who was never told no.

**Interview soundbite:** "I classify every stream as state or events first. State can be coalesced
and shed, so it goes in a bounded buffer. Events must be durable, so they go on a queue. Getting it
backwards either wastes enormous capacity or loses customer intent."

## 2. Redis Streams vs Kafka (be able to argue both)

**Redis Streams:** consumer groups, explicit acks, `XPENDING`/`XCLAIM` redelivery, capped length.
No new infrastructure if you already run Redis.

**Kafka:** partitioned on-disk log with retention, arbitrary replay from an offset, far higher
throughput ceiling, mature ecosystem. Costs a broker in every environment.

**The real decision criterion is usually operational, not technical.** Adding a second stateful
system is paid again in local dev, CI, Compose, Kubernetes, and on-call.

**Interview soundbite:** "I chose Redis Streams because Redis was already a hard dependency and
Streams give exactly the semantics I needed. Kafka's advantages — retention, replay, partition
throughput — are real and nothing in the system used them. I put it behind an interface so a Kafka
implementation is one package."

## 3. At-least-once, and why exactly-once is a trap

Exactly-once across a process boundary is not available without distributed transactions. What is
available is **at-least-once + idempotent consumers**.

That is why every message carries a business id (`RequestID`) separate from the broker's id: the
broker id changes on redelivery, so only the business id can deduplicate.

**Interview soundbite:** "Exactly-once delivery is usually a misnomer for at-least-once delivery plus
idempotent processing. I make the idempotency key explicit and mandatory at publish time."

## 4. The mechanism: PEL, and the trap inside it

```
XADD                       -> append
XREADGROUP ... NOACK=false -> claim into this consumer's Pending Entries List
XACK                       -> remove from PEL
```

An unacked message stays in that consumer's PEL forever. If the consumer dies, the message is
**safely stored and delivered to nobody**.

**The trap:** `XREADGROUP` with `">"` returns only messages never delivered to *anyone*. A crashed
consumer's messages are invisible to normal consumption. Without `XPENDING`/`XCLAIM` running on a
schedule, "durable" means "lost, slowly".

**Durability without a reclaim path is not durability.**

## 5. `minIdle` is the line between "crashed" and "slow"

Reclaim only takes messages idle longer than `minIdle`. Too low and you steal from a live-but-slow
consumer, so two consumers process the same message **concurrently** — for a ride request, two
dispatched cars.

There is no way to know a process is dead. You only know it has not touched something recently.
`minIdle` is where you encode how long "recently" is.

## 6. Dead letters: the queue's escape valve

Without one, a poison message is retried forever, occupying a consumer slot and eventually stalling
the queue. Two distinct routes in:

- **Exceeded max deliveries** — it keeps coming back.
- **Undecodable payload** — it will fail identically every time, so retry immediately is pointless.

**Dead-lettering must ack in the same operation**, or the message is both set aside *and* still
pending.

**And ordering under partial failure matters:** write-aside first, then ack. If the ack fails you get
a harmless duplicate in the dead-letter stream. The reverse ordering could lose the message.

## 7. Where to start a consumer group

```
XGROUP CREATE ... "$"   -> start at the current end; SKIPS everything already waiting
XGROUP CREATE ... "0"   -> start at the beginning
```

`"$"` is a silent data-loss bug that only appears on a cold start. Worth a test.

## 8. Everything must be bounded

- `MAXLEN ~` on the stream — an untrimmed log is a slow-motion OOM. The `~` lets Redis trim on node
  boundaries, dramatically cheaper than exact trimming.
- Bounded reclaim per sweep — Redis is single-threaded, and an unbounded reclaim after an outage
  would stall it. The reaper causing the outage it cleans up after.

**Recurring theme across this whole project: every buffer, stream, and batch has an explicit bound.**

## 9. Unique consumer names are load-bearing

Two consumers sharing a name share a PEL, so each treats the other's in-flight work as abandoned and
reclaims it. Default it to `hostname-pid` and refuse to start without one.

## 10. The bug: an off-by-one in an error check

```go
err.Error()[:8] == "BUSYGROUP"   // "BUSYGROUP" is NINE characters
```

Compared `"BUSYGROU"`, never matched, so "group already exists" — the normal case for every process
after the first — became fatal. Invisible with one process.

**Use `strings.HasPrefix`, and be suspicious of any hand-sliced string comparison.**

---

## Self-test

1. How do you decide whether a stream may be coalesced or must be queued?
2. Argue for Redis Streams over Kafka. Now argue the reverse.
3. Why is exactly-once delivery usually the wrong thing to promise?
4. A consumer crashes after claiming. Walk through exactly how the message is recovered.
5. Why doesn't `XREADGROUP` with `">"` return a crashed consumer's messages?
6. What breaks if `minIdle` is too low? Too high?
7. Name two reasons to dead-letter, and why the ack must be part of it.
8. Why start a consumer group at `"0"` and not `"$"`?
9. Two instances share a consumer name. What goes wrong?
