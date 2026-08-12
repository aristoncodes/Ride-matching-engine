# Technical Design Document: B2B Geospatial Ride-Matching & Dynamic Pricing Engine

**Document Owner:** Aditya Yadav
**Target Completion:** 24 Weeks (Paced at 5–8 Hours/Week)
**Primary Languages:** C++ (Core Algorithmic Engine) & Go (Network/Microservices)

---

## 1. Introduction

This document outlines the technical design and implementation timeline for a highly concurrent, B2B Geospatial Ride-Matching and Dynamic Pricing Engine. The system utilizes a polyglot architecture: a lightning-fast C++ brain for complex bipartite graph matching and spatial partitioning, orchestrated by a robust Go (Golang) microservice layer for real-time WebSocket state ingestion and high-throughput message brokering.

## 2. Goals

- **Algorithmic Supremacy:** Pair thousands of riders and drivers in sub-millisecond times using advanced competitive programming graph algorithms (Min-Cost Max-Flow / Hungarian Algorithm).
- **Real-Time Data Ingestion:** Handle massive concurrent streams of GPS pings via WebSockets and Redis Spatial indices (GEOADD/GEORADIUS).
- **Enterprise Multi-Tenancy:** Ensure strict database isolation, API key management, and data segregation so multiple institutional clients can use the engine simultaneously.
- **Resiliency:** Wrap the architecture in Kubernetes (K8s) and message brokers (Kafka/Redis Streams) to guarantee zero dropped ride requests during traffic spikes.
- **Timeline:** Complete production-ready implementation within 24 weeks based on a 5–8 hour weekly commitment.

## 3. Tenets

- **Right Tool for the Right Job:** C++ will strictly handle CPU-bound computational math (graphs, quadtrees). Go will strictly handle I/O-bound network concurrency (WebSockets, API routes, Redis connections).
- **Batching over Streaming for Math:** Ride requests will be aggregated in 3-second windows before being passed to the C++ engine to optimize matrix calculations.
- **Fail-Safe Orchestration:** If the C++ worker crashes, the Go message queue must retain the ride requests and safely retry upon container restart.

## 4. State of the System (Context)

Standard ride-sharing backends often struggle under peak loads (e.g., a massive concert ending) because computing the optimal driver-rider pairings in a dense area is an O(N × M) problem if done naively. Doing this math in a high-level garbage-collected language leads to massive latency spikes. By isolating the graph math in a highly optimized C++ binary and feeding it batched geospatial data via a highly concurrent Go bridge, we achieve enterprise-grade scale at a fraction of the compute cost.

## 5. Implementation Plan (Week-by-Week)

This is the single source of truth for the 24-week schedule. Each week states its **date**, **completion status**, and **baseline deliverable**, followed by its **intent**, the concrete additions that form the **"definition of done"**, and a one-line **checkpoint** for deciding the week is truly finished. Items tagged **_(stretch)_** are optional when the weekly hours run short — everything else is part of a complete deliverable.

---

### Phase 1 — The Core Algorithmic Engine (C++)

**Phase goal:** Build the raw computational brain using competitive programming paradigms.

#### Week 1 · Jul 9, 2026 · Repository Setup — ✅ Complete
**Baseline deliverable:** A script generating random driver and rider coordinates.
*Intent: produce the raw, reproducible datasets every later stage depends on.*

- ✅ **Parameterize everything.** `N` (`-N/--riders`), `M` (`-M/--drivers`), `GRID_SIZE` (`-g/--grid`), and the RNG seed (`-s/--seed`) are **CLI arguments** — no recompiling to change dataset size.
- ✅ **Reproducibility mode.** `--seed` fixes the RNG → byte-identical output on repeat runs; a random run reports its seed on stderr so it can be reproduced later.
- ✅ **Machine-readable output.** `-f/--format` supports `text | csv | json` (JSON verified to parse). Same seed yields identical coordinates across all formats.
- ✅ **Unambiguous IDs.** Riders are `R0..`, drivers are `D0..` — no collision in a combined set (see [Data_Model.md](Data_Model.md)).
- ✅ **Checkpoint met:** the generator produces reproducible, machine-readable data on stdout, pipeable straight into another program. *(The interim matching demo was moved out to `match_demo.cpp` to keep the generator single-purpose.)*

