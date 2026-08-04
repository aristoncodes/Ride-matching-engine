# Learnings — Week 4 (Dijkstra / A* on a Real Road Graph)

The routing week. Concepts and interview-ready takeaways.
Report: [week4.md](week4.md)

## 1. Dijkstra, in one sentence you can defend

**Repeatedly settle the cheapest unsettled node; when a node is popped, its cost is final.**

*Why* is it final? Everything still in the heap costs at least as much, and **all edge weights are
non-negative**, so nothing left can ever improve it. That single sentence is the whole correctness
proof, and it is also exactly the assumption that breaks: with a negative edge, a cheaper path
could still arrive later, and Dijkstra is simply wrong. (That is why the flow solver in Week 3
could not use it directly — see learnings5.)

**Interview soundbite:** "Dijkstra's correctness rests entirely on non-negative weights. The moment
you have negative edges you need Bellman-Ford, or Johnson potentials to transform them away."

## 2. A* = Dijkstra + a guess about where the goal is

Dijkstra orders the queue by `g(v)` — cost so far. A* orders it by `f(v) = g(v) + h(v)` — cost so
far **plus estimated cost remaining**. Dijkstra has no idea where the destination is, so it grows a
circle in every direction, including backwards.

**The heuristic only steers the search order. It is never part of the answer.** In my code the
stored distance stays `g`; `h` appears only in the priority.

## 3. Admissibility — the property that makes A* safe

**Admissible = never over-estimates the true remaining cost.**

My heuristic and why each half is safe:

```
h(v) = haversine(v, goal) / maxSpeedMps
        ^^^^^^^^^^^^^^^^^   ^^^^^^^^^^^
        no road can be      nothing on the map
        shorter than a      goes faster than the
        straight line       fastest road
```

**Why over-estimating breaks it:** if `h` overshoots, A* can settle a node *before* its true
cheapest path has been found. The node is then locked in at the wrong cost, and the final answer is
silently suboptimal — no crash, no warning, just a worse route.

**The trap to name in an interview:** dividing by the *local* road's speed gives a much sharper
heuristic and an **inadmissible** one. You must divide by the **global maximum** speed.

**Consistency (monotonicity)** is the stronger cousin: `h(u) ≤ cost(u,v) + h(v)`. Consistent implies
admissible, and it is what lets you skip re-opening settled nodes. Haversine/maxSpeed is consistent,
because it is a metric divided by a constant.

## 4. The result that makes this concrete

On my 27,890-node Bengaluru graph, over 200 pairs: A* settled **1.82× fewer nodes**, and the cost
differed from Dijkstra by **exactly 0.00 s** on every single pair.

That number *is* the demonstration: A* is a **search-order optimisation, not an approximation**. If
your A* returns different costs than Dijkstra, your heuristic is inadmissible — that is the debugging
rule.

## 5. Why 1.82× fewer nodes was only 1.17× faster (be honest about this)

Two reasons, and being able to explain them is more impressive than the speedup itself:

1. **A* does more work per node** — an extra heap key, more pushes as `f` improves.
2. **Admissibility forces a weak heuristic.** Dividing by 70 km/h in a city that mostly runs at
   25–45 means every estimate is ~2× short, so the search stays broad.

**And a real bug I hit:** I was recomputing `h(v)` on *every relaxation*. Haversine is four trig
calls plus an `atan2`. Memoising it per node roughly halved A*'s runtime. **Lesson: a heuristic that
costs more than the search it saves is a pessimisation** — always measure, never assume.

**Where this goes next:** ALT (A* with landmarks + triangle inequality) and Contraction Hierarchies
both exist to get a *sharper* bound while staying admissible. CH preprocesses shortcut edges and
turns continental routing into microseconds.

## 6. Directed graphs: one-ways change the question

`time(A→B) ≠ time(B→A)`. Obvious, and easy to get backwards in code.

The matching question is **"how long until each driver reaches this rider?"** — so the cost is
`time(driver → rider)`.

- Forward searches: **one Dijkstra per driver**.
- **One backward search from the rider**: answers it for *every* driver at once.

So I store the graph twice — forward and reverse adjacency — and the dense road cost matrix runs
**N backward sweeps for N riders**, not N×M point-to-point routes. At N=M=200 that is 200 searches
instead of 40,000.

**Interview soundbite:** "Reverse the graph and one search answers a one-to-many question. It turns
an N×M problem into an N problem."

## 7. Strongly connected components (and why not plain connected)

