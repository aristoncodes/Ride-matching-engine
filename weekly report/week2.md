# Week 2 — Spatial Partitioning (Quadtree)

**Date:** Jul 16, 2026 · **Phase:** 1 (Core Algorithmic Engine, C++) · **Status:** ✅ Complete

## What this week was about
When a rider requests a ride, we need to answer "which drivers are near this pickup?" **fast**.
Scanning all M drivers for every rider is O(N·M) — it collapses under load (the concert-lets-out
scenario). A **quadtree** partitions 2D space so "who's near here?" is answered in ~O(log N)
by pruning whole regions that can't contain nearby points.

## What was built / extended
- **`matching_engine/src/quadtree.h` / `quadtree.cpp`** — an explicit quadtree over 2D points.
- **`matching_engine/src/benchmark.cpp`** — a timing harness (quadtree vs brute force).
- **`match_demo.cpp`** — updated to call the new `nearestNeighbor()` instead of hand-rolling the search.

## What the quadtree does
- **`insert(point)`** — place a point; a full leaf `subdivide()`s into 4 quadrants (NW/NE/SW/SE).
- **`queryRange(box, out)`** — collect all points inside an axis-aligned box, pruning
  non-intersecting branches.
- **`remove(point)`** — remove by id, and **collapse** a subtree of 4 leaves back into their
  parent once they fit — so churn (drivers going on/offline) doesn't permanently bloat the tree.
- **`nearestNeighbor(x, y)`** — single nearest point, via an **expand-and-retry box search**
  (grow the search box until the closest candidate is provably closer than the box edge).
- **`kNearest(x, y, k)`** — the k nearest points (added in Week 3 for sparse candidate generation).
- **`size()` / `clear()`** — convenience.

## Safety & correctness properties
- **Memory-safe:** children are `std::unique_ptr` (auto-freed, recursively); copy constructor/
  assignment `= delete`d to prevent double-free.
- **`MAX_DEPTH` cap:** prevents infinite recursion when many points share (near-)identical
  coordinates — a capped node just becomes a bucket.
- **Verified against brute force:** the benchmark cross-checks every quadtree result against a
  linear scan via a checksum; a mismatch prints `[MISMATCH]`.

## Benchmark result (the "fast" claim, with a number)
Quadtree vs brute-force nearest-neighbor, 2000 queries:

| N | brute-force | quadtree | speedup |
|---|---|---|---|
| 100 | 0.34 ms | 1.08 ms | 0.3× (tree overhead dominates at tiny N) |
| 10,000 | 15.5 ms | 1.35 ms | ~11× |
| 100,000 | 105 ms | 2.0 ms | ~52× |
| 500,000 | 511 ms | 3.2 ms | ~160× |

The speedup grows with N — the signature of O(log N) beating O(N).

## Two real bugs found & fixed this week
1. **`remove()` couldn't reach points stored above the leaf level.** Because `insert()` doesn't
   push a node's existing points down when it subdivides, a *divided* node can still hold its own
   points. The first `remove()` recursed straight into children and skipped them — making some
   points permanently unremovable (though `size()`/`queryRange` still saw them). Fix: check the
   node's own points *before* recursing.
2. **`collapseIfPossible()` silently deleted data.** It did `points.clear()` before merging
   children up — fine only if the parent had no points of its own, which (per bug #1) isn't
   guaranteed. Fix: count the parent's own points into the capacity check and *append* instead
   of clearing.

## Definition of done — all met
- ✅ insert / queryRange / subdivide (memory-safe, depth-capped, brute-force-verified)
- ✅ remove with subtree collapse
- ✅ nearestNeighbor wrapper (search logic moved out of `main` into the class)
- ✅ size / clear
- ✅ (stretch) benchmark harness with a committed number

→ Learnings for this week: [learnings2.md](learnings2.md)