#### Week 2 · Jul 16, 2026 · Spatial Partitioning — ✅ Complete
**Baseline deliverable:** An explicit Quadtree data structure implemented in C++ to partition 2D space.
*Intent: a spatial index that answers "who's near here?" in ~O(log N) instead of scanning everyone.*

- ✅ **Already solid:** memory-safe (`unique_ptr` children, copies deleted), a `MAX_DEPTH` cap that kills infinite recursion on coincident points, and correctness verified against a brute-force scan (0 mismatches).
- ✅ **`remove(point)`.** Removes by id and collapses a subtree of 4 undivided children back into their parent once it fits within `capacity`, so long-running insert/remove churn doesn't bloat the tree.
- ✅ **Convenience API.** `size()`, `clear()`, and a **`nearestNeighbor(point)`** wrapper that hides the expand-and-retry box search — `match_demo.cpp` now calls it directly instead of reimplementing the search inline.
- ✅ _(stretch)_ **Benchmark harness** (`benchmark.cpp`). Quadtree vs. brute-force nearest-neighbor timing at N = 100 .. 500,000, with a checksum cross-check between the two so a correctness regression shows up as a `[MISMATCH]`, not just a suspicious number. Speedup grows cleanly with N (O(log N) vs O(N)): ~0.3x at N=100 (tree overhead dominates at tiny sizes) up to ~160x at N=500,000.
- ✅ **Checkpoint:** insert, query, *and* remove all covered, with the nearest-neighbor search encapsulated behind one clean call.

#### Week 3 · Jul 23, 2026 · Bipartite Graphs — ✅ Complete
**Baseline deliverable:** The Hungarian Algorithm or Min-Cost Max-Flow (MCMF) implemented in C++ from scratch. → **MCMF chosen** (`mcmf.{h,cpp}`), because it handles rectangular N≠M natively and composes with the sparse candidate stretch goal.
*Intent: assign riders to drivers optimally — minimum total cost, each driver used once.*

- ✅ **Separated concerns.** `mcmf.{h,cpp}` is a pure, domain-agnostic Min-Cost Max-Flow primitive. `assignment.{h,cpp}` models the rider/driver problem as a flow network on top of it (source→riders→drivers→sink, unit capacities). `cost_matrix.{h,cpp}` is the *only* file that knows about coordinates/distances — it emits the `MatchEdge` list the solver consumes. The solver never sees a coordinate.
- ✅ **Handles rectangular N×M** natively via the flow model — no dummy padding. Max flow = min(N, M); the imbalance falls out of the graph structure.
- ✅ **Proved optimality.** `assignment_test.cpp` checks the solver against a brute-force min-cost-max-matching oracle over **3000 randomized cases** (all dims 0–8, random costs) plus explicit edge cases (empty, 1×1, N>M, M>N, edgeless-rider, and a greedy-trap where greedy = 101 but optimal = 4). **29,683 assertions, 0 failures.** Wired into CTest (`ctest` green).
- ✅ **Overflow policy defined & documented** (in `assignment.h`): unmatched riders (N>M, or a sparse rider with no candidate edges) return `riderToDriver[i] == -1` with a correct `matchedCount`; the solver never silently drops or mis-assigns. The re-queue-vs-reject decision is explicitly left to the service layer (Week 12 batcher).
- ✅ **_(stretch)_ Sparse cost matrix** done. `QuadTree::kNearest(x, y, k)` feeds each rider only its k nearest drivers → O(N·k) edges instead of O(N·M). Measured on N=M=600: dense solve ~1000ms over 360k edges vs. sparse ~18ms over 4800 edges — a ~55× solve speedup for a small, quantified match-quality tradeoff (sparse may leave a few far-flung riders unmatched). Demo: `optimal_match.cpp`.
- ✅ **Checkpoint met:** solver returns a provably optimal assignment on small cases and a documented leftover policy on large/imbalanced ones.
- ✅ **Known follow-up — CLOSED in Week 5.** The SPFA shortest-path (O(F·V·E)) was the planned Dijkstra-with-potentials target. Johnson potentials are now implemented; the surprise was that Dijkstra is *not* universally faster — it wins on dense matrices (8.2s → 3.6s at N=M=1000) and loses on sparse ones (SPFA is 2.7× faster at k=8). `ShortestPathEngine::Auto` now picks by edge density at a measured crossover of 40 edges/node. See `docs/Benchmarks.md` §5.

