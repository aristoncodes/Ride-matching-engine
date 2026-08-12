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

#### Week 5 · Aug 6, 2026 · Advanced Testing — ✅ Complete

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

#### Week 6 · Aug 13, 2026 · Go, Goroutines, cgo/gRPC — ✅ Complete

**Baseline deliverable:** A Go wrapper that passes mock data to the C++ engine and receives the match array back.
*Intent: let Go drive the C++ brain without the two crashing each other.*

- ✅ **gRPC over cgo, and now validated** (ADR-0002 moved Proposed → **Accepted**). `matching_server` runs the engine as its own process; `MatchingService` (`matching_service.{h,cpp}`) implements `SolveBatch`/`Health` on top of the untouched Week 1–5 solver. `graph_registry.{h,cpp}` loads road graphs **at startup** — a ~130 ms parse inside a request would blow every batch deadline — and is immutable afterwards, which is why the service needs no locks at all.
- ✅ **Schema first, and the schema was revised before any code existed.** Reconciling `matching.proto` with what the engine does post-Week-4 produced: typed cost fields (the old untyped `double cost` changed unit with the metric — a genuine cross-language trap), `road_graph_id`, `max_candidates_per_rider`, `max_pairing_cost`, and per-rider `UnmatchedReason`. Removed tags are `reserved`, and the compatibility rules are written into the file.
- ✅ **Every call bounded.** `internal/engine.Client` imposes a deadline (default 2 s, sized against the 3 s batch window) *and* honours caller cancellation — different things: the deadline stops a hung engine, cancellation lets a caller abandon work it no longer needs.
- ✅ **Typed failure taxonomy** (`errors.go`) answering the only question the service layer asks: retry or poison? `ErrEngineUnavailable`/`ErrTimeout` are retryable; `ErrInvalidBatch`/`ErrGraphNotLoaded`/`ErrBatchTooLarge` are not. An unrecognised error defaults to retryable, because dropping a real ride request is worse than retrying a stateless call.
- ✅ **Checkpoint met, automated:** `TestSurvivesEngineCrash` **SIGKILLs** the C++ process mid-run and asserts the Go process survives, gets a typed retryable error, and **recovers by itself** when the engine restarts. An isolation story that needs the Go layer restarted to recover is not isolation.
- ✅ **Tests:** 15 C++ service cases (contract semantics: error codes, unmatched reasons, which fields are set under which metric) + 9 Go integration tests against the real binary. No mocks at the boundary — a mock returning `Unavailable` only proves the mock was written to.
- **Deliberately deferred:** `road_distance_meters` is left unset in batch responses. Recovering it means routing every matched pair a second time (~0.6 ms each, doubling batch cost) for a number the caller does not need to dispatch. The field is `optional` precisely so absent is a legal answer.

#### Week 7 · Aug 20, 2026 · Redis GEOADD/GEORADIUS — ✅ Complete

**Baseline deliverable:** A Redis instance plus Go code to store driver locations and perform spatial updates.
*Intent: a fast, shared store of live driver locations.*

- ✅ **Interface first.** `locations.Repository` is defined before the Redis implementation, so the ingestor and batcher depend on the interface and are tested against an in-memory fake with no Redis at all.
- ✅ **TTL, solved properly.** Redis has **no per-member expiry** — a geo set is a sorted set, and `EXPIRE` on it would drop every driver in the city at once. So freshness lives in a companion ZSET (`drivers:seen:<tenant>`, score = last-ping unix ms), making "is this driver fresh?" an O(log N) score range. Reads filter by it **and** a background reaper deletes by it: filtering alone leaves dead drivers in memory forever, reaping alone serves stale drivers between sweeps. Both are required.
- ✅ **Resilience.** Pooling (`PoolSize`), dial/read/write timeouts (without them a partition blocks a goroutine for minutes), and retry with **exponential backoff plus jitter** — the jitter matters, since without it every goroutine that failed on a Redis restart retries in lockstep and stampedes the recovering server. Retries are restricted to genuinely transient errors: a `WRONGTYPE` is a bug, and retrying it just fails three times as slowly.
- ✅ **Checkpoint met:** `TestStaleDriversAreNotReturned` — with an injected clock, a driver that stops pinging disappears from radius results at the TTL while one that keeps pinging does not, and the stale entry is proven to still occupy memory until reaped.
- ✅ **Tests:** 10 cases against a **real redis-server** on a private unix socket per test. Includes concurrent-writer safety under `-race`, upsert-replaces-not-appends, and Redis being killed mid-flight.
- **Bug worth recording:** tests failed only for those with LONG NAMES. A unix socket path is capped at `sun_path` = 104 bytes on macOS, and Go's `t.TempDir()` embeds the test name, so the socket path length — and therefore the failure — was a function of the test's name.

