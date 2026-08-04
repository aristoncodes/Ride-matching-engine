# Learnings — Week 5 (Advanced Testing + Johnson Potentials)

The testing week, which turned into a performance week. Concepts and interview-ready takeaways.
Report: [week5.md](week5.md)

## 1. The oracle pattern — the single most useful testing idea here

**Test a fast, clever implementation against a slow, obvious one.**

| Under test | Oracle |
|---|---|
| Quadtree nearest / k-nearest / range | Linear scan, full sort |
| MCMF optimal assignment | Exhaustive search (≤ 8×8) |
| Dijkstra | **Bellman-Ford** |
| A* | Dijkstra |
| Sparse cost matrix | Dense cost matrix |

The requirement is that the oracle **shares no code** with the thing it checks. Bellman-Ford has no
heap, no settled set, no early exit — so none of Dijkstra's failure modes can be shared. Two
implementations can both be wrong; they are unlikely to be wrong *the same way*.

**Why it matters most here:** these bugs are *silent*. A suboptimal matching is still a legal
matching — every driver used once, a plausible total. A wrong nearest neighbour is still a real
point. You cannot eyeball either. Only an independent optimum can tell them apart.

**Interview soundbite:** "For anything optimising or indexing, I write a brute-force oracle and
fuzz against it. Property-based comparison finds what hand-written expectations never will."

## 2. Randomised testing with a fixed seed

```cpp
std::mt19937 rng(12345);   // fixed seed => a failure is reproducible
```

3000 random matrices beat 30 hand-written cases — but only if a failure can be re-run. Random seeds
from the clock give you a red CI you cannot reproduce.

**Compare the right thing.** When ties exist, the *specific* answer is ambiguous — several
assignments can be equally optimal. So I assert on **total cost and match count**, never on which
particular driver got which rider. Over-specified tests fail on legal behaviour.

## 3. Edge cases that actually catch bugs (not the checklist ones)

- **Empty**, **single element** — the boundaries of every loop.
- **All-coincident points** — subdividing can *never* separate them, so a quadtree without a depth
  cap recurses until the stack dies.
- **Exactly on a boundary** — especially the centre point, which lies on the dividing line of all
  four children. A wrong `<` vs `<=` stores it twice or loses it.
- **Zero as a legitimate value** — a driver already at the door costs 0. Any code treating `0` as a
  "no edge" sentinel is now wrong. **Sentinel values that collide with real data are a classic bug
  class.**
- **Sustained churn** — 5000 insert/removes. The quadtree's failure mode is slow corruption, not an
  immediate crash, so a single operation proves nothing.
- **Asymmetry** — I test one-way streets specifically because on an *undirected* graph a
  forward/backward direction bug is invisible.

## 4. Benchmarks that assert, not benchmarks that print

**A timing that only prints is a timing nobody reads.** Rules I settled on:

- **Median of N runs.** Not the mean (one descheduled run dominates it), and not the minimum (the
  best case is not what production sees).
- **State the input size.** "Sub-millisecond" is meaningless without it.
- **Record the sizes that FAIL too.** Sub-ms held at N=M=100 and broke by 150 — publishing only the
  passing number is how a benchmark becomes marketing.
- **Never assert timings under sanitizers or `-O0`.** A 2–3× slowdown makes every budget noise.
  (Also: CMake defaulting to *no* build type means `-O0`, so every unconfigured benchmark measures
  unoptimised code.)
- **Modest headroom.** A budget with 10× slack is not a budget, it is decoration.

**Separate `correctness` from `performance` in CTest.** Red-correctness means *wrong*;
red-performance means *slow*. Different responses, so they should not fail as one thing.

## 5. Johnson potentials — the technique of the week

**Problem:** min-cost max-flow needs a shortest path per unit of flow, but the residual graph has
**negative edges** (cancelling flow refunds its cost). Dijkstra cannot run on it. Bellman-Ford can,
at O(V·E) *per augmentation* — with one augmentation per matched rider, that is where the whole
solve went.

**The trick:** keep a potential `pot[v]` (cheapest known cost to reach `v`) and search on the
**reduced cost**

```
cost'(u → v) = cost(u → v) + pot[u] − pot[v]
```

**Why it is ≥ 0:** `pot[v] ≤ pot[u] + cost(u,v)` is exactly the triangle inequality that shortest
paths already satisfy. Rearrange it and you have non-negativity — which is all Dijkstra needs.

**Why the answer is unchanged:** along any path the `pot` terms **telescope** — each intermediate
cancels in a pair — leaving `realCost = reducedCost + pot[sink] − pot[source]`. Every path shifts by
the same constant, so the *cheapest* path is the same one. **It is a change of coordinates, not an
approximation.**