#### Week 4 · Jul 30, 2026 · Contraction Hierarchies (Dijkstra / A\*) — ✅ Complete

**Baseline deliverable:** Dijkstra's Algorithm or A\* Search implemented in C++ for accurate travel times.
*Intent: replace "straight-line distance" with real travel time, which is what riders actually feel.*

- ✅ **Real graph.** `osm_loader.{h,cpp}` parses an OpenStreetMap `.osm` extract with no external dependencies (the XML subset OSM uses is tiny; libosmium/expat would cost a dependency for nothing). It honours **one-ways** (`oneway=yes/-1/reverse`, plus untagged `junction=roundabout`), **maxspeed** in km/h and mph, and per-highway-class speed defaults; footways and steps are excluded. `road_graph.{h,cpp}` freezes it into a **CSR** graph — one contiguous block, not a vector-of-vectors — with both forward and reverse adjacency. Data: central Bengaluru, **27,890 nodes / 53,784 arcs**, refetchable via `tools/fetch_road_extract.py`.
- ✅ **A\* done properly.** Binary-heap priority queue, and the **admissible** heuristic `haversine(v, goal) / maxSpeedMps`. Admissible because a great-circle distance is the shortest path that *could* exist (no road is shorter than a straight line) and the network's *fastest* road bounds how quickly it could be covered — so the estimate can never overshoot. Overshooting would let A\* settle a node before its true cheapest path was found, and optimality would be silently lost. The heuristic is **memoised per node**: it was originally recomputed on every relaxation, which cost more than the search it saved.
- ✅ **Path reconstruction + validation.** `RouteResult` returns the node sequence, road length, travel time, and nodes settled. `route_demo` validates **A\* cost == Dijkstra cost on every pair** (200 random pairs, worst difference **0.00 s**) and exits non-zero on any mismatch; it is registered as the `routing_end_to_end` CTest. A\* settles **1.82× fewer nodes** but is only **1.17× faster** — because it does more work per node, and because admissibility forces a weak heuristic (dividing by 70 km/h in a 25–45 km/h city). That gap is what ALT landmarks and true Contraction Hierarchies exist to close.
- ✅ **Strongly connected component extraction.** Iterative Kosaraju over the directed graph. "Strongly" matters: with one-ways, u reaching v does not imply v reaches u. The Bengaluru extract's largest SCC is **26,919 nodes (96.5%)**; the rest are boundary-clipped stubs. Benchmarks sample only from the SCC, or they would measure failed searches instead of routing.
- ✅ **Wired into the matcher.** `buildRoadEdgesDense/Sparse` in `cost_matrix.{h,cpp}` price the matrix in **seconds of driving**, not map distance. Costed with **one backward Dijkstra per rider** — a reverse sweep gives every driver's ETA to that rider at once, so an N-rider batch costs N searches, not N·M. Direction is load-bearing: the driver drives *to* the rider, and on a one-way network those differ. `assignment.cpp` and `mcmf.cpp` were **not touched** — the Week 3 boundary held exactly as designed.
- ✅ **_(stretch)_ Hot-origin cache.** `SourceDistanceCache` precomputes backward one-to-all trees for hot origins (depots, ranks). 4 origins = 16.2 ms build, 0.85 MB; 200 lookups in 0.046 ms vs 372 ms of fresh Dijkstras (~8000×).
- ✅ **Checkpoint met:** two points on the road graph return an optimal route and its travel time, matching Dijkstra's cost exactly. **Result that justifies the week:** road-time matching changes **63% of pairings** and saves **9.5% of total rider waiting** vs distance matching, on the same solver. Mean detour factor **1.51×** — the multiplier by which straight-line distance was lying. See `docs/Benchmarks.md` §3–4.
- **Noted, not fixed:** minimising the *sum* of waits let the *worst* individual wait rise (8.7 → 9.3 min). Not a bug — a sum has no opinion about its largest term. A per-rider ceiling is a constraint to add to the model, and is deferred to the service layer.

