# Week 15 — Load Testing (and finding out the complexity claim was half wrong)

**Date:** Oct 15, 2026 · **Phase:** 4 (DevOps & Benchmarking) · **Status:** ✅ Complete
**Phase 4 complete.**

## What this week was about

Turning the project's performance goals into measured evidence — and being willing to publish the
measurement when it disagrees with the goal.

## The harness

`cmd/loadtest`, two modes answering different questions:

| Mode | What it measures |
|---|---|
| `--mode=pipeline` | End-to-end. N drivers on WebSockets while riders POST to REST. What a client experiences. |
| `--mode=sweep` | Algorithmic. Straight to the engine over gRPC, sweeping N and M, no network noise. |

Percentiles come from the **full sample set** — exact, nearest-rank — not a running mean.
Percentiles cannot be computed incrementally without an approximation structure, and at these
volumes the exact set costs a few MB. For a report meant to be believed, that is the right trade.

## Result 1: the pipeline at 10,000 drivers

```
connected=10000 failed=0 refused=0 in 1.616s
connect latency:  p50=27.15ms  p95=55.93ms  p99=71.97ms  max=116.2ms

accepted=2000 rejected=0 failed=0 at 399 req/s
request latency:  p50=405µs   p95=3.14ms   p99=8.35ms   max=19.76ms
```

And the batcher, under that load:

```
requests 2000   matched 2000   unmatched 0   solve_errors 0
batch: riders=500 drivers=8152 matched=500 match_rate=1.00 solve_ms=34
```

**100% match rate, zero errors, p99 of 8.35 ms.** Both flush reasons appeared naturally — `size`
while the burst was arriving, `timer` for the tail.

**Averages would have hidden the interesting part.** The mean request latency was 857 µs while the
p99 was 8.35 ms — a 10× gap. A system reported as "sub-millisecond" is one where 1 rider in 100
waits ten times that.

## Result 2: the complexity claim, tested properly

The TDD claims matching is near **O(N log M)**. That gives two falsifiable predictions: doubling N
should double the time; doubling M should barely move it.

### M — confirmed, emphatically

| M | p50 | vs previous |
|---|---|---|
| 500 | 15.72 ms | — |
| 1000 | 16.04 ms | ×1.02 |
| 2000 | 17.13 ms | ×1.07 |
| 4000 | 20.89 ms | ×1.22 |
| 8000 | 25.49 ms | ×1.22 |
| 16000 | 32.41 ms | ×1.27 |

**32× the drivers costs ~2× the time.** An O(N·M) design would show ×2.0 per doubling. The Week 2
quadtree shortlist does exactly what it was built for: the driver pool can grow enormously without
the matcher noticing.

### N — the claim is wrong

| N | p50 | vs previous |
|---|---|---|
| 100 | 3.27 ms | — |
| 200 | 4.89 ms | ×1.49 |
| 400 | 12.39 ms | ×2.53 |
| 800 | 39.42 ms | ×3.18 |
| 1600 | 186.93 ms | ×4.74 |
| 3200 | 327.76 ms | ×1.75 |

**32× the riders costs ~100× the time — roughly O(N^1.33).** Not linear.

This is not a bug; it is the claim describing the solver incorrectly. Successive Shortest Paths
performs **one augmentation per matched rider**, and each augmentation is a shortest-path search
over a graph whose edge count is itself O(N·k). So the solve is nearer **O(N²k log N)** — quadratic
in N in the worst case. The `log M` term is real and belongs to the *candidate lookup*, not the
solve.

**An honest statement of the complexity:**

```
candidate shortlist :  O(N log M)      <- confirmed by sweep B
solve               :  ~O(N^1.3..2)    <- measured by sweep A
dense alternative   :  ~O(N^2.9)       <- measured by sweep C
```

### The dense control

Doubling both N and M multiplies dense time by ×5–10. At N=M=800 a dense solve takes **~2 seconds**
— past the 3-second batch window on its own. That is the measured justification for the sparse
path, rather than the assertion Week 3 made.

## The finding changed an operational decision

Because cost is superlinear in N, **`MAX_BATCH` is a latency control, not just a memory bound**:

- one 3200-rider batch: **~328 ms**
- four 800-rider batches: **~158 ms total** for the same riders

Splitting a large batch is strictly faster in wall clock, at some cost in match quality — each
smaller batch optimises over a smaller pool. That trade-off is what `MAX_BATCH` exposes, and this
table is now the evidence for choosing its value rather than a guess.

## Why I published the contradiction

It would have been easy to report only the M sweep, which supports the claim beautifully, or to
quietly reword "O(N log M)" in the TDD and move on.

The measurement is the deliverable. A benchmark that only ever confirms what you already wrote is
not evidence; it is decoration. The report states which prediction held, which did not, why, and
what changes as a result.

## Checkpoint

> ✅ A committed report shows p99 latency at 10k drivers and a curve matching the claimed complexity.

Met — with the correction that the curve matches the claim in **M** and refutes it in **N**, and
both reports are committed: [Load_Test_Pipeline.md](../docs/Load_Test_Pipeline.md) and
[Load_Test_Sweep.md](../docs/Load_Test_Sweep.md).

## Phase 4 is complete

| Week | Deliverable | Checkpoint |
|---|---|---|
| 14 | Docker Compose | healthy stack in 12–13 s, 3 cold cycles |
| 15 | Load testing | 10k drivers, p99 8.35 ms, complexity measured |

Phase 5 (Week 16, Oct 22) is Kubernetes: probes, resource limits, an HPA, and a chaos test that
kills the C++ pod under load and proves zero dropped requests. The last of those is the real one —
and Weeks 10 and 12 were built so it should already pass.

## Files touched

`infrastructure/cmd/loadtest/{main,report}.go`, `docs/Load_Test_Pipeline.md`,
`docs/Load_Test_Sweep.md`, `docker-compose.yml` (raised engine batch caps for the sweep).