With one-ways, `u` reaching `v` does **not** imply `v` reaches `u`. Only inside a **strongly**
connected component is every pair mutually reachable.

Every bounded map extract has dead stubs — roads clipped at the boundary, one-way service
driveways. My extract: 96.5% of nodes are in the largest SCC; the other 3.5% would silently poison
every benchmark with failed searches.

**Kosaraju in two passes:** (1) DFS the forward graph, push nodes by finish time; (2) DFS the
reverse graph in reverse finish order — each tree is one SCC. I already had the reverse graph, so
pass 2 was free.

**Write it iteratively.** A recursive DFS down a 27k-node highway chain is a stack overflow, which
is a crash, not an exception.

## 8. CSR (Compressed Sparse Row) — the graph layout to know

```
offsets: [0, 3, 5, 5, 9, ...]   // node v's arcs live in arcs[offsets[v] .. offsets[v+1])
arcs:    [ ................. ]  // one contiguous block
```

Built with a **counting sort**: count arcs per node, prefix-sum into `offsets`, then place. Two
linear passes.

vs `vector<vector<Arc>>`: 28,000 separate allocations and a pointer chase per neighbour lookup — in
the innermost loop of every query. **Interview soundbite:** "CSR trades mutability for locality.
For a graph you build once and query millions of times, that is the right trade."

## 9. Lazy deletion in the priority queue

`std::priority_queue` has no `decrease-key`. The standard workaround:

```cpp
if (settled[v]) continue;   // skip the stale copy
```

Push a node again each time its cost improves, and skip outdated pops. You get duplicates in the
heap (bounded by the edge count), but it is **faster in practice** than maintaining an indexed heap
with handles. Know that a Fibonacci heap gives the better O(E + V log V) bound and is almost always
slower in reality due to constants.

## 10. Geospatial details that bite

- **Haversine, not Euclidean, on lat/lon.** Degrees are not metres.
- **Use the `atan2` form**, not `asin(sqrt(a))`: for near-antipodal points `a` creeps past 1.0 and
  you get a `NaN`.
- **A degree of longitude shrinks by cos(latitude).** Any Euclidean spatial index over raw lat/lon
  is stretched, and "nearest" can be wrong. Scale longitude by `cos(mean latitude)`. ~2.5% error in
  Bengaluru, badly wrong in Oslo.
- **Snapping.** GPS fixes land on rooftops and in car parks, never on a junction. Every query starts
  by snapping to the nearest graph node — which is what the Week 2 quadtree is for.

## 11. The systems lesson: the cost matrix is where the intelligence goes

I swapped the entire meaning of "cost" — map distance → road travel time — and **`assignment.cpp`
and `mcmf.cpp` were not touched**. Week 3 quarantined all geometry in `cost_matrix.cpp`, and this
week proved the boundary was drawn in the right place.

**Interview soundbite:** "The optimiser should not know what it is optimising. Keep the cost model
behind one interface and you can change the objective — distance, time, price, driver fairness,
carbon — without touching the solver."

## 12. The result to actually lead with

Routing changed **63% of pairings** and cut **9.5% of total rider waiting**, with the *same* solver.
Mean detour factor **1.51×** — straight-line distance was under-stating real trips by half.

## 13. The uncomfortable finding (bring this up before they do)

**Total wait improved while the worst individual wait got worse** (8.7 → 9.3 min).

Not a bug. **Minimising a sum says nothing about its largest term.** The solver will make one rider
wait 9 minutes to save 30 seconds each for twenty others. If the product needs a per-rider ceiling,
that is a **constraint to add to the model** — drop edges above a cutoff, or minimise a convex
function of wait (which penalises long waits superlinearly) — not something optimality provides for
free.

**This is the sum-vs-max / utilitarian-vs-fairness tradeoff**, and naming it unprompted is a strong
signal in a system design interview.

---

## Self-test

1. Why is Dijkstra correct, and what exactly breaks with a negative edge?
2. State the admissibility condition. Why must `h` divide by the network's *fastest* speed rather
   than the local road's?
3. Your A* returns a different cost than Dijkstra on one pair. What is wrong?
4. You need every driver's ETA to one rider on a one-way network. How many shortest-path searches,
   and in which direction?
5. Why strongly connected components rather than connected components?
6. Describe CSR and what it trades away.
7. Why does `std::priority_queue` need lazy deletion, and what is the cost?
8. Your A* settles 2× fewer nodes but runs the same speed. Name two plausible causes.
9. Total waiting time went down but one rider waits longer. Is the solver broken? What would you
   change?
