# Architecture & Data Flow

**Status:** Draft · **Related:** [Technical_Design_Document.md](Technical_Design_Document.md), [Data_Model.md](Data_Model.md)

Diagrams use [Mermaid](https://mermaid.js.org/), which GitHub and most IDEs render natively.

---

## 1. System Context (C4 Level 1)

```mermaid
flowchart LR
    client["Fleet Operator's App\n(driver GPS + rider requests)"]
    subgraph engine["Ride-Matching Engine"]
      go["Go Service Layer\n(I/O, concurrency)"]
      cpp["C++ Core\n(spatial + graph math)"]
    end
    redis[("Redis\n(GEO store)")]
    queue[("Kafka / Redis Streams\n(ride requests)")]
    db[("Relational DB\n(tenants, API keys)")]

    client -->|WebSocket GPS pings| go
    client -->|REST ride requests| go
    go <-->|gRPC batches| cpp
    go <--> redis
    go <--> queue
    go <--> db
    client <-->|match results| go
```

## 2. Component View (C4 Level 2)

```mermaid
flowchart TB
    subgraph GoLayer["Go Service Layer"]
      ws["WebSocket Server\n(driver location ingest)"]
      api["REST API\n(ride requests)"]
      batcher["Match Batcher\n(3s windows)"]
      bridge["C++ Bridge Client\n(gRPC)"]
    end
    subgraph CppCore["C++ Core Engine"]
      qt["Quadtree\n(spatial index) ✅"]
      router["Router\n(Dijkstra / A*)"]
      matcher["Bipartite Matcher\n(MCMF) ✅"]
    end
    redis[("Redis GEO")]
    queue[("Message Queue")]
    outcomes[("Results Store\n(outcomes, 24h TTL)")]

    ws --> redis
    api --> queue
    api --> outcomes
    batcher --> outcomes
    batcher --> queue
    batcher --> bridge
    bridge -->|cost matrix request| qt
    qt --> router
    router --> matcher
    matcher -->|assignment| bridge
    bridge --> batcher
```

Legend: ✅ = implemented (Quadtree Week 2, MCMF matcher Week 3). Everything else is scheduled per the TDD.

The results store is written by **both** the API (`PENDING` on accept) and the
batcher (the terminal state), and read only by the API. It is the one piece of
state that outlives a request's presence in the queue — by design, since the
queue holds work and deletes it on completion, while an outcome is only useful
*after* the work is done.

## 3. The Critical Path: a ride request end-to-end

```mermaid
sequenceDiagram
    participant Rider as Client (Rider)
    participant API as Go REST API
    participant Q as Message Queue
    participant B as Match Batcher (Go)
    participant C as C++ Engine
    participant R as Redis GEO
    participant S as Results Store

    Rider->>API: POST /v1/ride-requests (tenant, pickup)
    API->>API: validate + authenticate (API key, tenant)
    API->>Q: enqueue RideRequest (durable)
    API->>S: record PENDING
    API-->>Rider: 202 Accepted (request id)

    loop every 3 seconds
        B->>Q: pop pending requests (this window)
        B->>R: GEORADIUS candidate drivers per pickup
        B->>C: gRPC: batch of riders + candidate drivers
        C->>C: build cost matrix (quadtree + router)
        C->>C: solve optimal assignment (Hungarian/MCMF)
        C-->>B: rider→driver matches (+ ETA)
        B->>S: record MATCHED / UNMATCHED
        B->>Q: ack processed requests
    end

    Rider->>API: GET /v1/ride-requests/{id}
    API->>S: read outcome (scoped to the authenticated tenant)
    API-->>Rider: PENDING, or MATCHED with the driver
```

Two things in that diagram are easy to skim past and are the whole design:

**The batcher never talks to the rider.** An earlier version of this diagram
drew `B-->>Rider: match result`, and that arrow was fiction — the batcher has
no connection to the client and never did. The rider polls. There is no push
channel in v1: no webhook, no rider socket. Anything drawn as a push here would
be a diagram describing software that does not exist.

**The write to the results store precedes the ack.** Ack-then-record loses the
match outright on a crash, because the ack is what deletes the request. 
Record-then-ack merely duplicates work: the request is redelivered, re-matched,
and the record overwritten. That ordering is why at-least-once delivery is safe
here rather than merely tolerable.

## 4. Failure & resiliency flow

```mermaid
sequenceDiagram
    participant B as Batcher (Go)
    participant Q as Message Queue
    participant C as C++ Worker

    B->>Q: pop batch (NOT yet acked)
    B->>C: gRPC solve
    C--xB: worker crashes (no response)
    Note over B: gRPC call times out (bounded)
    B->>Q: do NOT ack → messages redelivered
    Note over C: Kubernetes restarts the worker
    B->>Q: pop batch again (next window)
    B->>C: retry solve
    C-->>B: success → ack
```

This is the concrete realization of the **Fail-Safe Orchestration** tenet: requests are only acked *after* a successful match, so a crash re-processes rather than drops.

## 5. Key architectural boundaries

| Boundary | Contract | Why it matters |
|----------|----------|----------------|
| Client ↔ Go | [REST](api/rest-openapi.yaml) + WebSocket | public, versioned, must stay stable |
| Go ↔ C++ | [gRPC/proto](api/matching.proto) | hardest to change; defined before either side is coded |
| Go ↔ Redis/Queue/DB | repository interfaces | mockable, swappable in tests |

## 6. Scaling model

- **Go services are stateless** → scale horizontally behind a load balancer (HPA in K8s).
- **C++ workers are stateless per batch** → scale as a worker pool; a crash affects only its in-flight batch.
- **Redis / queue / DB** are the stateful tier → scaled independently, with the queue providing the durability guarantee.