**Bootstrapping:** you need one Bellman-Ford pass first, because the *initial* graph may have
negative edges. One O(V·E) pass up front, then Dijkstra forever after. After each augmentation,
`pot[v] += dist[v]` rolls the potentials forward.

**Interview soundbite:** "Johnson's reweighting makes negative edges non-negative without changing
which path is shortest, because the potentials telescope. It is what lets min-cost flow use Dijkstra
instead of Bellman-Ford."

## 6. The lesson that mattered most: the "obvious" optimisation was wrong half the time

Week 3 left a note: *"SPFA is O(F·V·E); Dijkstra-with-potentials is the planned speedup."* I
implemented it. **The sparse solve got 2.4× slower.**

| N=M=1000 | edges/node | SPFA | Dijkstra |
|---|---|---|---|
| k=8 sparse | 5 | **56 ms** | 152 ms |
| k=64 | 33 | **347 ms** | 356 ms |
| k=96 | 49 | 482 ms | **436 ms** |
| dense | 500 | 8206 ms | **3576 ms** |

**Why:**
- **SPFA** re-relaxes an edge once per improvement — its work grows with **density**. On a sparse
  layered graph the frontier stays tiny and it is nearly O(E) with small constants.
- **Dijkstra** settles each node exactly once, but pays a log-factor heap and an O(V) reset per
  augmentation — pure overhead when E is small.

**Asymptotics describe the limit, not your input.** SPFA's worst case is far worse; on *this*
workload it wins by 2.7×.

The crossover sat between **33 and 49 edges/node at every size tested** — so `Auto` switches at
**40**, a number from a measurement sweep rather than from taste.

**Interview soundbite:** "I implemented the textbook speedup and benchmarked it. It was 2.3× faster
on dense inputs and 2.7× *slower* on sparse ones, so I made the engine pick by measured density
rather than shipping the version that looked better on paper."

## 7. When you add a fast path, prove it changes nothing

An engine choice that silently changed answers depending on batch density would be the worst class
of bug — it would only appear at certain loads, in production, intermittently.

So the parameter exists **specifically so the equivalence is testable**, and 1200 random graphs plus
a dense case assert both engines return an identical optimum.

**General rule: any optimisation that adds a branch needs a test that both branches agree.**

## 8. Static initialisation / destruction order (a real C++ trap)

My benchmark results file wrote out **empty**.

```cpp
std::vector<...>& measurements() {
    static std::vector<...> table;   // constructed lazily on FIRST USE
    return table;
}
const MeasurementWriter g_writer;    // constructed at load time — EARLIER
```

Statics are destroyed in **reverse order of construction**. `table` is constructed *later* (first
call), so it is destroyed **first** — and `~MeasurementWriter` then read a dead vector.

**Fix:** heap-allocate and intentionally never free it (`static auto* table = new ...`). Leaking one
vector at process exit costs nothing, and the object is now guaranteed alive for any destructor that
runs later.

Related and worth knowing: the **static initialisation order fiasco** across translation units —
construction order between TUs is undefined. The function-local static ("Meyers singleton") solves
*that* one, and is what created *this* one.

## 9. Vendor vs fetch your test framework

Catch2 **v2** is one header with no build system of its own. Vendoring it in `third_party/` instead
of using CMake `FetchContent` means a clean checkout builds and tests **offline**, including in a CI
container with no network egress.

**A test suite that needs the internet to run is a test suite that will be skipped.**

## 10. Sanitizers

```bash
cmake -DENABLE_SANITIZERS=ON -DCMAKE_BUILD_TYPE=Debug ..
```

- **ASan** — use-after-free, buffer overflows, leaks. ~2× slower.
- **UBSan** — signed overflow, bad casts, misaligned access: things that "work" until a compiler
  upgrade changes its mind.

Run the *whole* suite under them, not a smoke test. My highest-risk code is raw pointer arithmetic
into CSR spans and heavy `unique_ptr` churn in the quadtree — exactly where a use-after-free hides.

**Interview soundbite:** "Sanitizers catch the bugs that pass all your tests today and crash in
production next quarter. They belong in CI, not on someone's laptop."

---

## Self-test

1. What makes an oracle a valid test, and why is Bellman-Ford the right oracle for Dijkstra?
2. Your randomised matching test fails once in CI and never again. What did you do wrong?
3. Why should you assert on total cost rather than on the specific pairing?
4. Name three edge cases that break spatial code, and what specifically breaks in each.
5. Why is `0` a dangerous sentinel in a cost matrix?
6. State the reduced-cost formula. Why is it non-negative, and why does the shortest path not change?
7. Why does min-cost max-flow need one Bellman-Ford pass before it can use Dijkstra?
8. SPFA beat Dijkstra-with-potentials on your sparse input. Explain, and say how you would decide
   which to ship.
9. A `static` object's destructor reads another `static` and finds garbage. Why, and what is the fix?
10. Why should timing assertions be skipped under sanitizers?