#### Week 5 · Aug 6, 2026 · Advanced Testing — ✅ Complete *(current position)*

**Baseline deliverable:** Rigorous C++ unit tests guaranteeing the pairing matrix computes in sub-millisecond time.
*Intent: turn "seems to work" into "provably works, this fast."*

- ✅ **Real framework.** **Catch2 v2**, single header vendored in `third_party/catch2/` rather than fetched at configure time, so a clean checkout builds and tests **offline** — including in a CI container with no network egress. The ad-hoc `src/assignment_test.cpp` `main()` is deleted, every check ported. Suite: **107,598 assertions / 71 cases in 0.12 s**. Split into two CTest entries on purpose — `correctness` red means the engine is *wrong*, `performance` red means it is merely *slow*, and those demand different responses.
- ✅ **Oracles, not self-comparison.** `tests/oracles.h` holds deliberately slow reference implementations that share no code with what they check: linear scan vs quadtree, exhaustive min-cost matching vs MCMF, **Bellman-Ford vs Dijkstra** (no heap, no settled set, no early exit). Two implementations can both be wrong; they are unlikely to be wrong the same way.
- ✅ **Edge cases on purpose.** Empty trees and empty batches; a single point and a single-node graph; **500 coincident points** (the case that recurses forever without the depth cap); points **exactly on boundaries**, including the centre that lies on all four children's dividing lines; N≠M in both directions; a rider with no candidate edges; **zero-cost edges** (which a sentinel bug would read as "no edge"); duplicate edges; 5000-step insert/remove churn; one-way streets, disconnected islands, and `oneway=-1`.
- ✅ **Benchmarks that assert.** `tests/test_performance.cpp` reports the **median of 21 runs** and fails against a stated budget. Headline: **N=M=100, k=8, coordinates → provably optimal assignment in 0.404 ms**, budget 1.0 ms. The *stated size* matters — sub-ms holds through 100 and is lost by 150, and **both** are recorded rather than quoting only the size that passes. Also pinned: the road matrix costs one search *per rider*, so 4× the drivers costs no extra time. Numbers committed to **`docs/Benchmarks.md`**, and written to `benchmark_results.txt` on every run.
- ✅ **_(stretch)_ Sanitizers.** `cmake -DENABLE_SANITIZERS=ON` builds with **ASan + UBSan**; the full suite passes clean. Timing assertions are not registered under sanitizers, since a 2–3× slowdown makes every budget meaningless.
- ✅ **Closed the Week 3 MCMF follow-up.** Johnson potentials implemented (see Week 3 above). The finding was that there is no universally faster engine — density decides — so `Auto` switches at a measured crossover instead of a guessed constant, and **1200 random graphs assert both engines return identical optima**. A speed decision that changes an answer is the worst kind of bug: it would only appear at certain loads.
- ✅ **Checkpoint met:** `ctest` is green (4/4, 2.35 s total), edge cases are covered, and the sub-ms claim is backed by a committed measurement on a stated input size.
- **Bugs this week actually found:** (1) the benchmark results file wrote empty — a **static destruction order** bug, where the lazily-constructed measurement table was destroyed *before* the writer that reads it; (2) `Point` has no default constructor, so `std::vector::resize` will not compile — the same trap `kNearest` hit in Week 2.

---

### Phase 2 — The Go Bridge & State Ingestion

**Phase goal:** Establish the polyglot architecture and real-time data ingestion pipelines.

#### Week 6 · Aug 13, 2026 · Go, Goroutines, cgo/gRPC — ⬜ Not started
**Baseline deliverable:** A Go wrapper that passes mock data to the C++ engine and receives the match array back.
*Intent: let Go drive the C++ brain without the two crashing each other.*

- **Prefer gRPC + Protobuf over cgo.** Running C++ as a separate process behind gRPC gives **failure isolation** — a C++ segfault returns an error instead of taking down the Go process. That directly serves the "fail-safe orchestration" tenet; cgo couples their lifetimes.
- **Schema first.** Write the **`.proto`** before the code and treat it as the binding contract between the two languages.
- **Bound every call.** A **timeout + context cancellation** on each request into C++ so a hung or slow worker can never block the Go layer indefinitely.
- ✅ **Checkpoint:** Go sends a batch, gets the match array back, and survives a deliberately-killed C++ worker with a clean error.