#### Week 8 · Aug 27, 2026 · WebSockets in Go — ✅ Complete

**Baseline deliverable:** A functional WebSocket server built in Go.
*Intent: accept thousands of live GPS streams without leaking resources.*

- ✅ **Dead-client detection.** Server-side ping every 54 s against a 60 s read deadline (~90%, tolerating one lost ping), with the deadline pushed forward on every pong *and* on any traffic. This is the only thing that finds a phone in a tunnel: TCP will hold the socket open indefinitely.
- ✅ **Limits.** `MaxConnections` is checked **before** the upgrade — a 503 with `Retry-After` is cheaper and kinder than accepting a connection and tearing it down. `MaxMessageBytes` (4 KB against a ~100 byte ping) stops a client announcing a 2 GB frame. Write deadlines stop a client that has stopped reading from pinning a goroutine via TCP backpressure.
- ✅ **No leaks.** Exactly two pumps per connection (gorilla permits one concurrent reader and one writer — more is a race, not a queue), coupled by a per-connection context so whichever notices the connection has ended takes the other down.
- ✅ **Checkpoint met, automated:** `TestNoGoroutineLeakOnClientDisconnect` — **3 goroutines before, 3 after 100 abrupt connect/disconnect cycles.** Plus the inverse test, that a *responsive* client survives 3× `PongWait`, which is what actually catches a bad ping/deadline ratio.
- **Bug the test suite caught:** `Shutdown` timed out. `ReadMessage` does **not** return on context cancel — gorilla has no context-aware read — so an idle connection sat in a socket read until its deadline. The fix is a small goroutine per connection that closes the socket when the context ends, which is the only thing that unblocks a blocked reader.
- **Noted, not built:** outbound server→client push. The write pump exists as a separate goroutine anyway so that when a later week pushes match results to drivers, there is one obvious place for it instead of a race.

#### Week 9 · Sep 3, 2026 · Pipeline Integration — ✅ Complete *(Phase 2 done)*

**Baseline deliverable:** A Go server accepting mock GPS pings every 3 seconds and updating the Redis cache.
*Intent: wire ingestion → cache into one flowing pipe on the 3-second cadence.*

- ✅ **Configurable window** (`--window`, default 3 s per ADR-0005) with per-window throughput logged: pings in, drivers out, flush duration, shed count. `FlushTimeout > Window` is **rejected at construction** — a flush slower than the window means windows overlap and queue, which is precisely the unbounded growth this component exists to prevent.
- ✅ **Coalescing is the core idea.** The buffer is a `map[driverID]DriverLocation`, so ten pings from one driver in a window become one write. This is not lossy: an older GPS fix is not partial information, it is *wrong* information a newer one has already superseded. Measured live: **5,438 pings → 2,000 writes**.
- ✅ **Deliberate, counted shedding.** The bound is distinct drivers, not raw pings; past the cap new drivers are shed and counted, while updates to already-buffered drivers are still accepted (they reuse an entry, so refusing them would discard fresher data for zero memory saving). A failed flush **drops the window** rather than retrying into the next one — re-queueing would grow the buffer during exactly the outage that caused the failure, and every driver pings again in 3 seconds anyway.
- ✅ **The lock is never held across the store write.** The buffer is swapped out under the lock and written outside it; holding it would block every connection goroutine for a Redis round trip, turning one slow dependency into a stalled ingestion layer. Asserted by `TestSlowStoreCannotBlockIngestion`.
- ✅ **Runnable end to end.** `cmd/ingestd` (WebSocket → pipeline → Redis, graceful shutdown in the right order: stop accepting → drain connections → final flush) and `cmd/mockdrivers` (load generator).
- ✅ **Checkpoint met, demonstrated live:**
  - *Normal:* 500 drivers, 5,438 pings, **0 failures**, 500 drivers in Redis, `GEOSEARCH` returning nearest-first with distances.
  - *Overload:* 600 drivers against a 200-connection / 50-driver-buffer limit → **400 refused with 503, 7,287 pings shed, 0 failures, 0 crashes**, buffer never above its cap. Bounded by design rather than by luck.
