# ADR-0006: Redis Streams (not Kafka) for the ride-request queue

## Status
🟡 Proposed — decided before Week 10; to be validated when the queue is built.

## Context

The "Fail-Safe Orchestration" tenet says a C++ worker crash must not lose ride requests. Week 6 made a crash *survivable* (the Go process stays up and gets a retryable error), but survival is not durability: a request held only in Go's memory when the process dies is gone.

So ride requests need a durable queue between the REST front door (Week 11) and the Match Batcher (Week 12), with:

- **at-least-once delivery** — an un-acked message must be redelivered after a consumer crash;
- **consumer groups** — multiple batcher instances sharing one stream without duplicating work;
- **a dead-letter path** — a message that fails repeatedly must be moved aside rather than blocking the queue forever.

Note the asymmetry with driver locations (ADR-0004): **GPS pings are state and may be coalesced or dropped; ride requests are events and may not.** Losing a driver's position for one window is invisible — they ping again in 3 seconds. Losing a ride request means a customer standing on a street corner who was never told no.

## Options considered

1. **Kafka.** The default answer for durable event streaming. Partitioned log with retention, so consumers can replay from an offset; enormous throughput; strong ordering guarantees per partition; mature consumer-group semantics.
   *Cost:* a broker to run, configure, monitor, and deploy (plus ZooKeeper, or KRaft mode). That is a second stateful system in every environment — local, CI, and production — for a project whose entire persistence story is currently one Redis.

2. **Redis Streams.** An append-only log with `XADD`, consumer groups via `XREADGROUP`, explicit acknowledgement with `XACK`, and — the part that matters — `XPENDING`/`XCLAIM` for finding and reclaiming messages a dead consumer never acked.
   *Cost:* weaker retention story than Kafka (a stream is capped by length or age, not a durable log you replay from arbitrary offsets), and throughput ceilings far lower than Kafka's, though far above anything this system will produce for a long time.

3. **A database-backed outbox table.** Durable and transactional, and genuinely the right answer when the queue write must be atomic with a business-table write. But it means polling, a second datastore, and hand-building the consumer-group and redelivery semantics the other two give for free.

## Decision

Use **Redis Streams**, accessed through a **`queue.Queue` interface** rather than directly.

The interface is not hedging or speculative generality — it is the same discipline as `locations.Repository` (ADR-0004), and it earns its keep immediately: the Week 12 batcher can be tested against an in-memory fake with no Redis at all, exactly as the Week 9 pipeline was.

## Rationale

- **Redis is already a hard dependency.** Adding Kafka means a second stateful system in every environment. Adding a stream to Redis means one more key. Operational surface is a real, recurring cost, and it is paid in every later week (Docker Compose in Week 14, Kubernetes in Week 16, CI in Week 17).
- **Redis Streams provides exactly the semantics the requirement names.** Consumer groups, explicit acks, and redelivery of un-acked messages are the whole of Week 10's definition of done. Kafka would provide those same semantics *plus* capabilities nothing in this system currently uses.
- **Throughput is not the binding constraint.** Redis Streams handles tens of thousands of messages per second on modest hardware. The engine's own limit is a 3-second batch window carrying at most a few thousand riders. We would hit product limits long before broker limits.
- **The honest counter-argument:** Kafka is the stronger signal on a CV, and "why not Kafka?" is a question worth being able to answer. This ADR is that answer. Choosing the smaller dependency you can fully justify is a better engineering signal than choosing the larger one by default.

## Consequences

- ➕ No new infrastructure. Local dev, CI, and production keep a single stateful dependency.
- ➕ Week 10's checkpoint (kill a consumer, confirm redelivery) is directly testable against a real Redis, consistent with how Weeks 7–9 are tested.
- ➕ The Week 6 error taxonomy (`Retryable(err)`) already encodes the dead-letter routing rule — retryable failures go back on the stream, poison goes aside.
- ➖ **Redis is in-memory.** Durability depends on the persistence configuration (AOF/RDB), and a Redis failure without AOF loses the queue. Kafka's on-disk log is genuinely stronger here. **Mitigation:** enable AOF with `appendfsync everysec` in any environment that matters, and treat this as a known limit rather than a solved problem. Revisit if the durability requirement ever hardens to "must survive a Redis node loss with zero message loss".
- ➖ **No arbitrary replay.** A capped stream is not a retained log; you cannot rewind a consumer group to last Tuesday. Nothing needs that today.
- ➖ **Streams grow unless trimmed.** `XADD` with `MAXLEN ~` is mandatory, not optional — an untrimmed stream is a slow-motion OOM, the same class of bug as the Week 9 unbounded buffer.
- ➖ Migrating to Kafka later means writing a second `queue.Queue` implementation. The interface bounds that work to one package, but the *semantic* differences (partition ordering, offset management) would still need thought.

## Revisit if

- durability must survive losing a Redis node with zero loss;
- another team or service needs to consume the same ride-request stream (Kafka's fan-out and retention are much better suited);
- sustained throughput approaches the tens-of-thousands-per-second range.
