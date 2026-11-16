# Data Model & Schema Design

**Status:** Draft · **Related:** [api/matching.proto](api/matching.proto), [api/rest-openapi.yaml](api/rest-openapi.yaml)

Defines the canonical entities that flow through the system, their fields, the ID strategy, and lifecycle states. This is the shared vocabulary every layer (C++ engine, Go services, Redis, queue) must agree on.

---

## 1. ID Strategy

A concrete decision to avoid the collision we already hit (riders and drivers both starting at `0`):

- Every entity ID is **globally unique and type-prefixed**: `R-<uuid>` for riders, `D-<uuid>` for drivers, `RQ-<uuid>` for ride requests, `M-<uuid>` for matches.
- Inside the C++ hot path, IDs are mapped to **dense integer indices** (`0..N-1`) for matrix math, with a lookup table back to the string ID. The prefixing lives at the boundary; the core stays integer-fast.
- Every entity carries a **`tenant_id`** — the multi-tenancy anchor. Nothing is queryable without it.

## 2. Core Entities

### Driver
| Field | Type | Notes |
|-------|------|-------|
| `id` | string | `D-<uuid>` |
| `tenant_id` | string | isolation key |
| `location` | `{lat, lng}` | last known position |
| `status` | enum | `ONLINE`, `EN_ROUTE`, `OFFLINE` |
| `updated_at` | timestamp | drives TTL / staleness |

### Rider
| Field | Type | Notes |
|-------|------|-------|
| `id` | string | `R-<uuid>` |
| `tenant_id` | string | isolation key |
| `pickup` | `{lat, lng}` | requested pickup point |
| `requested_at` | timestamp | |

### RideRequest (the unit of work through the queue)
| Field | Type | Notes |
|-------|------|-------|
| `id` | string | `RQ-<uuid>` |
| `tenant_id` | string | |
| `rider_id` | string | |
| `pickup` | `{lat, lng}` | |
| `status` | enum | `PENDING`, `BATCHED`, `MATCHED`, `UNMATCHED`, `CANCELLED` |
| `created_at` | timestamp | |

### Match (the result)

> **As built** (`internal/results`, Redis key `results:<tenant>:<request_id>`).
> The Week-1 draft below it is kept for the diff; three fields changed once the
> engine existed.

| Field | Type | Notes |
|-------|------|-------|
| `request_id` | string | the key, and the id the API returned to the client |
| `tenant_id` | string | **part of the Redis key**, not a field to compare after reading — see below |
| `status` | enum | `PENDING`, `MATCHED`, `UNMATCHED`, `FAILED` |
| `driver_id` | string | set only when `MATCHED` |
| `straight_line_meters` | double | always set on a match; makes the result auditable |
| `eta_seconds` | int, optional | **absent** under euclidean pricing, never 0 |
| `reason` | string | why a non-matched terminal state was reached |
| `attempts` | int | how many match windows this request went through |
| `updated_at` | timestamp | server clock at the last write |

Three corrections against the Week-1 draft, each forced by the implementation:

- **No `id`.** A `M-<uuid>` would be a second identifier for something the
  client already addresses by `request_id`, and it could not be used for the
  lookup the client actually performs.
- **No `cost`.** It was an untyped `double` whose unit depended on the cost
  metric, so a client could not interpret it without knowing which metric ran.
  `matching.proto` reserves the field name for exactly this reason;
  `straight_line_meters` replaces it with something dimensioned.
- **`eta_seconds` is optional.** Under euclidean pricing the engine has no
  basis for an ETA. A default 0 reads as "the car is already here".

The tenant being in the KEY rather than a field is the isolation boundary: a
cross-tenant read addresses a key that does not exist, so it cannot leak a
driver id or even confirm the request is real. A tenant *field*, compared after
reading, works until one handler forgets the comparison.

<details>
<summary>Week-1 draft, superseded</summary>

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | `M-<uuid>` |
| `tenant_id` | string | |
| `rider_id` | string | |
| `driver_id` | string | |
| `cost` | double | distance or travel-time cost used by the solver |
| `eta_seconds` | int | from the routing component |
| `matched_at` | timestamp | |

</details>

### Tenant
| Field | Type | Notes |
|-------|------|-------|
| `id` | string | |
| `name` | string | institutional client |
| `api_key_hash` | string | **hashed**, never raw |
| `status` | enum | `ACTIVE`, `SUSPENDED` |

## 3. Internal Compute Types (C++ core)

These are *not* persisted; they exist only inside the engine per batch.

- **`Point { int index; double x; double y; }`** — dense-indexed position (already implemented in the quadtree).
- **`CostMatrix`** — `N × M` (or sparse `N × k`) rider→driver costs, built from the quadtree + router.
- **`Assignment`** — `rider_index → driver_index` result from the Hungarian/MCMF solver.

## 4. Lifecycle: RideRequest state machine

**As built.** Only the states a client can actually observe:

```
PENDING ──(solver runs)──┬──▶ MATCHED     driver assigned; terminal
                         ├──▶ UNMATCHED   no car after MaxMatchAttempts windows; terminal
                         └──▶ FAILED      could not be processed at all; terminal
   │
   └──(no driver this window, attempts remain)──▶ republished, stays PENDING
```

Two states from the Week-1 draft are gone, and both removals are deliberate:

- **`BATCHED`** is real internally but lasts milliseconds and is never
  persisted. Advertising a state no client can catch is noise in a contract.
- **`CANCELLED`** was never built. Riders cannot cancel — see
  [Scope_and_Production_Comparison.md](Scope_and_Production_Comparison.md).
  Publishing an enum value the server can never send is a contract that lies.

`FAILED` is new, and the distinction from `UNMATCHED` is the point: UNMATCHED
means the system looked and there was no car, which is a legitimate answer to
give a customer. FAILED means the request was never properly considered, which
is an operational fault.

Driver: `ONLINE ──(assigned)──▶ EN_ROUTE ──(trip ends)──▶ ONLINE`; any state `──(no ping past TTL)──▶ OFFLINE`.

## 5. Storage mapping

| Data | Store | Rationale |
|------|-------|-----------|
| Live driver locations | Redis (GEO) | fast spatial reads/writes, TTL for staleness |
| Ride-request outcomes | Redis (HASH, 24h TTL) | a receipt the client polls for, not a trip history |
| Pending ride requests | Message queue (Kafka/Redis Streams) | durability, redelivery on crash |
| Tenant / API-key records | Relational DB | strong consistency, isolation |
| Per-batch compute state | In-memory (C++) | ephemeral, never persisted |

## 6. Open questions

- ~~Do matches need long-term persistence (audit/billing), or are they
  fire-and-forget to the client? (affects whether Match needs a durable
  store)~~ — **Resolved: neither.**

  "Fire-and-forget to the client" turned out to be incoherent, because there is
  no channel to forget them *to*: the batcher acked the request, which deleted
  the only record that the match had happened, and the rider had no way to ask.
  The endpoint documented in `rest-openapi.yaml` was unimplementable for as
  long as that was true.

  The answer is a **third option the question did not offer**: short-lived
  durable storage. A 24-hour TTL'd hash is long enough for a client to poll and
  bounded enough not to grow without limit in a Redis capped at 512 MB — where
  unbounded growth would evict the API keys and the ride queue alongside it.
  Long-term persistence stays unbuilt because nothing reads it: there is no
  billing or audit feature, and building storage for a consumer that does not
  exist is speculative work.

- Geohash precision for the tenant/geo-partitioned locking in Week 13.
