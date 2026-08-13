# Ride-Matching Engine

A B2B geospatial ride-matching system that pairs riders to drivers **optimally**
rather than greedily, prices those pairings in **real road travel time**, and
does not lose a ride request when things crash.

Polyglot by design: **C++** for the CPU-bound graph maths, **Go** for the
I/O-bound network layer, joined by gRPC.

```
                    ┌─────────────┐
   riders  ──REST──▶│  requestd   │──┐
                    └─────────────┘  │
                                     ▼
                            ┌──────────────────┐
                            │  Redis Streams   │   durable ride-request queue
                            └──────────────────┘
                                     │
                                     ▼
  drivers ──WebSocket──▶┌─────────┐  ┌──────────┐  ──gRPC──▶ ┌──────────────┐
                        │ ingestd │  │ batcherd │            │ matching     │
                        └────┬────┘  └────┬─────┘ ◀──────────│ engine (C++) │
                             │            │                  └──────────────┘
                             ▼            │                    quadtree +
                    ┌──────────────────┐  │                    min-cost max-flow
                    │  Redis GEO       │◀─┘                    + A* routing
                    │  live locations  │   candidate lookup
                    └──────────────────┘
```

---

## Quickstart

**Prerequisites:** Docker. That is all — everything else builds inside containers.

```bash
git clone https://github.com/aristoncodes/Ride-matching-engine.git
cd Ride-matching-engine
docker compose up -d          # ~2 min first time; builds C++ and Go images
```

Wait for all five services to report healthy:

```bash
docker compose ps
```

Submit a ride request:

```bash
curl -X POST localhost:8081/v1/ride-requests \
  -H 'Content-Type: application/json' \
  -d '{"rider_id":"R-1","pickup":{"lat":12.9716,"lng":77.5946}}'
# {"request_id":"req_...","status":"PENDING"}
```

Nothing will match it yet — there are no drivers. Start some:

```bash
cd infrastructure
go run ./cmd/mockdrivers --url ws://localhost:8080/v1/drivers/stream \
  --drivers 200 --interval 1s --duration 60s
```

Then submit again and watch it match:

```bash
curl -s localhost:8082/stats     # batcherd: batches, matched, queue depth
docker compose logs -f batcherd  # per-batch: riders, drivers, match_rate, solve_ms
```

**Local development without Docker** needs Go 1.26, CMake, gRPC/protobuf and
Redis — see the Makefile (`make check` verifies your toolchain).

---

## What is actually interesting here

### Matching is provably optimal, not greedy

Greedy assignment — every rider takes their nearest driver — is locally sensible
and globally poor. On a two-rider example it scores **101 against an optimal 4**.
The engine models the batch as min-cost max-flow and is verified against a
brute-force oracle over 3,000 randomised cases.

### Cost is road travel time, not straight-line distance

Straight-line distance ignores rivers, one-ways and motorways. Measured on a real
OpenStreetMap extract of central Bengaluru (27,890 nodes), the **mean detour
factor is 1.51×** — straight-line distance understates real trips by a third.

Switching the cost model **changed 63% of pairings and cut total rider waiting by
9.5%**, using the same solver.

### It does not lose requests

The Week 16 chaos test deletes **every** engine pod mid-traffic:

```
submitted & accepted   : 200
distinct in the queue  : 200
pending / undelivered  : 0 / 0
dead-lettered          : 0
→ ZERO DROPPED REQUESTS across a full engine outage
```

That works because the engine is a separate process (a crash is an error value,
not a dead service), requests live in a durable stream, and **a request is acked
only after it is matched**.

### Tenants are genuinely isolated

The tenant comes from the authenticated API key and **never** from the request.
`internal/tenancy` is a test suite written from the attacker's side: one client
holding tenant A's key tries to spoof a header, read a competitor's drivers,
consume their queue, and read their dead-letter stream — and is denied at every
layer.

---

## Benchmarks

Measured on an Apple M-series laptop. Absolute numbers are machine-specific; the
**ratios** are what should hold anywhere. All reproducible — see below.

| | Result |
|---|---|
| Coordinates → provably optimal assignment (N=M=100, k=8) | **0.40 ms** |
| Quadtree vs brute-force nearest neighbour (N=500k) | **~160× faster** |
| 32× more drivers (500 → 16,000) | **~2× time** — the shortlist works |
| A* vs Dijkstra, 200 routes | **1.82× fewer nodes**, identical costs |
| 10,000 concurrent drivers | connected in 1.6 s, **0 failures** |
| Request accept latency at that load | p50 **405 µs**, p99 **8.35 ms** |
| Distributed locking, 40 concurrent workers | global lock 3/40 → **geohash-partitioned 40/40** |

