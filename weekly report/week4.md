# Week 4 — Dijkstra / A* Routing on a Real Road Graph

**Date:** Jul 30, 2026 · **Phase:** 1 (Core Algorithmic Engine, C++) · **Status:** ✅ Complete

## What this week was about

Weeks 1–3 treated the world as a flat plane and "distance" as a straight line. That is a lie a
rider can feel: two points 300 m apart across a lake are a 4 km drive. This week replaces
straight-line distance with **real travel time along real roads** — which is the thing a rider
actually waits through.

The Week 3 solver was optimal *with respect to the cost matrix it was handed*. Week 4 fixes the
matrix, not the solver.

## What was built

| File | Role |
|------|------|
| **`road_graph.h/.cpp`** | The road network as a directed, weighted graph in **CSR** form. Forward *and* reverse adjacency, haversine, GPS→junction snapping, largest strongly connected component. |
| **`osm_loader.h/.cpp`** | Dependency-free `.osm` XML parser: one-ways, maxspeed, per-class speed defaults. Plus `buildGridGraph()`, a synthetic grid so tests never need the data file. |
| **`router.h/.cpp`** | Dijkstra, A*, one-to-all forward/backward trees, and `SourceDistanceCache` for hot origins. |
| **`route_demo.cpp`** | Validates A* cost == Dijkstra cost; reports search-space reduction. Exits non-zero on mismatch → CTest anchor. |
| **`road_match_demo.cpp`** | The integration question: does routing change *who gets matched to whom*? |
| **`tools/fetch_road_extract.py`** | Reproducibly downloads and filters the OSM extract. |
| **`data/bengaluru_roads.osm`** | 27,890 nodes / 7,811 road ways of central Bengaluru. |

## The data

`api.openstreetmap.org` caps a single bbox request at ~50k nodes, so the fetcher pulls a **grid of
12 tiles and merges them** — nodes shared across tile borders de-duplicate by OSM id, and that is
what stitches the tiles into one network. It then strips everything that is not a drivable road,
taking the download from ~40 MB of raw OSM to a 3.3 MB extract.

| Property | Value |
|---|---|
| Nodes | 27,890 |
| Directed arcs | 53,784 |
| Largest strongly connected component | 26,919 (**96.5%**) |
| Bounding box diagonal | 10.31 km |
| Parse time | 130 ms |

## Design decisions worth defending

**CSR, not a vector-of-vectors.** The finished graph is one contiguous arc array plus an offset
array. A `vector<vector<Arc>>` would be ~28,000 separate allocations and a pointer chase per
neighbour — in the innermost loop of every shortest-path query the engine will ever run.

**A reverse graph, built up front.** The matching question is *"how long until each driver reaches
this rider?"*. Answering it with forward searches costs one Dijkstra per driver. Running **one
backward search from the rider** answers it for every driver at once. On a directed network this
is not a symmetry trick — A→B and B→A are genuinely different trips.

**Strongly connected components, not connected components.** With one-ways, u reaching v does not
imply v reaches u; only inside an SCC is every pair mutually reachable. Every extract of a bounded
area contains dead stubs (roads clipped at the boundary, service driveways), and a benchmark that
keeps drawing unreachable pairs measures failed searches instead of routing. Iterative Kosaraju,
because a recursive DFS down a 27k-node highway chain is a stack overflow, not an exception.

**cos(latitude) longitude scaling in the spatial index.** A degree of longitude is shorter than a
degree of latitude everywhere but the equator. Without the correction the quadtree's Euclidean
metric is stretched and "nearest" can simply be wrong — negligible in Bengaluru (~2.5%), badly
wrong in Oslo. This engine is meant to be sold to fleets anywhere.

**No XML library.** The subset of XML an `.osm` file uses is tiny — three element types, no
namespaces, no CDATA, five predefined entities. A hand-rolled single-pass scanner keeps
`cmake && make` working on any machine. It is a parser for *our* input, and the header says so.

## Why the A* heuristic is admissible (the whole argument)

```
h(v) = haversine(v, goal) / maxSpeedMps
```

- The **great-circle distance is the shortest path that could possibly exist** between two points.
  No road is shorter than a straight line.
