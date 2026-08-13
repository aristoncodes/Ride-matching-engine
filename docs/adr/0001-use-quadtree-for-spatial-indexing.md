# ADR-0001: Use a Quadtree for spatial indexing

## Status
🟢 **Accepted** — built in Week 2, and its central claim has now been measured twice.

**Validation:**
- **Correctness vs brute force** (Weeks 2 and 5): `queryRange`, `nearestNeighbor` and `kNearest` are all checked against linear-scan oracles over thousands of randomised queries, plus the edge cases that actually broke it — coincident points, boundary points, and insert/remove churn.
- **The asymptotic claim, measured** (Week 2): ~160x faster than brute force at N=500,000. Also recorded honestly: at N=100 the tree is 0.3x, i.e. *slower*, because the overhead dominates below a few hundred points.
- **The claim that actually matters, measured end to end** (Week 15): with the quadtree shortlist, **32x the drivers (500 → 16,000) costs only ~2x the solve time**. That is the O(N·M) bottleneck this ADR was written to avoid, shown not to materialise.

## Context
The engine must answer "which drivers are near this rider?" thousands of times per batch. Scanning all M drivers per rider is O(N·M) — the exact bottleneck the project exists to avoid. We need a spatial index that prunes far-away drivers cheaply, in C++ (the CPU-bound tier).

## Options considered
1. **Flat array + linear scan** — trivial, but O(N·M); defeats the purpose.
2. **Uniform grid / geohash buckets** — simple and cache-friendly, but performs poorly when density is highly non-uniform (a stadium vs. suburbs), which is precisely our peak-load scenario.
3. **Quadtree** — recursively subdivides space by density; adapts to clustering. O(log N) average queries. Well-understood, self-contained, no dependencies.
4. **k-d tree / R-tree** — excellent query performance but more complex to implement correctly from scratch and to keep balanced under churn.

## Decision
Implement an **explicit Quadtree** from scratch in C++. It adapts to non-uniform density (unlike a uniform grid), is simple enough to implement and verify by hand, and aligns with the "build the core algorithms from scratch" learning goal of the project.

## Consequences
- ➕ O(log N) range queries; branch pruning skips empty regions.
- ➕ Simple enough to fully unit-test and brute-force-verify (done: 0 mismatches).
- ➖ Requires care around edge cases — coincident points can cause unbounded recursion (mitigated with a `MAX_DEPTH` cap).
- ➖ No built-in removal in the first cut; `remove()` is required before drivers can go offline dynamically (tracked as a Week 2 enhancement).
- Redis GEO is still used for the *live location store* (see ADR-0004); the quadtree is the *in-engine* index built per batch. The two are complementary, not competing.
