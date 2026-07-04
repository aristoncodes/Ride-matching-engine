# ADR-0001: Use a Quadtree for spatial indexing

## Status
✅ Accepted (Week 2)

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