- **Bug worth recording:** a test comparing `12.97 + 9*0.001` against a runtime-computed `12.97 + float64(9)*0.001` failed while printing two identical numbers. Go folds untyped constant arithmetic at arbitrary precision and rounds once; the runtime version rounds at every step.

---

### Phase 3 — Message Brokering & Batching

**Phase goal:** Build the high-concurrency Go infrastructure to handle B2B traffic spikes.

#### Week 10 · Sep 10, 2026 · Kafka / Redis Streams — ✅ Complete

**Baseline deliverable:** An active message queue configured in Go for incoming ride requests. → **Redis Streams chosen** ([ADR-0006](adr/0006-redis-streams-over-kafka.md)).
*Intent: never lose a ride request, even on a crash.*

- ✅ **Redis Streams over Kafka, recorded as an ADR.** Redis was already a hard dependency, so a stream is one more key rather than a second stateful system in every environment. Kafka's retention, replay and partition throughput are genuinely stronger and are not exercised by anything here. Accessed through a `queue.Queue` interface, which earns its keep immediately: the Week 12 batcher is testable against an in-memory fake.
- ✅ **The asymmetry with driver locations is the design.** GPS pings are **state** — only the latest matters, so Week 9 coalesces and sheds them. Ride requests are **events** — every one is a customer on a street corner, so none may be dropped. Getting these backwards is a serious error in either direction.
- ✅ **Durable consumption.** `XADD` → `XREADGROUP` (NOACK=false) → `XACK`. A claimed-but-unacked message stays in that consumer's Pending Entries List, which is the entire recovery mechanism. **`Reclaim` (XPENDING + XCLAIM) is not an optimisation but the other half of the guarantee** — durability without it just means the request is safely stored and delivered to nobody.
- ✅ **Dead-letter path.** Two routes in: exceeding `MaxDeliveries` (poison that keeps coming back), and a payload that fails to decode (which will fail identically on every redelivery, so retrying it is how a queue stalls). Dead-lettering acks in the same operation, so a poison message stops occupying a consumer slot.
- ✅ **Bounded by construction.** `MAXLEN ~` is mandatory, not optional — an untrimmed stream is the same slow-motion OOM as the Week 9 unbounded buffer. `NewRedisStream` refuses a zero `MaxLen`, a missing consumer name, or a zero `MaxDeliveries`.
- ✅ **Checkpoint met, automated:** `TestUnackedMessageIsRedelivered` — consumer A claims a request and dies without acking; a plain `Consume` from consumer B correctly does *not* see it (">"" returns only never-delivered messages); `Reclaim` recovers it intact with an incremented delivery count. Also asserted: `minIdle` protects a merely-SLOW consumer from having its work stolen, which is the only thing separating "crashed" from "still working".
- **Bug worth recording:** every consumer after the first failed to start. `isGroupExists` compared `err.Error()[:8]` against `"BUSYGROUP"` — nine characters — so the check never matched. An off-by-one in an error-string test, invisible until a second process existed.

#### Week 11 · Sep 17, 2026 · REST API Standards — ✅ Complete

**Baseline deliverable:** A Ride Request Service built in Go to accept rider requests.
*Intent: a clean front door for rider requests.*

- ✅ **One error envelope, everywhere.** `{"error":{"code","message","field","request_id"}}` for every non-2xx, including 404s and 405s that would otherwise be Go's plain text. A B2B client writes its error handling once. `code` is stable and machine-readable; clients branch on it, never on `message`, which gets reworded.
- ✅ **Validation that names what was expected.** Missing/oversized `rider_id`, missing `pickup`, out-of-range or **non-finite** coordinates, wrong types, **unknown fields** (`DisallowUnknownFields` — a client sending `pickupp` has a bug worth surfacing during integration), trailing JSON, and a 16 KB body cap enforced by `MaxBytesReader` so an oversized body is never fully buffered. `LatLng` uses **pointer fields** so a missing `lat` is distinguishable from a real `0` — (0,0) is a genuine coordinate.
- ✅ **Request IDs, propagated and sanitised.** An inbound `X-Request-ID` is honoured so a trace spans services, but is length-capped and stripped of anything but `[A-Za-z0-9._-]` — an unbounded client-controlled string flowing into logs and headers is a log/header-injection vector, and there is a test that proves CRLF cannot inject a header.
- ✅ **Correct status codes.** **202** not 201 (nothing is created; it is queued). **503 + `Retry-After`** not 500 when the queue is unreachable — the request was valid and the fault is ours and transient. Internal error strings never reach the client; the request id is how the log is found.
- ✅ **Liveness vs readiness are different endpoints and behave differently.** `/healthz` deliberately does **not** check Redis: a liveness probe that fails during a dependency outage gets the container killed, which fixes nothing and removes capacity. `/readyz` does check it, pulling the instance out of the load balancer instead. Asserted by a test that fails the dependency and requires the two to diverge.
- ✅ **Panic recovery** as a backstop, so even an unforeseen 500 is structured and traceable rather than a dropped connection.
- ✅ **OpenAPI updated to match the implementation** — the spec previously described a flat `{"error": string}` that the code never produced. A spec that lies is worse than none.
- ✅ **Checkpoint met:** 14 malformed-input cases, each asserting a 4xx (never a 5xx), a machine-readable code, a request id, and that **nothing invalid reached the queue**.

#### Week 12 · Sep 24, 2026 · Microservice Architecture — ✅ Complete

**Baseline deliverable:** A Match Batcher microservice popping requests, aggregating them into 3-second windows, and passing data to the C++ engine.
*Intent: aggregate requests into the windows the C++ engine wants.*

- ✅ **Where every previous week converges:** queue (W10) → driver locations (W7) → C++ engine (W6) → error taxonomy (W6) deciding ack vs requeue vs dead-letter. Runs as its own process (`cmd/batcherd`), horizontally scalable because instances share one consumer group.
- ✅ **Dual-trigger flush, both halves tested separately.** Timer protects a lone rider in a quiet period from waiting for company that never arrives; size protects against a spike building a batch that blows the solve budget and the memory bound. `FlushReason` is recorded per batch, because consistently size-triggered batches mean the window is too long for the load.
- ✅ **THE correctness property: a request is acked only after it is matched.** Acking on receipt is simpler and silently drops every in-flight request on a crash. An **unmatched** rider is deliberately **not acked** either — they are still waiting, and leaving the message pending retries them in a later window when more drivers may be on shift. That is the "retain and retry" tenet made concrete.
- ✅ **Failure handling driven by the Week 6 taxonomy.** Retryable (engine crashed/timed out) → leave the whole batch unacked for redelivery; not retryable (malformed, missing graph, too large) → dead-letter, because retrying is futile and would block the queue forever. Driver-store failure or zero candidates → requeue, since a driver may come on shift within seconds.
- ✅ **Drivers deduplicated across riders.** A driver near two riders must appear **once**; sending them twice lets the solver believe there are two cars where there is one.
- ✅ **Per-batch metrics:** size, driver count, matched/unmatched, **match rate**, **queue-wait p50** (the rider-facing latency, p50 rather than mean so one reclaimed request cannot hide the typical experience), solve duration, and total duration. Emitted via a hook so tests are deterministic rather than scraping logs.
- ✅ **Checkpoint met:** light load flushes on the **timer** (3 riders, 3 matched, rate 1.00); heavy load flushes on **size** before a deliberately-long window can fire. Verified end to end with all four services running: 30 riders / 44 candidate drivers / 17 matched / solve 1 ms, and the 13 unmatched left correctly pending.
- **Bug found only by running it:** the first batch after every start timed out. `grpc.NewClient` dials lazily, so the first RPC paid TCP + HTTP/2 setup — over a second on a loaded machine, against a `SolveTimeout` of window/2 — while the solve itself takes ~6 ms. Every batcher restart would have lost its first batch to a full reclaim cycle. Fixed by probing `Health` at startup, which both warms the connection and fails fast on a bad address.

#### Week 13 · Oct 1, 2026 · Go Mutexes & Distributed Locking — ✅ Complete *(Phase 3 done)*

**Baseline deliverable:** Distributed locks guaranteeing two riders are never matched to the same driver simultaneously.
*Intent: guarantee two riders never grab the same driver at once.*

- ✅ **Why the solver is not enough.** The C++ engine guarantees no driver is used twice **within one batch** — that is what unit-capacity flow is for. It says nothing about two batchers solving different batches concurrently, both containing the same nearby driver. Each solve is internally correct and the combination dispatches one car to two riders.
- ✅ **Leases, not locks.** Every acquisition carries a TTL. A lock without one is a deadlock waiting for a crash: the holder dies and that driver is unmatchable forever, with nothing able to distinguish "in use" from "abandoned". `Extend` renews for legitimately long work, so the TTL can stay short instead of being sized for the worst case.
- ✅ **Atomic acquire.** `SET key token NX PX ttl` in one command. The GET-then-SET version has a race wide enough for two batchers to both observe "free" and both write.
- ✅ **Fencing tokens, enforced by Lua.** Release and Extend run a script that checks the token before acting, atomically. Without it: holder A stalls, its lease expires, B acquires, A wakes and `DEL`s **B's** lease — the driver double-booked by the very mechanism meant to prevent it. There is a test for exactly that sequence.
- ✅ **Geohash partitioning, and why geohash specifically.** Contention is **spatial**: two batchers collide precisely when working the same neighbourhood, because that is when their candidate sets overlap. Hashing by driver id would scatter one neighbourhood across every partition and re-serialise everything. A geohash prefix means physical proximity, so a concert letting out contends only with itself.
- ✅ **Checkpoint met, both halves:** 100 goroutines racing for one driver behind a barrier → **exactly 1 winner, 99 clean `ErrNotAcquired`**. A crashed holder's lease **self-releases** and the driver becomes acquirable again.
- ✅ **"Prove it scales" — measured, not asserted:** 40 concurrent workers, **one global lock: 3/40 acquired concurrently in 11.0 ms. Geohash-partitioned: 40/40 in 3.1 ms** (spread 0.90). ~13× the concurrency and 3.5× the wall clock, on the named top risk.
- **Test bug worth recording:** the contention test first reported a spread of 0.28 and failed. The riders were spaced 0.02° (~2.2 km) apart while a precision-5 geohash cell is ~4.9 km, so most "distinct" riders shared a partition. That was the test being unrealistic, not the partitioning being broken — two riders 2 km apart genuinely *are* in one neighbourhood and *should* contend.

---

### Phase 4 — DevOps & Benchmarking

**Phase goal:** Prove the hybrid system works under intense simulated pressure.

#### Week 14 · Oct 8, 2026 · Docker Compose Basics — ✅ Complete
**Baseline deliverable:** A `docker-compose.yml` booting up the Go services, the C++ engine, and Redis together.
*Intent: one command boots the whole polyglot stack.*

> **Amended Aug 13, 2026.** This week originally said "Go, C++, Redis, **and Kafka**", written in Week 0 before the broker was chosen. [ADR-0006](adr/0006-redis-streams-over-kafka.md) (Week 10) selected **Redis Streams over Kafka** specifically so there would be no second stateful system to run — and avoiding exactly this compose file was one of the stated reasons. Including a Kafka broker that nothing connects to would contradict the architecture and make the stack heavier for no benefit. The stack is therefore: `redis`, `matching-engine` (C++), `ingestd`, `requestd`, `batcherd`.

- ✅ **Ordered startup.** `depends_on: condition: service_healthy`, not the bare form — plain `depends_on` waits only for the container to START, and Redis accepts TCP before it has finished loading its AOF, so the Go services would race it and fail their first writes. Redis is probed with `redis-cli ping` rather than a TCP connect for exactly that reason.
- ✅ **Reproducible builds.** Every image pinned to a patch version (`redis:8.0.3-alpine`, `debian:bookworm-20250630-slim`), and every apt package version-pinned to what those bases actually ship. Config lives in `.env` (see `.env.example`), never in the compose file.
- ✅ **Multi-stage everywhere.** C++: a builder with gRPC's dev headers (~1 GB) produces a **179 MB** runtime with only the shared libraries. Go: one parameterised Dockerfile for all three services, `CGO_ENABLED=0`, runtime `FROM scratch` → **18–31 MB** images with no shell, no package manager, and nothing to patch for CVEs. All run as a non-root uid.
- ✅ **A healthcheck that works on `scratch`.** There is no shell, so `HEALTHCHECK CMD curl …` cannot run at all. `cmd/healthcheck` is a tiny static Go probe compiled into each image and invoked in exec form. `requestd` probes **`/readyz`** rather than `/healthz`, since a front door with no reachable queue should not receive traffic.
- ✅ **Checkpoint met:** `docker compose up` brought all five services to `healthy` in **12–13 s across three consecutive cold cycles** (`down -v` between each), and the full flow was verified through the containers — 50 drivers over WebSockets, 25 riders over REST, 14 matched, 0 solve errors.
- **Bug found by containerising:** the batcher's startup engine-probe timed out. `Client.Health` caps at the client's 2 s default, and a *first* gRPC connection inside Docker (DNS + TCP + HTTP/2) exceeds it — so the Week 12 cold-start fix silently stopped working in the environment it mattered most. Fixed with a longer client timeout plus retry; the probe now succeeds on attempt 1.

#### Week 15 · Oct 15, 2026 · Load Testing Basics — ✅ Complete *(current position — Phase 4 done)*

**Baseline deliverable:** A Go script simulating 10,000 concurrent drivers, proving latency is near O(N log M).
*Intent: turn the performance goals into measured evidence.*

- ✅ **`cmd/loadtest`, two modes.** `--mode=pipeline` drives the real stack end to end (WebSockets + REST); `--mode=sweep` talks straight to the engine over gRPC to test the algorithmic claim without network noise. Percentiles come from the full sample set (exact, nearest-rank), not a running mean.
- ✅ **Percentiles, not averages.** **10,000 concurrent drivers connected, 0 failed, 0 refused, in 1.6 s.** Request latency: **p50 405 µs, p95 3.14 ms, p99 8.35 ms**, max 19.76 ms over 2,000 requests at 399/s — 2,000 accepted, **2,000 matched, 0 errors**. Reports committed to [Load_Test_Pipeline.md](Load_Test_Pipeline.md) and [Load_Test_Sweep.md](Load_Test_Sweep.md).
- ⚠️ **The complexity claim is HALF WRONG, and the report says so.** Sweeping N and M independently:
  - **M (drivers): confirmed, emphatically.** 32× the drivers (500 → 16,000) costs only **~2× the time** — each doubling multiplies by 1.02–1.27 against the 2.0 an O(N·M) design would show. The Week 2 quadtree shortlist does exactly what it was built for.
  - **N (riders): the claim is wrong.** Doubling N multiplies time by 1.5–4.7, and 32× the riders costs **~100× the time (≈O(N^1.33))**. This is not a bug but a misdescription: Successive Shortest Paths runs **one augmentation per matched rider**, each a shortest-path search over a graph with O(N·k) edges, so the solve is nearer **O(N²k log N)**. The `log M` term is real and belongs to the *candidate lookup*, not the solve.
  - **Dense control:** ~O(N^2.9) — at N=M=800 a dense solve takes **~2 s**, past the batch window on its own. That is the measured justification for the sparse path.
- ✅ **The finding changed an operational decision.** Because cost is superlinear in N, **`MAX_BATCH` is a latency control, not merely a memory bound**: one 3200-rider batch takes ~328 ms, while four 800-rider batches total ~158 ms for the same riders — faster, at some cost in match quality since each optimises over a smaller pool.
- ✅ **Checkpoint met:** a committed report shows p99 at 10k drivers and the measured curves — including where they contradict the original claim.

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