#### Week 7 · Aug 20, 2026 · Redis GEOADD/GEORADIUS — ⬜ Not started
**Baseline deliverable:** A Redis instance plus Go code to store driver locations and perform spatial updates.
*Intent: a fast, shared store of live driver locations.*

- **Hide Redis behind an interface.** Wrap `GEOADD`/`GEORADIUS` in a small repository type so it can be mocked in tests and swapped later.
- **Expire stale data.** Give locations a **TTL** so a driver who stopped pinging ages out instead of being matched as if present.
- **Resilient connections.** **Connection pooling** plus retry-with-backoff on transient Redis errors.
- ✅ **Checkpoint:** driver locations update in Redis and a radius query returns only recently-seen drivers.

#### Week 8 · Aug 27, 2026 · WebSockets in Go — ⬜ Not started
**Baseline deliverable:** A functional WebSocket server built in Go.
*Intent: accept thousands of live GPS streams without leaking resources.*

- **Detect dead clients.** Enforce **read/write deadlines** and **ping/pong heartbeats** — TCP can hold a "connection" open long after the client is gone.
- **Set limits.** Bound **message size** and **max concurrent connections** so one abusive client can't exhaust memory.
- **No goroutine leaks.** One goroutine per connection, each with **clean shutdown on context cancel**.
- ✅ **Checkpoint:** clients connect, stream, and disconnect cleanly; killing a client frees its goroutine.

#### Week 9 · Sep 3, 2026 · Pipeline Integration — ⬜ Not started
**Baseline deliverable:** A Go server accepting mock GPS pings every 3 seconds and updating the Redis cache.
*Intent: wire ingestion → cache into one flowing pipe on the 3-second cadence.*

- **Config the window.** Make the **3-second batch window** a config value, and log per-window throughput (pings in, cache updates out).
- **Backpressure.** If the C++ engine falls behind, **shed load deliberately** (drop-oldest or reject) rather than letting queues grow until the process OOMs.
- ✅ **Checkpoint:** mock pings flow in every 3s, Redis reflects them, and the system stays bounded under overload.

---

### Phase 3 — Message Brokering & Batching

**Phase goal:** Build the high-concurrency Go infrastructure to handle B2B traffic spikes.

#### Week 10 · Sep 10, 2026 · Kafka / Redis Streams — ⬜ Not started
**Baseline deliverable:** An active message queue configured in Go for incoming ride requests.
*Intent: never lose a ride request, even on a crash.*

- **Durable consumption.** Use **consumer groups + explicit acks** so an un-acked message is redelivered after a crash — this is what makes the "retain and retry" tenet real instead of assumed.
- **Dead-letter path.** Route repeatedly-failing ("poison") messages aside so they don't block the queue.
- ✅ **Checkpoint:** kill a consumer mid-process and confirm the request is redelivered, not dropped.

#### Week 11 · Sep 17, 2026 · REST API Standards — ⬜ Not started
**Baseline deliverable:** A Ride Request Service built in Go to accept rider requests.
*Intent: a clean front door for rider requests.*

- **Defensive from day one.** **Input validation, request IDs** (for tracing), and **structured error responses**.
- **Versioned & documented.** Prefix routes (`/v1/...`) and publish an **OpenAPI** spec so B2B clients can integrate.
- ✅ **Checkpoint:** a malformed request gets a clear 4xx with a request ID, not a 500.

#### Week 12 · Sep 24, 2026 · Microservice Architecture — ⬜ Not started
**Baseline deliverable:** A Match Batcher microservice popping requests, aggregating them into 3-second windows, and passing data to the C++ engine.
*Intent: aggregate requests into the windows the C++ engine wants.*

- **Dual-trigger flush.** Flush on the **3-second timeout OR a max batch size**, whichever comes first — protects both latency (small quiet periods) and memory (sudden spikes).
- **Per-batch metrics.** Emit size, latency, and match rate for every batch; you'll need these numbers for Phase 6 tuning.
- ✅ **Checkpoint:** batches form correctly under both light and heavy load, with metrics visible.

