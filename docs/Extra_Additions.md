# Extra Additions (Post-Project Ideas)

**Status:** Idea backlog · **Related:** [Scope_and_Production_Comparison.md](Scope_and_Production_Comparison.md)

A parking lot for features to build **after** the core 24-week project is done. These are deliberately *out of scope* for v1 — they depend on the foundation (spatial index, optimal matching, routing/ETA) being finished first. Captured here while the ideas are fresh so they aren't lost.

Each entry notes **what it is**, **what it depends on**, and a **rough sketch** of how it would work.

---

## 1. Ride-Pooling (Shared Rides)

*The UberPool / Ola Share idea: one car carries multiple riders going in a similar direction, each paying less, with a bounded extra detour.*

### The idea (product view)
When a rider books a car, they can **opt in to sharing**. The system may then add a second rider from the same area to the same car, if and only if:
- there's a **free seat** (capacity),
- the **existing passenger's arrival is delayed by no more than a cap** (e.g. **+5–7 minutes**),
- the new rider's wait/detour is reasonable,
- **both riders pay a lower fare** than riding solo.

These are the real constraints production pooling systems use.

### Why it's a *separate, harder* problem (not part of v1)
The v1 engine solves **1 rider → 1 driver** (the assignment problem, via Hungarian/MCMF). Pooling changes the shape:

| | v1 matching | Ride-pooling |
|---|---|---|
| Riders per car | 1 | multiple |
| The question | "who pairs with whom?" | "who pairs with whom **and in what pickup/dropoff order**?" |
| Core algorithm | assignment (Hungarian) | **routing + insertion with time windows** |
| Known as | Linear Assignment Problem | **Dial-a-Ride Problem (DARP)** / Vehicle Routing |
| Difficulty | hard | much harder (research-grade at high capacity) |

### Dependencies (must exist first)
- ✅ **Spatial index (Week 2)** — find nearby poolable riders.
- ⬜ **Optimal matching (Week 3)** — the matching machinery is reused (see "shareability graph" below).
- ⬜ **Routing / ETA (Week 4)** — **required.** Detour = *travel time*, which needs the road-graph router, not straight-line distance.

So this comes **after Week 4 at the earliest.**

### Rough sketch — the feasibility check
The heart of pooling is: *"can rider B be added to the car currently serving rider A?"*

1. **Capacity:** free seat? If not, stop.
2. **Try insertion orderings** of B's pickup + dropoff into A's route, e.g.
   `driver → A_pickup → B_pickup → B_dropoff → A_dropoff` (and other valid orderings).
3. For each ordering, use the **routing engine** to compute new travel times.
4. **Check constraints:** A's delay ≤ cap (e.g. 7 min); B's wait acceptable; both save money.
5. If any ordering passes → the pair is **poolable**; score it by benefit (fare saved vs. time added).

### Two tiers of ambition
- **Pairwise pooling (2 riders/car)** — *achievable extension.* Model it as **matching on a "shareability graph"** (nodes = trips, edge = "these two can share"). This reuses the exact bipartite-matching machinery from Week 3 — just applied to rider-pairs instead of rider-driver pairs.
- **High-capacity pooling (3–4+ riders)** — *research-grade.* Route combinations explode; real systems use heuristics + integer programming. Reference: *Alonso-Mora et al., "On-demand high-capacity ride-sharing via dynamic trip-vehicle assignment," PNAS 2017.*

### Suggested first version
Pairwise pooling, opt-in, with a hard detour cap — built on top of the finished router and matcher. High-capacity is a later, optional research exercise.

---

## Template for new ideas

```
## N. <Feature name>
*One-line description.*
### The idea
### Why it's separate / hard
### Dependencies (what must exist first)
### Rough sketch
```
