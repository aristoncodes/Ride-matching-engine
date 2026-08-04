# Week 5 — Advanced Testing (and the perf follow-up it uncovered)

**Date:** Aug 6, 2026 · **Phase:** 1 (Core Algorithmic Engine, C++) · **Status:** ✅ Complete

## What this week was about

Turning "seems to work" into "provably works, **this fast**". Weeks 2–4 each shipped their own
correctness anchor as an ad-hoc `main()`. That does not scale, does not report properly, and
cannot be filtered. This week moves everything onto a real framework and — the part that matters —
makes the performance claims **assert** instead of print.

## What was built

| File | Role |
|------|------|
| **`third_party/catch2/catch.hpp`** | Catch2 v2, vendored single header. |
| **`tests/oracles.h`** | Deliberately slow reference implementations that share no code with what they check. |
| **`tests/test_quadtree.cpp`** | Week 2 structure, incl. the edge cases that actually broke it. |
| **`tests/test_assignment.cpp`** | Week 3 solver + the new engine-equivalence proof. |
| **`tests/test_router.cpp`** | Week 4 Dijkstra/A*/backward search/snapping. |
| **`tests/test_osm_loader.cpp`** | Map *interpretation*: one-ways, speeds, exclusions, clipped refs. |
| **`tests/test_cost_matrix.cpp`** | The geometry↔solver boundary. |
| **`tests/test_performance.cpp`** | Budgets that fail the build. |

`src/assignment_test.cpp` is **deleted** — every check it made is ported.

## Why Catch2 v2, vendored

v2 is **one header with no build system of its own**, so it drops into CMake with zero ceremony.
Vendoring it rather than using `FetchContent` means a clean checkout builds and tests **offline** —
including in a CI container with no network egress. A test suite that needs the internet to run is
a test suite that will be skipped.

Split into two CTest entries deliberately:

```
correctness   →  the engine is WRONG
performance   →  the engine is merely SLOW
```

Those demand different responses, so they should not fail as one thing.

## The testing idea that carries the week: independent oracles

The point of an oracle is that it **shares no code with the thing it checks**. Two implementations
can both be wrong; they are unlikely to be wrong in the *same way*, so their agreement is evidence.

| Under test | Oracle |
|---|---|
| Quadtree `queryRange` / `nearestNeighbor` / `kNearest` | Linear scan, full sort |
| MCMF assignment | Exhaustive min-cost max-matching (≤ 8×8) |
| Dijkstra | **Bellman-Ford** — no heap, no settled set, no early exit |
| A* | Dijkstra |
| Sparse cost matrix | The dense one |

This is why the suite runs 107,598 assertions from 71 cases: most of them are randomised
comparisons, not hand-written expectations.

## Edge cases, chosen on purpose

Empty trees and empty batches. A single point; a single-node graph. **500 coincident points** — the
case that recurses forever without a depth cap, because subdividing can never separate them.
Points **exactly on boundaries**, including the centre, which lies on the dividing line of all four
children and gets stored twice or lost by a wrong comparison. N≠M in both directions. A rider with
no candidate edges. **Zero-cost edges** — a driver already at the door — which any code treating
`0` as a "no edge" sentinel gets wrong. Duplicate edges. 5000 steps of insert/remove churn, because
the quadtree's failure mode is slow corruption rather than an immediate crash. One-way streets,
`oneway=-1` (a way digitised backwards), untagged roundabouts, disconnected islands, and node
references clipped by the extract boundary.

## Benchmarks that assert

Rules the file follows to stay honest:

- report the **median of 21 runs** — not a single sample, and not best-of;
- assert a budget with real headroom, so it fails on regressions and not on a busy laptop;
- **never** assert a timing under sanitizers or in a debug build, where the number is meaningless
  (CMake does not even register the `performance` test when sanitizers are on).

### The headline claim

> **N=M=100, k=8: raw coordinates → provably optimal assignment in 0.404 ms.** Budget 1.0 ms.

Inside a 3-second batch window that is 0.013% of the budget.

**The stated size is the claim.** Sub-millisecond holds through N=M=100 and is lost by 150 — and
*both* are recorded, because quoting only the size that passes is how a benchmark becomes
marketing:

| N=M | 25 | 50 | 75 | **100** | 150 | 200 |
|---|---|---|---|---|---|---|
| build + solve | 0.15 | 0.44 | 0.61 | **0.64** | 1.42 | 2.46 ms |

