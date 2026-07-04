# Architecture Decision Records (ADRs)

Each ADR captures one significant decision: the **context**, the **options** considered, the **decision**, and its **consequences**. They are immutable once accepted — if a decision changes, write a new ADR that supersedes the old one (don't edit history).

Format based on Michael Nygard's ADR template.

## When does something get an ADR?

An ADR is warranted only when **all three** are true. Most weekly tasks are plain implementation ("build a WebSocket server") and get **no** ADR.

1. **There was a real choice** between viable alternatives.
2. **It's expensive to reverse** once built into the architecture.
3. **The reasoning is non-obvious** — someone will later ask "why *that* way?"

Statuses: **🟡 Proposed** (decided in principle, not yet built/validated) → **✅ Accepted** (committed, ideally proven in code) → **⛔ Superseded by ADR-XXXX** (never edit a decision; replace it).

## Index

| # | Title | Status |
|---|-------|--------|
| [0001](0001-use-quadtree-for-spatial-indexing.md) | Use a Quadtree for spatial indexing | ✅ Accepted (built + verified, Week 2) |
| [0002](0002-grpc-over-cgo-for-go-cpp-bridge.md) | gRPC-over-process for the Go↔C++ bridge | 🟡 Proposed (Week 6) |
| [0003](0003-optimal-matching-over-greedy.md) | Optimal (Hungarian/MCMF) matching over greedy | 🟡 Proposed · open question (Week 3) |
| [0004](0004-redis-geo-for-live-locations.md) | Redis GEO for the live driver-location store | 🟡 Proposed (Week 7) |
| [0005](0005-batch-in-3s-windows.md) | Batch ride requests in 3-second windows | 🟡 Proposed (Week 12) |

Only **ADR-0001** is Accepted — it's the one decision actually built and validated. The rest are architectural intentions from the TDD, recorded now while the reasoning is fresh, to be promoted to Accepted (or revised) in the week they're implemented.

## Decisions still to be recorded (backlog)

These are genuine decisions (they pass the three-part test) whose context isn't ripe yet. Each will get an ADR **in the week it's actually decided** — writing them now would be guessing.

| Future ADR | Decision | Week |
|------------|----------|------|
| 0006 | Serialization format: Protobuf vs. FlatBuffers | 6 |
| 0007 | Routing algorithm: plain Dijkstra vs. A\* vs. contraction hierarchies | 4 |
| 0008 | Message broker: Kafka vs. Redis Streams | 10 |
| 0009 | Distributed locking: Redis Redlock vs. geohash-partitioned locks | 13 |
| 0010 | Multi-tenancy isolation model: shared DB + tenant_id vs. DB-per-tenant | 19 |

*(Numbers are reserved, not fixed — record them in decision order as they happen.)*

## Template

```
# ADR-NNNN: <title>
## Status
Proposed | Accepted | Superseded by ADR-XXXX
## Context
What forces are at play? What problem are we solving?
## Options considered
1. Option A — pros / cons
2. Option B — pros / cons
## Decision
What we chose and the key reason.
## Consequences
What becomes easier, what becomes harder, what we now must live with.
```