### Where the claims turned out to be wrong

The TDD claimed matching is **O(N log M)**. Measuring it properly:

- **M (drivers): confirmed.** 32× the drivers costs ~2× the time.
- **N (riders): refuted.** 32× the riders costs ~100× the time — about
  **O(N^1.33)**, because min-cost max-flow runs one augmentation per matched
  rider. The `log M` term is real but belongs to the *candidate lookup*, not the
  solve.

That finding changed a decision: `MAX_BATCH` is a **latency control**, not just a
memory bound — four 800-rider batches take ~158 ms against ~328 ms for one
3200-rider batch.

Full numbers: [Benchmarks.md](docs/Benchmarks.md) ·
[Load_Test_Sweep.md](docs/Load_Test_Sweep.md) ·
[Load_Test_Pipeline.md](docs/Load_Test_Pipeline.md)

---

## Running the proofs yourself

```bash
# C++: correctness, contract, performance, and two end-to-end anchors
cd matching_engine && cmake -B build -S . && cmake --build build -j
cd build && ctest                                  # 5/5

# C++ under AddressSanitizer + UndefinedBehaviorSanitizer
cmake -B build-asan -S . -DENABLE_SANITIZERS=ON && cmake --build build-asan
./build-asan/unit_tests "~[perf]"

# Go: ~11 packages, race detector on
cd infrastructure && go test -race ./...

# Kubernetes + the chaos test (needs Docker)
./k8s/deploy.sh
./k8s/chaos-test.sh

# Load and profiling
./scripts/loadprofile.sh steady        # or spike / soak
./scripts/profile.sh baseline 30
```

---

## Layout

```
matching_engine/     C++ engine
  src/quadtree       spatial index                      (Week 2)
  src/mcmf           min-cost max-flow, domain-agnostic (Week 3)
  src/assignment     rider/driver flow model            (Week 3)
  src/cost_matrix    the ONLY file that touches geometry
  src/road_graph     CSR road network + A*/Dijkstra     (Week 4)
  src/matching_server  gRPC service                     (Week 6)

infrastructure/      Go services
  internal/engine    gRPC client + retryable/poison error taxonomy
  internal/locations Redis GEO live driver store
  internal/ingest    WebSocket ingestion
  internal/pipeline  3-second coalescing window
  internal/queue     durable Redis Streams queue
  internal/batcher   the matcher microservice
  internal/locks     TTL leases, geohash-partitioned
  internal/auth      API keys, rotation, rate limiting
  internal/tenancy   cross-tenant isolation suite
  cmd/               ingestd · requestd · batcherd · loadtest · mockdrivers

k8s/                 kind cluster, manifests, chaos test
docs/                TDD, ADRs, benchmarks, threat model, runbook
weekly report/       24 build reports + 24 learnings write-ups
```

---

## Design decisions

Each significant decision has an ADR with the options considered and what
validated it:

| | Decision |
|---|---|
| [0001](docs/adr/0001-use-quadtree-for-spatial-indexing.md) | Quadtree for spatial indexing |
| [0002](docs/adr/0002-grpc-over-cgo-for-go-cpp-bridge.md) | gRPC over cgo — a C++ crash must be an error, not an outage |
| [0003](docs/adr/0003-optimal-matching-over-greedy.md) | Optimal matching over greedy |
| [0004](docs/adr/0004-redis-geo-for-live-locations.md) | Redis GEO for live locations |
| [0005](docs/adr/0005-batch-in-3s-windows.md) | 3-second batch windows |
| [0006](docs/adr/0006-redis-streams-over-kafka.md) | Redis Streams over Kafka |

Two of these had a premise corrected once the code met reality — ADR-0004
claimed Redis TTLs could expire individual drivers, which is false. The
corrections are recorded in the ADRs rather than quietly fixed.

---

## Documentation

- **[Technical Design Document](docs/Technical_Design_Document.md)** — the
  24-week plan, with every week's checkpoint and the bugs found along the way
- [Architecture](docs/Architecture.md) · [Data Model](docs/Data_Model.md) ·
  [API](docs/api/) · [Threat Model](docs/Threat_Model.md)
- [Observability](docs/Observability.md) — metrics, SLOs, alerting rules
- [Runbook](docs/Runbook.md) · [Rollout & Rollback](docs/Rollout_Rollback.md)
- **[Weekly reports](weekly%20report/)** — what was built each week, what broke,
  and the concepts behind it

---

## Status

Weeks 1–24 complete. Built as a learning project with production discipline:
every performance claim has a committed measurement, every architectural decision
has an ADR, and the bugs found along the way are documented rather than tidied
away — including the ones where the original plan turned out to be wrong.
