# Benchmarks

Committed measurements, so performance regressions are visible in a diff rather
than discovered in production. Every number here is reproducible from the repo:

```bash
cd matching_engine/build
cmake .. && make
ctest                        # correctness + performance + end-to-end
./unit_tests "[perf]"        # timings only; also writes benchmark_results.txt
```

**Reference machine:** Apple M-series, macOS 15, Apple Clang, `-O3` (CMake
`Release`, which is now the default build type — see `CMakeLists.txt`).
**Last updated:** Aug 4, 2026 (Week 5).

A number without the machine it was measured on is a number you cannot check,
so re-measure before comparing. What should hold across machines is the
*shape*: the ratios between rows, not their absolute values.

---

## 1. The headline claim — sub-millisecond pairing matrix

> Given a 3-second batch of 100 riders and 100 drivers, going from raw
> coordinates to a **provably optimal** assignment takes **0.40 ms**.

That is 0.013% of the batch window. Asserted in
`tests/test_performance.cpp` with a 1.0 ms budget, so a regression fails
`ctest` rather than showing up later.

| Stage | N=M=100, k=8 | Notes |
|---|---|---|
| Sparse cost-matrix build | 0.229 ms | quadtree k-nearest shortlist |
| Optimal solve (MCMF) | 0.354 ms | min-cost max-flow, provably optimal |
| **End to end** | **0.404 ms** | budget: 1.0 ms |
| Dense equivalent | 3.80 ms | same batch, all N×M edges — 9.4× slower |

### Why the stated size is 100, not the largest number that passes

Sub-millisecond holds through N=M=100 and is lost by 150. Both are recorded,
because quoting only the size that passes is how a benchmark becomes marketing.

| N=M | build + solve (median of 21) |
|---|---|
| 25 | 0.15 ms |
| 50 | 0.44 ms |
| 75 | 0.61 ms |
| **100** | **0.64 ms** ← budgeted size |
| 150 | 1.42 ms |
| 200 | 2.46 ms |

(The 0.64 ms sweep figure and the 0.40 ms table figure differ because the sweep
was a standalone binary and the table is the in-suite measurement with warm
caches. Same code, different harness — which is itself a reminder that a timing
is only comparable to another timing taken the same way.)

---

## 2. Spatial index — quadtree vs brute force (Week 2)

Nearest-neighbour search, quadtree vs a linear scan:

| Points | Speedup |
|---|---|
| 100 | 0.3× (slower — the tree is pure overhead at this size) |
| 10,000 | ~14× |
| 500,000 | ~160× |

The 0.3× at N=100 is kept deliberately: below a few hundred points a linear
scan wins, and pretending otherwise would hide a real threshold.

`kNearest` at production scale, asserted with a 4 ms budget:

| Operation | Time |
|---|---|
| 1000 × `kNearest(k=8)` over 50,000 drivers | 1.84 ms (~1.8 µs/query) |

---

## 3. Routing — Dijkstra vs A* (Week 4)

Graph: real OpenStreetMap extract of central Bengaluru
(`matching_engine/data/bengaluru_roads.osm`, refetch with
`tools/fetch_road_extract.py`).

| Property | Value |
|---|---|
| Nodes | 27,890 |
| Directed arcs | 53,784 |
| Largest strongly connected component | 26,919 (96.5%) |
| Bounding box diagonal | 10.31 km |

Over 200 random pairs drawn from the SCC:

| Metric | Dijkstra | A* | Ratio |
|---|---|---|---|
| Nodes settled (total) | 2,872,154 | 1,580,510 | **1.82× fewer** |
| Wall clock per query | 0.83 ms | 0.71 ms | 1.17× faster |
| Worst cost difference vs Dijkstra | — | **0.00 s** | exact |

**A* settles 1.82× fewer nodes but is only 1.17× faster.** Two honest reasons,
neither of which is a bug:

1. A* does more work per node — an extra heap key, and more pushes as `f`
   improves.
2. The heuristic is weak *because it must be*. Admissibility requires dividing
   straight-line distance by the network's **fastest** road (70 km/h) while most
   of the city runs at 25–45 km/h, so every estimate is roughly 2× short and the
   search stays broad. Sharpening it without breaking admissibility is exactly
   what ALT landmarks and Contraction Hierarchies are for.

The zero cost difference is the point of the week: A* is a search-order
optimisation, not an approximation.

### Hot-origin distance cache (Week 4 stretch)

Backward one-to-all trees from 4 hot origins:

| Metric | Value |
|---|---|
| Build (4 backward Dijkstras) | 16.2 ms |
| Memory | 0.85 MB |
| 200 lookups | 0.046 ms |
| Same 200 as fresh Dijkstras | 372 ms |

~8000× faster per query once built, at 0.85 MB per 4 origins. That memory cost
is why it is a cache for hotspots and not a full distance table.

---

## 4. Does real routing actually change the match? (Week 4)

60 riders, 60 drivers on the Bengaluru graph. **Both assignments scored under
the same yardstick — true road travel time** — because scoring the
distance-matched assignment by distance would only prove that distance is good
at being distance.

| Matched on | Total wait | Mean | Worst |
|---|---|---|---|
| Straight-line distance | 156.5 min | 2.6 min | 8.7 min |
| **Real road time** | **141.6 min** | **2.4 min** | 9.3 min |

- **63% of pairings changed** (38 of 60).
- **9.5% of total rider waiting saved**, for free — same solver, better matrix.
- Mean detour factor across the network: **1.51×**. That is the multiplier by
  which straight-line distance was lying to the matcher.

### The worst case got worse, and that is not a bug

Total wait improved while the single worst wait rose from 8.7 to 9.3 minutes.
The solver minimises the **sum**, and a sum has no opinion about its largest
term — it will happily make one rider wait 9 minutes to save 30 seconds each for
twenty others. A ceiling on any individual wait is a **constraint to add to the
model** (drop edges above a cutoff, or minimise a convex function of wait), not
something optimality provides for free.

### Cost of pricing a matrix in road time

| Matrix | N=20 riders × 20 drivers | × 80 drivers |
|---|---|---|
| Road-time dense build | 30.2 ms | 30.1 ms |

**4× the drivers for no extra time.** The dense road matrix costs one *backward*
Dijkstra per **rider** — a single sweep prices that rider against every driver
at once — so cost scales with riders, not with pairs. This invariant is asserted
in the perf suite, because it is easy to destroy with an innocent refactor that
routes each pair individually.

---

## 5. MCMF shortest-path engine (Week 5)

Week 3 left a known follow-up: the min-cost-flow solver used SPFA
(queue-based Bellman-Ford), at O(F·V·E). Adding **Johnson potentials** lets each
augmentation use Dijkstra instead.

The result was not the expected clean win — it depends on density:

| N=M | k | edges/node | SPFA | Dijkstra | winner |
|---|---|---|---|---|---|
| 1000 | 4 | 3.0 | **25.6 ms** | 102.8 ms | SPFA 4.0× |
| 1000 | 8 | 5.0 | **56.4 ms** | 152.1 ms | SPFA 2.7× |
| 1000 | 32 | 17.0 | **199.5 ms** | 270.3 ms | SPFA 1.4× |
| 1000 | 64 | 33.0 | **347.5 ms** | 355.6 ms | SPFA 1.02× |
| 1000 | 96 | 49.0 | 482.0 ms | **436.4 ms** | Dijkstra 1.10× |
| 1000 | dense | 500.5 | 8206 ms | **3576 ms** | Dijkstra 2.3× |

The crossover sat between **33 and 49 edges per node at every size tested**
(N=200, 600, 1000), so `ShortestPathEngine::Auto` switches at 40.

- SPFA re-relaxes an edge once per improvement, so its work grows with density.
- Dijkstra settles each node once but pays a log-factor heap and an O(V) reset
  per augmentation — overhead that dominates when E is small.

Every configuration returned an **identical total cost**, and
`tests/test_assignment.cpp` asserts that equality over 1200 random graphs plus a
dense case that trips the threshold. The engine choice is a speed decision that
must never change an answer.

**Net effect on the dense path:** 8.2 s → 3.6 s at N=M=1000, closing the Week 3
follow-up.

---

## 6. Test suite

| Suite | Assertions | Cases | Time |
|---|---|---|---|
| `correctness` (`~[perf]`) | 107,598 | 71 | 0.12 s |
| `performance` (`[perf]`) | 21,079 | 5 | 0.79 s |
| `routing_end_to_end` | — | — | 0.78 s |
| `road_matching_end_to_end` | — | — | 0.66 s |

The whole suite runs in **2.35 s**, which is the number that actually decides
whether it gets run before every commit.

Clean under **AddressSanitizer + UndefinedBehaviorSanitizer**:

```bash
cmake -DENABLE_SANITIZERS=ON -DCMAKE_BUILD_TYPE=Debug .. && make && ./unit_tests "~[perf]"
# All tests passed (107598 assertions in 71 test cases)
```

Timing assertions are not registered under sanitizers — a 2–3× slowdown would
make every budget meaningless.
