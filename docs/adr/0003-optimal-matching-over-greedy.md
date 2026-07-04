# ADR-0003: Optimal (Hungarian / MCMF) matching over greedy nearest-neighbor

## Status
🟡 Proposed (open question) — the *greedy → optimal* direction is settled, but **Hungarian vs. MCMF** and the **unmatched-rider policy (N > M)** are still open, to be resolved when the matcher is built (Week 3).

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
