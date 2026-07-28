# Week 3 — Bipartite Matching (Optimal Assignment via MCMF)

**Date:** Jul 23, 2026 · **Phase:** 1 (Core Algorithmic Engine, C++) · **Status:** ✅ Complete

## What this week was about
Given a batch of riders and nearby drivers, assign them **optimally** — minimize the *total*
distance across all pairs, with each driver used at most once. This replaces the greedy
placeholder (each rider grabs its own nearest driver), which is locally optimal but globally
poor. This is the classic **Assignment Problem**.

## Algorithm chosen: Min-Cost Max-Flow (MCMF), not Hungarian
MCMF handles rectangular N≠M cases natively (riders rarely equal drivers) and composes with the
sparse-candidate optimization. Hungarian is the "textbook" square-matrix answer; MCMF is the
more general primitive.

## What was built (3 clean layers — deliberate separation of concerns)
| File | Role |
|------|------|
| **`mcmf.h/.cpp`** | Pure, domain-agnostic Min-Cost Max-Flow solver (Successive Shortest Paths). Knows nothing about riders/drivers. |
| **`assignment.h/.cpp`** | Models the rider→driver problem as a flow network on top of MCMF. Returns `Assignment{riderToDriver, totalCost, matchedCount}`. |
| **`cost_matrix.h/.cpp`** | The ONLY file that touches coordinates. Turns points into the `MatchEdge` list the solver eats: `buildDenseEdges` (all pairs) + `buildSparseEdges` (k-nearest via quadtree). |
| **`assignment_test.cpp`** | Correctness anchor: solver vs brute-force optimality oracle. |
| **`optimal_match.cpp`** | Showcase demo: greedy vs optimal-dense vs optimal-sparse + overflow. |

## The flow model (how the assignment becomes a flow problem)
```
source ──cap1,cost0──▶ rider ──cap1,cost=dist──▶ driver ──cap1,cost0──▶ sink
```
Each driver's **single unit** of outgoing capacity is what guarantees no driver is matched twice.
Max-flow = min(N,M) matched; min-cost among max-flows = cheapest total distance.

## Requirements — all met
- ✅ **Separated** cost-matrix construction from the pure solver.
- ✅ **Rectangular N×M** handled natively (no dummy padding).
- ✅ **Proved optimality:** vs a brute-force min-cost-max-matching oracle over **3000 random
  cases** (dims 0–8) + edge cases (empty, 1×1, N>M, M>N, edgeless rider, greedy-trap).
  **29,683 assertions, 0 failures.** Wired into CTest (`ctest` green).
- ✅ **Overflow policy** documented: unmatched riders return `-1`; re-queue-vs-reject is a
  service-layer decision, not the solver's.
- ✅ **(stretch) Sparse cost matrix:** each rider → its k nearest drivers → O(N·k) edges vs
  O(N·M). ~55× faster solve at N=M=600.

## Results from the demo
| Scenario | greedy total | optimal total | improvement |
|----------|-------------|---------------|-------------|
| 200×200 balanced | 213,993 | 148,607 | **30% cheaper** |
| 300×100 overflow | 138,511 | 29,799 | **78% cheaper** (+200 riders left over → `-1`) |
| 600×600 | 421,913 | 273,018 | 35% cheaper |

Dense N=M=600 solve ≈ 1000 ms over 360k edges; sparse ≈ 18 ms over 4,800 edges.

## Known follow-up (Week 5 perf)
The shortest-path inside MCMF is **SPFA-based**, O(F·V·E); dense N=M=1000 takes ~10 s.
**Dijkstra-with-potentials** is the planned speedup. Correctness is unaffected.

→ Learnings for this week: [learnings3.md](learnings3.md)
