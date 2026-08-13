# ADR-0003: Optimal (Hungarian / MCMF) matching over greedy nearest-neighbor

## Status
🟢 **Accepted** — built in Week 3 and validated repeatedly since.

**Validation:**
- **Optimality proven, not assumed.** `tests/test_assignment.cpp` checks the MCMF solver against an independent brute-force min-cost-max-matching oracle over 3,000 randomised cases plus hand-picked edge cases, including a greedy trap where greedy scores 101 and the optimum is 4.
- **Measured against greedy on real data** (Week 4): optimal beat greedy by 30–78% on total distance across batches.
- **The cost of optimality is measured** (Week 15): the sparse path solves N=M=500 in ~16 ms, while the dense equivalent at N=M=800 takes ~2 s. That is the number that justifies the k-nearest shortlist rather than assuming it.

## Context
The core value proposition is *optimal* matching. An interim greedy matcher (each rider grabs its nearest available driver) was built to exercise the quadtree, and it immediately exposed the problem: greedy is order-dependent and can produce globally poor assignments — e.g., an early rider takes the only driver that a much closer later rider needed, inflating total wait time. We must minimize *total* cost across the batch, with each driver used at most once. This is the assignment problem on a bipartite graph.

## Options considered
1. **Greedy nearest-neighbor** — O(N·k) and simple, but not optimal; sensitive to processing order. Fine as a placeholder, not as the product.
2. **Hungarian Algorithm** — O(n³), produces a provably optimal 1-to-1 assignment on a cost matrix. Classic, well-documented, straightforward to verify.
3. **Min-Cost Max-Flow (MCMF)** — models matching as a flow problem; naturally handles rectangular N≠M and capacity constraints (e.g., a driver serving multiple riders later). More flexible, slightly more machinery.

## Decision
Implement **optimal matching — Hungarian for the square/balanced case, with MCMF as the generalization** for rectangular N×M and future capacity constraints. Both are built from scratch (project learning goal) and verified against a brute-force permutation solver on small inputs.

## Consequences
- ➕ Provably optimal assignments — the product's headline claim becomes true, not aspirational.
- ➕ Correctness is anchorable via brute-force cross-check on N ≤ 8.
- ➖ O(n³) is heavier than greedy. **Mitigation:** feed the solver a *sparse* cost matrix (each rider's k-nearest drivers from the quadtree) instead of dense N×M, keeping it within the latency budget at scale.
- ➖ Requires an explicit policy for unmatched riders when N > M (re-queue vs. reject) — an open question tracked in the PRD.
- The greedy matcher is retained only as a test oracle / baseline, not shipped.