- Dividing by the **network's fastest road** gives the shortest time that distance could possibly
  take. Nothing on the map goes faster.

So `h` can never *over*-estimate. That is the property — and the only property — that keeps A*
optimal. An estimate that overshoots can make A* settle a node before its true cheapest path is
found, and the answer silently stops being optimal. Note it divides by the **global maximum**
speed, not the local road's speed; using the local speed would be a much sharper heuristic and an
inadmissible one.

## Results

### A* vs Dijkstra — 200 random pairs from the SCC

| Metric | Dijkstra | A* |
|---|---|---|
| Nodes settled | 2,872,154 | 1,580,510 (**1.82× fewer**) |
| Wall clock / query | 0.83 ms | 0.71 ms (1.17× faster) |
| **Worst cost difference** | — | **0.00 s** |

**Exact agreement on every pair.** A* is a search-order optimisation, not an approximation.

The honest bit: **1.82× fewer nodes but only 1.17× faster.** Two reasons, neither a bug —
A* does more work per node (extra heap key, more pushes as `f` improves), and the heuristic is
weak *because it must be*: admissibility forces dividing by 70 km/h in a city that mostly runs at
25–45, so every estimate is ~2× short and the search stays broad. Closing that gap without losing
admissibility is exactly what ALT landmarks and true Contraction Hierarchies are for.

A performance fix found along the way: the heuristic was originally recomputed on **every
relaxation**, and haversine is four trig calls and an `atan2`. Memoising it per node roughly halved
A*'s runtime — the node-count win was real all along, it was just being spent on trigonometry.

### Does it change the match? (the question that justifies the week)

60 riders, 60 drivers. **Both assignments scored under the same yardstick — true road travel
time** — because scoring the distance-matched assignment by distance would only prove that
distance is good at being distance.

| Matched on | Total wait | Mean | Worst |
|---|---|---|---|
| Straight-line distance | 156.5 min | 2.6 min | 8.7 min |
| **Real road time** | **141.6 min** | **2.4 min** | 9.3 min |

- **63% of pairings changed** (38 of 60).
- **9.5% of total rider waiting saved** — same solver, better matrix.
- **Mean detour factor 1.51×**: the multiplier by which straight-line distance was lying.

### Hot-origin distance cache (stretch)

| Metric | Value |
|---|---|
| Build (4 backward Dijkstras) | 16.2 ms |
| Memory | 0.85 MB |
| 200 lookups | 0.046 ms |
| Same 200 as fresh Dijkstras | 372 ms |

~8000× per query once built. The memory cost is why it is a cache for hotspots, not a full
distance table — and it is the same "pay once, answer instantly" shape Contraction Hierarchies
generalise.

## The result I did not expect

**Total wait improved while the worst individual wait got worse** (8.7 → 9.3 min).

Not a bug. The solver minimises the **sum**, and a sum has no opinion about its largest term — it
will happily make one rider wait 9 minutes to save 30 seconds each for twenty others. If the
product needs a ceiling on any individual wait, that is a **constraint to add to the model** (drop
edges above a cutoff, or minimise a convex function of wait), not something optimality gives away
for free. Flagged in the demo output and deferred to the service layer.

## What did *not* change

`assignment.cpp` and `mcmf.cpp` were **not touched**. Week 3 deliberately quarantined all geometry
in `cost_matrix.cpp`, and swapping the entire notion of "cost" from map distance to road travel
time confirmed the boundary was drawn in the right place. That is the cleanest possible evidence
that the layering was right.

## Checkpoint

> ✅ Given two points on the road graph, you get an optimal route and its travel time, matching
> Dijkstra's cost.

Met, and enforced: `route_demo` is registered as the `routing_end_to_end` CTest and exits non-zero
on any disagreement.

## Files touched

`road_graph.{h,cpp}`, `osm_loader.{h,cpp}`, `router.{h,cpp}`, `route_demo.cpp`,
`road_match_demo.cpp`, `cost_matrix.{h,cpp}` (added `GeoPoint`, `buildRoadEdgesDense/Sparse`),
`CMakeLists.txt`, `tools/fetch_road_extract.py`, `data/bengaluru_roads.osm`.

Full numbers: [../docs/Benchmarks.md](../docs/Benchmarks.md)