Also pinned: the road cost matrix costs one search **per rider**, so **4× the drivers costs no
extra time** (30.2 ms → 30.1 ms). That invariant is the entire reason the builder is shaped around
backward sweeps, and it is trivially destroyed by a refactor that routes each pair individually.

Numbers are committed to [`docs/Benchmarks.md`](../docs/Benchmarks.md) and written to
`benchmark_results.txt` on every run.

## Sanitizers (stretch)

```bash
cmake -DENABLE_SANITIZERS=ON -DCMAKE_BUILD_TYPE=Debug .. && make && ./unit_tests "~[perf]"
# All tests passed (107598 assertions in 71 test cases)
```

Clean under ASan + UBSan. Worth noting these were run against code doing raw pointer arithmetic
into CSR spans and heavy `unique_ptr` churn in the quadtree — precisely where a leak or a
use-after-free would hide.

## The surprise: the Week 3 perf follow-up did not go as planned

Week 3 left an explicit note: *"MCMF shortest-path is SPFA-based, O(F·V·E); dense N=M=1000 solve
~10s. Dijkstra-with-potentials is the planned speedup."*

I implemented **Johnson potentials** — keep a potential `pot[v]`, search on the reduced cost
`cost(u→v) + pot[u] − pot[v]`, which is provably ≥ 0 (that is just the triangle inequality shortest
paths already satisfy), so Dijkstra can run on a residual graph that contains negative edges. The
reduced costs telescope along any path, so it is a **change of coordinates, not an approximation**;
the optimum is untouched.

Then the asserting benchmark caught something I would otherwise have shipped: **the sparse solve
got 2.4× SLOWER.**

| N=M=1000 | edges/node | SPFA | Dijkstra | winner |
|---|---|---|---|---|
| k=8 (sparse) | 5.0 | **56 ms** | 152 ms | SPFA **2.7×** |
| k=64 | 33.0 | **347 ms** | 356 ms | tie |
| k=96 | 49.0 | 482 ms | **436 ms** | Dijkstra |
| dense | 500.5 | 8206 ms | **3576 ms** | Dijkstra **2.3×** |

- SPFA re-relaxes an edge once per improvement, so its work grows with **density**.
- Dijkstra settles each node once but pays a log-factor heap and an O(V) reset per augmentation —
  overhead that dominates when E is small.

The crossover sat between **33 and 49 edges per node at every size tested** (N = 200, 600, 1000),
so `ShortestPathEngine::Auto` switches at **40** — a threshold from a measurement sweep, not from
taste.

And the guard that makes it safe: **1200 random graphs plus a dense case assert both engines return
an identical optimum.** An engine choice that silently changed answers depending on batch density
would be the worst class of bug — it would only appear at certain loads.

**Net:** dense path 8.2 s → 3.6 s at N=M=1000; sparse path keeps its speed. Week 3's follow-up is
closed, though not in the direction it was written.

## Bugs this week found

1. **Static destruction order.** The benchmark results file wrote out empty. The measurement table
   was a function-local `static`, constructed lazily on *first use* — which is **after** the writer
   object. Statics are destroyed in reverse order of construction, so the table died first and the
   writer read a corpse. Fix: heap-allocate and intentionally leak it. Costs one vector at process
   exit.
2. **`Point` has no default constructor**, so `std::vector::resize` will not even compile — the same
   trap `kNearest` hit back in Week 2. `erase` is the answer.

## Checkpoint

> ✅ `ctest` is green, edge cases are covered, and the sub-ms claim is backed by a committed
> measurement.

| Suite | Assertions | Cases | Time |
|---|---|---|---|
| `correctness` | 107,598 | 71 | 0.12 s |
| `performance` | 21,079 | 5 | 0.79 s |
| `routing_end_to_end` | — | — | 0.78 s |
| `road_matching_end_to_end` | — | — | 0.66 s |

**4/4 passing, 2.35 s total** — which is the number that actually decides whether it gets run
before every commit.

## Files touched

`tests/*` (new), `third_party/catch2/catch.hpp` (vendored), `mcmf.{h,cpp}` (Johnson potentials +
engine selection), `assignment.{h,cpp}` (engine parameter), `CMakeLists.txt` (test target,
`ENABLE_SANITIZERS`, Release default, 4 CTest entries), `docs/Benchmarks.md` (new).
`src/assignment_test.cpp` deleted.
