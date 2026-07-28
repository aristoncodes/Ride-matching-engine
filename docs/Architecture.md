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

    ws --> redis
    api --> queue
    batcher --> queue
    batcher --> bridge
    bridge -->|cost matrix request| qt
    qt --> router
    router --> matcher
    matcher -->|assignment| bridge
    bridge --> batcher
```

Legend: ✅ = implemented (Quadtree Week 2, MCMF matcher Week 3). Everything else is scheduled per the TDD.

## 3. The Critical Path: a ride request end-to-end

```mermaid
sequenceDiagram
    participant Rider as Client (Rider)
    participant API as Go REST API
    participant Q as Message Queue
    participant B as Match Batcher (Go)
    participant C as C++ Engine
    participant R as Redis GEO

    Rider->>API: POST /v1/ride-requests (tenant, pickup)
    API->>API: validate + authenticate (API key, tenant)
    API->>Q: enqueue RideRequest (durable)
    API-->>Rider: 202 Accepted (request id)

    loop every 3 seconds
        B->>Q: pop pending requests (this window)
        B->>R: GEORADIUS candidate drivers per pickup
        B->>C: gRPC: batch of riders + candidate drivers
        C->>C: build cost matrix (quadtree + router)
        C->>C: solve optimal assignment (Hungarian/MCMF)
        C-->>B: rider→driver matches (+ cost/ETA)
        B->>Q: ack processed requests
        B-->>Rider: match result (rider id → driver id)
    end
```

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
