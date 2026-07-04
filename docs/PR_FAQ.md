# PR-FAQ: B2B Geospatial Ride-Matching & Dynamic Pricing Engine

> **Format note:** This is an Amazon-style "Working Backwards" document. The press release is written *as if the product has already launched*, to force clarity on the customer value before a line of production code is committed. The FAQ answers the hard questions a reviewer would raise.

---

## PRESS RELEASE (written from the future)

**FOR IMMEDIATE RELEASE**

### New Matching Engine Pairs 10,000 Riders and Drivers in Under a Millisecond, Cutting Wait Times for Fleet Operators

*A drop-in engine lets transportation companies match riders to drivers optimally at city scale — without the latency spikes that plague conventional backends.*

Today we announced the general availability of our **Geospatial Ride-Matching & Dynamic Pricing Engine**, a B2B platform that computes optimal rider-to-driver assignments for fleets of any size. Where a naive backend slows to a crawl when a stadium empties or a storm hits, our engine sustains **sub-millisecond matching decisions even under bursts of tens of thousands of simultaneous requests**.

Fleet operators — university shuttle services, corporate transport providers, and regional ride-hailing companies — previously faced an unpleasant trade-off: match *fast* but poorly (grab the first nearby driver), or match *well* but slowly (recompute a global optimum and watch latency balloon). Both cost them riders. Our engine removes the trade-off by isolating the heavy graph math in a highly optimized C++ core and feeding it batched, real-time geospatial data through a concurrent Go service layer.

"During peak events we used to see riders wait minutes for a match, and some just gave up," said a pilot operator. "Now assignments feel instant, and the matches are genuinely the closest available driver — not just *a* driver."

The engine supports **strict multi-tenancy**, so multiple institutional clients run on shared infrastructure with fully isolated data. It is delivered as a containerized, Kubernetes-ready stack with built-in resiliency: ride requests are never dropped, even if an individual worker crashes mid-computation.

The platform is available now for pilot onboarding.

---

## FAQ

### Customer FAQ

**Q: Who is this for?**
B2B transportation operators who need to match riders and drivers at scale: university/campus shuttles, corporate fleets, event transport, and regional ride-hailing companies. It is **not** a consumer app — there is no rider-facing mobile UI in scope.

**Q: What problem does it actually solve?**
Optimal matching in a dense area is an O(N × M) problem. Done naively in a garbage-collected language, latency spikes under load and riders churn. We make matching both *optimal* and *fast*.

**Q: What does "optimal" mean here?**
Minimum total rider-to-driver cost (distance or travel time) across the whole batch, with each driver assigned at most once — computed via the Hungarian Algorithm / Min-Cost Max-Flow, not a greedy first-come grab.

**Q: How is a client's data kept separate from another client's?**
Every request carries a tenant ID that is enforced at every layer — API auth, queue partitions, cache keys, and logs. Cross-tenant access is tested and denied. See [Threat_Model.md](Threat_Model.md).

**Q: What happens during a traffic spike?**
Requests are buffered in a durable message queue and matched in 3-second batches. If a worker crashes, un-acknowledged requests are redelivered on restart — no request is silently lost.

### Internal / Reviewer FAQ

**Q: Why a polyglot C++/Go architecture instead of one language?**
Right tool for the job: C++ for CPU-bound math (graphs, spatial indices), Go for I/O-bound concurrency (WebSockets, queues, APIs). See [adr/0002-grpc-over-cgo-for-go-cpp-bridge.md](adr/0002-grpc-over-cgo-for-go-cpp-bridge.md).

**Q: What is explicitly out of scope for v1?**
Consumer mobile apps, payment processing, real map-tile rendering, surge/dynamic *pricing* as a shipped feature (the pricing model is a later extension; v1 focuses on matching + routing), and ML-based demand prediction.

**Q: What is the single riskiest assumption?**
That the cross-language (Go↔C++) call overhead stays below our latency budget. Mitigated by batching and a binary serialization format. Tracked in the TDD risk register.

**Q: How do we know it works?**
Correctness is anchored by brute-force cross-checks on small inputs; performance is anchored by committed benchmarks (p99 latency vs. N). See [Test_Strategy.md](Test_Strategy.md) and [SLO_SLA.md](SLO_SLA.md).

**Q: What does success look like in numbers?**
See the success metrics in [Product_Requirements.md](Product_Requirements.md) and the formal targets in [SLO_SLA.md](SLO_SLA.md).
