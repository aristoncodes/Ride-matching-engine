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
| Field | Type | Notes |
|-------|------|-------|
| `id` | string | `M-<uuid>` |
| `tenant_id` | string | |
| `rider_id` | string | |
| `driver_id` | string | |
| `cost` | double | distance or travel-time cost used by the solver |
| `eta_seconds` | int | from the routing component |
| `matched_at` | timestamp | |

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

```
PENDING ──(batch window closes)──▶ BATCHED ──(solver runs)──┬──▶ MATCHED
   │                                                         └──▶ UNMATCHED ──(re-queue next window OR reject per policy)
   └──(rider cancels)──▶ CANCELLED
```

Driver: `ONLINE ──(assigned)──▶ EN_ROUTE ──(trip ends)──▶ ONLINE`; any state `──(no ping past TTL)──▶ OFFLINE`.

## 5. Storage mapping

| Data | Store | Rationale |
|------|-------|-----------|
| Live driver locations | Redis (GEO) | fast spatial reads/writes, TTL for staleness |
| Pending ride requests | Message queue (Kafka/Redis Streams) | durability, redelivery on crash |
| Tenant / API-key records | Relational DB | strong consistency, isolation |
| Per-batch compute state | In-memory (C++) | ephemeral, never persisted |

## 6. Open questions

- Do matches need long-term persistence (audit/billing), or are they fire-and-forget to the client? (affects whether Match needs a durable store)
- Geohash precision for the tenant/geo-partitioned locking in Week 13.
