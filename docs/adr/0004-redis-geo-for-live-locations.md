# ADR-0004: Redis GEO for the live driver-location store

## Status
🟡 Proposed — decided in principle in the TDD; to be validated when the location store is built (Week 7).

## Context
Drivers stream GPS pings continuously. We need a shared, fast store that (a) absorbs high-frequency location writes, (b) answers "drivers within radius R of this pickup" quickly to pre-filter candidates before the C++ solve, and (c) is reachable by the horizontally-scaled, stateless Go services. This is distinct from the in-engine quadtree (ADR-0001), which is built per batch inside C++.

## Options considered
1. **Query the C++ quadtree directly for live locations** — but the quadtree is an ephemeral, per-batch, in-process structure; it isn't a shared, durable, concurrently-writable store across Go instances.
2. **Relational DB with geo extensions (PostGIS)** — powerful and durable, but heavier write path; overkill for ephemeral, rapidly-churning location data.
3. **Redis with GEO commands (GEOADD/GEOSEARCH)** — in-memory, extremely fast reads/writes, native radius search, TTL support to age out stale drivers, trivially shared across Go instances.

## Decision
Use **Redis GEO** as the live location store and candidate pre-filter. `GEORADIUS`/`GEOSEARCH` narrows the driver set per pickup; the narrowed set is what's sent to the C++ engine, which builds the precise cost matrix.

## Consequences
- ➕ Fast writes handle high-frequency pings; fast radius reads pre-filter candidates cheaply.
- ➕ TTLs let stale (non-pinging) drivers age out automatically.
- ➕ Shared across all stateless Go instances.
- ➖ In-memory → not the durability tier; ride *requests* live in the message queue (ADR-0005), not Redis.
- ➖ Two spatial structures exist (Redis GEO for live filtering, C++ quadtree for precise per-batch math). This is intentional division of labor, but must be documented so it doesn't read as duplication.
- Access is wrapped behind a repository interface so it's mockable in tests.
