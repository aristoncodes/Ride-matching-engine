# ADR-0004: Redis GEO for the live driver-location store

## Status
🟢 **Accepted** — built and validated in Week 7 (Aug 20, 2026), with one premise corrected (see below).

## Context
Drivers stream GPS pings continuously. We need a shared, fast store that (a) absorbs high-frequency location writes, (b) answers "drivers within radius R of this pickup" quickly to pre-filter candidates before the C++ solve, and (c) is reachable by the horizontally-scaled, stateless Go services. This is distinct from the in-engine quadtree (ADR-0001), which is built per batch inside C++.

## Options considered
1. **Query the C++ quadtree directly for live locations** — but the quadtree is an ephemeral, per-batch, in-process structure; it isn't a shared, durable, concurrently-writable store across Go instances.
2. **Relational DB with geo extensions (PostGIS)** — powerful and durable, but heavier write path; overkill for ephemeral, rapidly-churning location data.
3. **Redis with GEO commands (GEOADD/GEOSEARCH)** — in-memory, extremely fast reads/writes, native radius search, trivially shared across Go instances.

## Decision
Use **Redis GEO** as the live location store and candidate pre-filter. `GEORADIUS`/`GEOSEARCH` narrows the driver set per pickup; the narrowed set is what's sent to the C++ engine, which builds the precise cost matrix.

## Consequences
- ➕ Fast writes handle high-frequency pings; fast radius reads pre-filter candidates cheaply.
- ⚠️ **A premise of this ADR was wrong.** It originally claimed Redis "TTL support" would age out stale drivers automatically. **It does not.** A Redis geo set *is* a sorted set, and expiry in Redis is **per key, not per member** — `EXPIRE` on the geo key would drop every driver in the city at once.
  **What was actually built:** a companion sorted set `drivers:seen:<tenant>` whose score is each driver's last-ping timestamp, making freshness an O(log N) score range. Reads filter by it *and* a bounded background reaper deletes by it. Both are required: filtering alone leaves dead drivers resident forever, reaping alone serves stale drivers in the window between sweeps. This is asserted directly in `TestStaleDriversAreNotReturned`.
- ➕ Shared across all stateless Go instances.
- ➖ In-memory → not the durability tier; ride *requests* live in the message queue (ADR-0005), not Redis.
- ➖ Two spatial structures exist (Redis GEO for live filtering, C++ quadtree for precise per-batch math). This is intentional division of labor, but must be documented so it doesn't read as duplication.
- Access is wrapped behind a repository interface so it's mockable in tests. (Done: `locations.Repository`, defined before the Redis implementation, which is what let the Week 9 pipeline be tested with no Redis at all.)
- ➖ Redis is single-threaded, so every command must be bounded. The reaper deletes at most 10,000 entries per sweep; an unbounded reap after an outage would stall the server — the reaper causing the outage it exists to clean up after.