#### Week 13 · Oct 1, 2026 · Go Mutexes & Distributed Locking — ⬜ Not started
**Baseline deliverable:** Distributed locks guaranteeing two riders are never matched to the same driver simultaneously.
*Intent: guarantee two riders never grab the same driver at once.*

- **Leases, not locks.** Always attach a **TTL** so a crashed lock-holder can't deadlock a driver forever.
- **Prove it scales.** **Load-test the contention path specifically** — this is a named top risk. Show, with a number, that geohash-partitioned locking reduces contention versus one global lock.
- ✅ **Checkpoint:** concurrent match attempts on the same driver resolve to exactly one winner, and a crashed holder's lock self-releases.

---

### Phase 4 — DevOps & Benchmarking

**Phase goal:** Prove the hybrid system works under intense simulated pressure.

#### Week 14 · Oct 8, 2026 · Docker Compose Basics — ⬜ Not started
**Baseline deliverable:** A `docker-compose.yml` booting up Go, C++, Redis, and Kafka together.
*Intent: one command boots the whole polyglot stack.*

- **Ordered startup.** **Healthchecks + `depends_on: condition: service_healthy`** so Go doesn't start hammering Redis/Kafka before they're ready.
- **Reproducible builds.** **Pin image versions**; keep secrets and config in env files, not committed into the compose file.
- ✅ **Checkpoint:** `docker compose up` yields a healthy Go + C++ + Redis + Kafka stack every time.

#### Week 15 · Oct 15, 2026 · Load Testing Basics — ⬜ Not started
**Baseline deliverable:** A Go script simulating 10,000 concurrent drivers, proving latency is near O(N log M).
*Intent: turn the performance goals into measured evidence.*

- **Percentiles, not averages.** Report **p50 / p95 / p99** latency — tail latency is the entire point of surviving spikes, and averages hide it.
- **Validate the complexity claim.** **Plot measured latency against N** and confirm it tracks O(N log M) rather than O(N·M).
- ✅ **Checkpoint:** a committed report shows p99 latency at 10k drivers and a curve matching the claimed complexity.

---

### Phase 5 — Production Orchestration

**Phase goal:** Ensure B2B reliability, multi-tenancy, and edge-case handling.

#### Week 16 · Oct 22, 2026 · Kubernetes (K8s) Basics — ⬜ Not started
**Baseline deliverable:** Microservices wrapped and managed by Kubernetes to stay online during crashes.
*Intent: the system heals itself when parts die.*

- **Health & scaling.** Define **liveness/readiness probes**, **resource requests/limits**, and an **HPA** for the stateless Go services.
- **Prove resiliency.** Kill the C++ pod under active load and demonstrate **zero dropped ride requests** — this validates the project's core resiliency goal, not just that YAML applies cleanly.
- ✅ **Checkpoint:** a chaos test (delete a pod mid-traffic) shows automatic recovery with no lost requests.

#### Week 17 · Oct 29, 2026 · CI/CD — ⬜ Not started
**Baseline deliverable:** Automated testing and deployment pipelines configured (GitHub Actions / Jenkins).
*Intent: every merge is automatically proven safe.*

- **Quality gates.** Block merges unless **build + unit tests + sanitizers + lint** all pass.
- **Release automation.** Build and push **versioned images on tag**.
- ✅ **Checkpoint:** a red test blocks the merge; a tag produces a deployable image.

#### Week 18 · Nov 5, 2026 · API Key Management — ⬜ Not started
**Baseline deliverable:** Logic handling dropped WebSocket connections, network latency, and basic API key validation.
*Intent: authenticate clients and survive flaky networks.*

- **Key security.** **Hash API keys at rest** (never store raw), support rotation, and **rate-limit per key**.
- **Network resilience.** Graceful **WebSocket reconnect with resumable state**, plus the fallback REST path from the risk table.
- ✅ **Checkpoint:** a revoked key is rejected instantly; a dropped socket reconnects without losing session state.

#### Week 19 · Nov 12, 2026 · Data Segregation — ⬜ Not started
**Baseline deliverable:** Logic handling rider cancellations, driver rejections, and database isolation between institutional clients.
*Intent: institutional clients are fully isolated from one another.*

