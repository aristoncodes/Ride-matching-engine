# Product Requirements Document (PRD)

**Product:** B2B Geospatial Ride-Matching & Dynamic Pricing Engine
**Owner:** Aditya Yadav
**Status:** Draft · **Related:** [PR_FAQ.md](PR_FAQ.md), [Technical_Design_Document.md](Technical_Design_Document.md)

---

## 1. Summary

A backend engine that matches riders to drivers optimally and quickly, delivered to institutional (B2B) clients as a multi-tenant, containerized service. It combines a C++ algorithmic core (spatial indexing + optimal bipartite matching + routing) with a Go service layer (real-time ingestion, queuing, APIs).

## 2. Problem Statement

Computing optimal driver-rider pairings in a dense area is O(N × M). Naive implementations in high-level languages spike in latency under load (e.g., an event letting out), causing long waits and rider churn. Operators are forced to choose between fast-but-poor matching and optimal-but-slow matching.

## 3. Personas

| Persona | Description | Primary need |
|---------|-------------|--------------|
| **Fleet Operator (buyer)** | Ops manager at a shuttle/transport company | Reliable, fast matching; proof of scale; simple integration |
| **Integrating Developer (user)** | Engineer at the client integrating our API | Clear, versioned API contracts; good docs; predictable errors |
| **Platform SRE (internal)** | Runs the engine in production | Observability, runbooks, safe rollouts, defined SLOs |

*Note: the end-rider is an indirect beneficiary, not a direct user of this system.*

## 4. Goals & Success Metrics

| Goal | Metric | Target (v1) |
|------|--------|-------------|
| Fast matching | p99 match latency for a batch | ≤ 1 ms compute at 10k drivers (see [SLO_SLA.md](SLO_SLA.md)) |
| Optimal matching | Total assignment cost vs. brute-force optimum | 0% gap on verified test cases |
| Scale | Concurrent drivers tracked | 10,000+ |
| Resiliency | Ride requests dropped during a worker crash | 0 |
| Tenancy | Cross-tenant data leaks | 0 (tested) |

## 5. User Stories

- As a **fleet operator**, I want the closest *available* driver assigned to each rider so wait times are minimized.
- As a **fleet operator**, I want no rider request lost during a demand spike so I never strand a customer.
- As an **integrating developer**, I want a versioned REST API with clear errors so integration is predictable.
- As an **integrating developer**, I want to stream live driver GPS locations over WebSockets so matches use fresh positions.
- As a **platform SRE**, I want the system to self-heal when a worker dies so I'm not paged for transient crashes.
- As a **compliance owner** at a client, I want a guarantee my data is isolated from other tenants.

## 6. Functional Requirements

1. Ingest live driver locations via WebSocket and store them in a spatial index.
2. Accept rider ride-requests via REST.
3. Batch requests in configurable (default 3s) windows.
4. Compute optimal rider→driver assignment per batch (each driver used once).
5. Compute travel-time-based cost using a routing algorithm (not just straight-line).
6. Return match results (rider id → driver id, cost/ETA) to the caller.
7. Enforce per-tenant isolation and API-key authentication.

## 7. Non-Functional Requirements

- **Performance:** sub-millisecond core matching (see SLO doc).
- **Availability:** target 99.9% for the ingestion/API layer.
- **Scalability:** horizontal scaling of the stateless Go services.
- **Security:** hashed API keys, per-tenant data segregation.
- **Operability:** metrics, structured logs, runbooks, safe rollback.

## 8. Scope & Non-Goals (v1)

**In scope:** matching, routing/ETA, ingestion, batching, queuing, multi-tenancy, containerized deployment, observability.

**Out of scope (v1):**
- Consumer-facing mobile/web apps.
- Payment processing / billing.
- Dynamic *pricing* as a shipped feature (the name reserves it; v1 ships matching + routing. Pricing is a fast-follow that reuses the same cost matrices).
- ML demand forecasting **and learned multi-factor ranking** (acceptance probability, driver rating, surge balance, cancellation probability). v1 ranks by distance/travel-time cost only.
- **Dispatch / offer negotiation protocol** (sequential or parallel offers, 15s accept windows, first-accept-wins). v1 produces optimal *assignments*; turning an assignment into an offer/accept loop is a separate layer.
- Live traffic-aware ETA and multi-region scale-out.
- Real map-tile rendering; we use a road graph for routing, not visuals.

> For a full, honest comparison against a production system (Ola/Uber) and the reasoning behind these boundaries, see [Scope_and_Production_Comparison.md](Scope_and_Production_Comparison.md).

## 9. Assumptions

- Clients can push driver GPS pings at least every few seconds.
- A small/real road graph is available for the routing component.
- Batch-window matching (3s) is acceptable latency for the business use case.

## 10. Open Questions

- What is the exact policy for unmatched riders when N > M — re-queue vs. reject? (decided per Week 3 deliverable)
- Is dynamic pricing v1.1 or v2?
- Which road-graph source (OSM extract vs. synthetic grid) for the routing milestone?
