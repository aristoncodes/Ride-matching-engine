# Test Strategy

**Status:** Draft · **Related:** [SLO_SLA.md](SLO_SLA.md), [Technical_Design_Document.md](Technical_Design_Document.md)

How we prove the system is **correct**, **fast**, and **resilient** — with the specific "correctness anchors" for the hard algorithms.

---

## 1. Testing pyramid

```
        ▲  fewer, slower, higher-confidence
   E2E / chaos          (kill-the-pod, full pipeline)
   Load / performance   (10k drivers, p99 latency)
   Integration          (Go↔C++ gRPC, Go↔Redis/queue)
   Unit                 (quadtree, matcher, router, handlers)
        ▼  many, fast, run on every commit
```

## 2. Correctness anchors (the non-negotiables)

Each hard algorithm ships with a way to **prove** it's right, not just observe it passing:

| Component | Anchor |
|-----------|--------|
| **Quadtree** | Brute-force nearest-neighbor cross-check on random inputs → 0 mismatches. ✅ *Already done for the current insert/query.* |
| **Bipartite matcher** | Brute-force every permutation on small N (≤ 8), assert solver hits the true optimum cost. |
| **Router (Dijkstra/A\*)** | A\* cost == Dijkstra cost on the same source/target pairs; hand-checked tiny graphs. |
| **Batcher** | Property: every ingested request ends in exactly one terminal state (matched/unmatched/cancelled), none lost. |

## 3. Test levels

### Unit (every commit)
- C++: **GoogleTest or Catch2** via CTest.
- Go: standard `testing` + table-driven tests.
- **Edge cases on purpose:** empty input, single point, all-coincident points, on-boundary points, N≠M, zero drivers, one driver many riders.

### Integration
- Go↔C++ over a real gRPC channel (not mocked) with a test engine binary.
- Go↔Redis and Go↔queue against ephemeral containerized instances.
- Contract tests validating requests/responses against [api/](api/).

### Load / performance (Weeks 15, 21)
- Simulate 10k concurrent drivers; measure **p50/p95/p99**, not averages.
- Plot latency vs. N; assert it tracks **O(N log M)**, not O(N·M).
- Assert SLO #1 (compute p99 ≤ 1ms) and record the number in the repo.

### Chaos / resiliency (Week 16)
- Kill the C++ worker mid-batch → assert **0 dropped requests** (redelivery works).
- Kill a Go pod under load → assert self-healing, no lost data.
- Drop Redis/queue → assert graceful degradation, not a crash loop.

### Security (Week 19)
- **Cross-tenant denial test:** authenticate as Tenant A, attempt to read Tenant B at every layer → all denied.
- API-key: revoked key rejected immediately; rate limit enforced.

## 4. Tooling & gates
- **Sanitizers:** run the C++ suite under **AddressSanitizer / UBSan** in CI.
- **CI gate (Week 17):** merge blocked unless build + unit + integration + sanitizers + lint pass.
- **Coverage:** track, but treat correctness anchors and edge cases as the real signal, not a coverage %.

## 5. Test data
- Reproducible via the Week 1 generator's `--seed` flag — every perf/correctness run uses fixed seeds so results are comparable across commits.

## 6. Definition of "tested" for a week
A week's deliverable isn't done until: its unit tests pass, its correctness anchor (if it has one) is green, and any performance claim it makes is backed by a committed measurement.