- **Tenant ID everywhere.** Thread it through **every layer** — API auth, queue partitions, cache key prefixes, and logs.
- **Prove isolation.** Add tests that **confirm tenant A cannot read tenant B's data**. Isolation you haven't tested is isolation you don't have.
- ✅ **Checkpoint:** an automated test attempts cross-tenant access and is denied at every layer.

---

### Phase 6 — Enterprise Hardening

**Phase goal:** Provide institutional clients with proof of internet-scale capabilities.

#### Week 20 · Nov 19, 2026 · Go pprof Profiling — ⬜ Not started
**Baseline deliverable:** Profiling scripts ready to monitor CPU usage, memory allocation, and goroutine blocking.
*Intent: know where the time and memory actually go before touching anything.*

- Capture a **baseline pprof profile + flame graphs first**, so Week 22's optimizations are measured against a real starting point rather than a guess.
- ✅ **Checkpoint:** you can point at the specific functions consuming CPU and allocations.

#### Week 21 · Nov 26, 2026 · Traffic Blasting — ⬜ Not started
**Baseline deliverable:** Intense traffic blasts against the Docker cluster using wrk or Locust.
*Intent: stress the system the way real B2B spikes will.*

- Script **repeatable load profiles** — steady-state, sudden spike, and long soak — instead of one-off manual blasts, so results are comparable run to run.
- ✅ **Checkpoint:** each profile is a rerunnable script with recorded results.

#### Week 22 · Dec 3, 2026 · GC Optimization — ⬜ Not started
**Baseline deliverable:** Bottlenecks resolved to ensure sub-millisecond routing decisions under intense stress.
*Intent: hit the latency target under stress, guided by data.*

- Optimize **only what the profiler flagged**, and **re-run the same benchmark after each change** to confirm it actually helped (and didn't regress elsewhere).
- ✅ **Checkpoint:** routing decisions stay sub-millisecond under the Week 21 stress profiles.

#### Week 23 · Dec 10, 2026 · Go slog & Telemetry — ⬜ Not started
**Baseline deliverable:** Production-grade telemetry, structured logging, and metric tracking for throughput.
*Intent: make the running system observable in production.*

- Expose **Prometheus metrics** and **structured logging via `slog`**, and define the **SLOs** you hold yourself to (e.g. p99 match latency, throughput).
- ✅ **Checkpoint:** a dashboard shows live throughput and latency against explicit SLO thresholds.

#### Week 24 · Dec 17, 2026 · Documentation — ⬜ Not started
**Baseline deliverable:** Final code cleanup, architectural documentation, and polishing the GitHub repository.
*Intent: the artifact that proves internet-scale capability to clients.*

- Ship an **architecture diagram**, a **runnable quickstart**, and a **benchmarks-with-numbers** section in the README — the concrete proof a B2B buyer looks for.
- ✅ **Checkpoint:** a newcomer can clone, run, and understand the system from the README alone.

---

### Cross-Cutting Principles (apply every single week)

- **Test as you build.** Don't defer all testing to Week 5 — each C++ algorithm should ship with its own correctness anchor the week it's written.
- **Commit measured numbers, not adjectives.** "Sub-millisecond" and "O(N log M)" are claims; back each with reproducible evidence checked into the repo.
- **Config over constants.** Nothing performance- or environment-related should be hard-coded — it should be a flag, env var, or config file.
- **Graceful degradation everywhere.** Every external dependency (Redis, Kafka, the C++ worker) needs a defined, tested behavior for when it's unavailable.

## 6. Risks & Mitigations

| Risk | Impact | Mitigation Strategy |
|------|--------|---------------------|
| Cgo/gRPC Serialization Overhead | Data transfer between Go and C++ slows down the system. | Use highly optimized Protobufs or FlatBuffers. Batch requests into 3-second windows to minimize cross-language calls. |
| Distributed Lock Contention | High latency when marking drivers as "busy". | Use Redis Redlock efficiently or partition the locking mechanism based on geographic geohashes. |
| WebSocket Connection Drops | Real-time GPS data becomes stale. | Implement automatic client-side reconnects and a fallback REST endpoint for emergency state updates. |
