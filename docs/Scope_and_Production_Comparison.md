# Scope Boundary & Comparison to a Production System (e.g. Ola / Uber)

**Status:** Reference · **Related:** [Product_Requirements.md](Product_Requirements.md), [Technical_Design_Document.md](Technical_Design_Document.md)

This document sets an honest boundary: what this 24-week project **will** be by the end, and where it deliberately stops short of a production ride-hailing backend. It exists so the scope is explicit rather than implied — and so the project isn't judged against a bar it never aimed for.

> **One-line verdict:** By Week 24 this is a **real, runnable, benchmarked, single-region ride-matching engine with a production-shaped service skeleton** — architecturally faithful to a system like Ola/Uber and demonstrating the same core techniques — but **not** the ML ranking, live-traffic ETA, dispatch negotiation, surge pricing, or true multi-region internet scale.

---

## 1. Why the comparison

Production ride-matching engines (Ola, Uber, Lyft) are proprietary, but their public papers, patents, and talks reveal a common shape. This project intentionally rebuilds the **algorithmic heart** of that shape — the part those companies themselves call the hardest problem: *finding nearby drivers fast, then assigning them optimally* — while stubbing or omitting the surrounding concerns that require teams, historical data, or years.

## 2. Component-by-component mapping

| Production component | This project | Coverage |
|----------------------|-------------|----------|
| Geospatial indexing (H3 / S2 / Geohash / QuadTree) | Week 2 QuadTree + Week 7 Redis GEO | ✅ Full — same technique (hexagonal cells vs. quad cells; same idea) |
| Real-time location streaming (Kafka → Redis) | Weeks 7–10 (WebSockets, Redis, Kafka/Streams) | ✅ Full |
| Candidate search + remove busy/offline | Week 2 query + Week 7 TTL staleness | ✅ Full |
| Optimal assignment (Hungarian / MCMF / auction) | Week 3 Hungarian / MCMF | ✅ Full — the marketplace-balancing math |
| ETA via road graph + shortest path | Week 4 Dijkstra / A\* | 🟡 Partial — real road graph, **no live traffic prediction** |
| Event-driven architecture | Weeks 9, 10, 12 | ✅ Full |
| Observability (metrics / logs / tracing) | Weeks 20, 23 | ✅ Full |
| Resiliency / zero dropped requests | Week 10 queue + Week 16 K8s | ✅ Full |
| Multi-tenancy (data isolation) | Week 19 | ✅ Full — a *bonus* beyond a single-brand app |
| **Ranking function (learned, multi-factor)** | — | ❌ Gap — we rank by distance/ETA cost, not a learned score |
| **Dispatch / offer protocol (offer→accept→next)** | — | ❌ Gap — we produce assignments, not a negotiation loop |
| **Surge / dynamic pricing** | Deferred (v1 non-goal) | ❌ Gap (intentional) |
| **True internet scale (millions/min, multi-region)** | Weeks 15/21 simulate ~10k | 🟡 Partial — algorithms proven to scale; not run at production volume |
| **Full microservice suite (auth, payment, notifications, analytics)** | Matching-relevant services only | 🟡 Partial |

## 3. What you WILL have at Week 24

A working system that:
- indexes drivers spatially and finds candidates in ~O(log n);
- ingests live GPS over WebSockets / Kafka into Redis;
- computes **globally optimal** rider→driver assignments (not greedy);
- costs them with real road-graph routing (A\* / Dijkstra);
- runs event-driven, resilient (no dropped requests), multi-tenant, observable, and containerized on Kubernetes;
- is **benchmarked** with committed p99 numbers proving the latency and complexity claims.

This is architecturally faithful to a production diagram — the same DNA — at a scale you can run and prove on your own hardware.

## 4. What you will NOT have — and why that's the correct call

| Omission | Why it's out of scope for a solo 24-week build |
|----------|-----------------------------------------------|
| **ML ranking / RL dispatch** | Needs historical trip data you don't have + a full ML pipeline. It's a *separate project*, not a feature. |
| **Live traffic-aware ETA** | Requires real-time traffic feeds and prediction models; static-graph A\* is the honest, buildable version. |
| **Dispatch/offer negotiation** (15s offer, reject, next driver) | A distinct real-time protocol layered on top of assignment; assignment is the prerequisite and the harder algorithmic piece. |
| **Surge / dynamic pricing** | Reserved as a fast-follow that reuses the same cost matrices; excluded from v1 to protect focus. |
| **True multi-region internet scale** | Millions/min across regions with 11+ services is a team-years effort; simulated load (10k) proves the algorithms without that infra. |

Attempting all of the above solo would guarantee finishing none of it. The plan trades breadth for a **complete, correct, provable core** — the right trade for a learning/portfolio artifact.

## 5. Natural extensions beyond v1 (if the project continues)
In rough priority order, the most valuable next steps toward "production-like":
1. **Dispatch/offer protocol** on top of the assignment output (highest realism-per-effort).
2. **Surge/pricing service** reusing the batch cost matrices.
3. **Multi-factor ranking** (start rules-based: distance + rating + idle time), then learned weights once data exists.
4. **Traffic-aware ETA** with a time-dependent road graph.
5. **Horizontal/multi-region scale-out** with real (not simulated) traffic.
