# Learnings — Week 2 (Quadtree / Spatial Partitioning)

Concepts and interview-ready takeaways from the quadtree. Report: [week2.md](week2.md)

## 1. Why a spatial index at all
"Find drivers near this rider" by scanning everyone is **O(N)** per query, **O(N·M)** for a batch.
A quadtree recursively splits space into 4 quadrants, so a range/nearest query **prunes** whole
quadrants that can't contain the answer → ~**O(log N)** on uniformly spread data.
- **Interview angle:** *"How would you find the nearest of a million points?"* → spatial index
  (quadtree / k-d tree / R-tree / geohash grid). Naming the tradeoffs is what scores.

## 2. Quadtree vs k-d tree (know the difference)
- **Quadtree:** splits *space* into 4 fixed quadrants each level. Simple; can get deep/unbalanced
  when points cluster (mitigated by a MAX_DEPTH cap).
- **k-d tree:** splits on alternating axes at the *median point*, staying balanced; better for
  static data, harder to update dynamically.
- Quadtree was chosen here because drivers move — dynamic insert/remove is the common op.

## 3. The "fast" claim needs a number (and a correct benchmark)
Speedup grew 0.3× → 160× as N went 100 → 500,000. Two benchmarking lessons:
- **Small N: the index LOSES.** Tree overhead beats a tight linear scan only past a break-even
  size. Always report the crossover, don't cherry-pick big N.
- **Dead-code elimination will fake your numbers.** The first benchmark showed brute-force at
  `0.000 ms` because its result was unused, so the optimizer deleted the loop. Fix: consume the
  result (`volatile` accumulator / `DoNotOptimize`). **Interview angle:** *"How do you benchmark
  in C++ without the optimizer cheating?"*

## 4. Nearest-neighbor with a range-query tree: the correctness subtlety
`queryRange` answers "what's in this box." Nearest-neighbor via a growing box has a trap: a point
up to **√2 · radius** away can sit just *outside* a box of half-extent `radius` (near a corner).
So a candidate is only trustworthy once its true distance ≤ `radius`; otherwise grow and retry.
- **Density-aware start radius:** starting the box at a fixed fraction of the whole grid degrades
  to O(N) at high density (the first box scoops up thousands). Seed the radius from the *leaf
  cell* containing the query instead — small where dense, large where sparse. This is *the* detail
  behind the "O(log N) nearest neighbor" claim.

## 5. Memory safety in C++ (RAII)
- Children are `std::unique_ptr` → freed automatically and recursively; no manual destructor.
- Copy ctor / copy assignment are `= delete`d → copying an owning tree would double-free.
- **Interview angle:** *"unique_ptr vs shared_ptr?"* unique = single owner, zero overhead;
  shared = refcounted, use only when ownership is genuinely shared. Prefer unique by default.

## 6. Removal invariants — where my two bugs lived
The deep lesson: **know exactly which invariant your structure maintains.**
- Because `insert()` doesn't redistribute a node's existing points when it subdivides, a *divided*
  node can still hold points. Any code assuming "divided ⇒ points only in children" is wrong.
  → `remove()` must check the node's own points before recursing.
- "Collapse empty subtrees" sounds like a nice-to-have but interacts with the above: clearing a
  parent before merging children silently deletes the parent's own points. **Silent data loss, not
  a crash** — exactly what tests exist to catch.
- **Interview angle:** *"What's the hard part of deletion in a tree?"* → maintaining structural
  invariants (balance, occupancy) after the removal, not the find-and-erase itself.

## Quick self-test
- At what N does a quadtree start beating brute force, and why not sooner?
- Why can't you trust a nearest-neighbor candidate until its distance ≤ the search radius?
- Why did `remove()` miss points even though `size()` counted them?
- Why is copy-construction deleted on the quadtree?
