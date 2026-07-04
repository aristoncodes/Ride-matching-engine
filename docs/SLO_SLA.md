# Service Level Objectives (SLOs) & SLAs

**Status:** Draft · **Related:** [Observability.md](Observability.md), [Product_Requirements.md](Product_Requirements.md)

This document turns the project's adjectives ("sub-millisecond", "zero dropped requests") into **measurable targets with error budgets**. If it isn't measured here, it isn't a real requirement.

---

## 1. Definitions

- **SLI (Indicator):** a measured number (e.g., p99 match compute time).
- **SLO (Objective):** the internal target for an SLI (e.g., p99 ≤ 1ms).
- **SLA (Agreement):** the externally-promised subset, with consequences for the client. SLAs are looser than SLOs on purpose (the gap is safety margin).
- **Error budget:** `1 − SLO`. Spending it is allowed; exhausting it freezes risky changes.

## 2. SLIs and SLOs

| # | SLI | SLO (internal) | Measured where |
|---|-----|----------------|----------------|
| 1 | **Core match compute time** (C++ solve, per batch) | p99 ≤ **1 ms** at 10k drivers | `compute_micros` in the gRPC response |
| 2 | **End-to-end match latency** (request enqueue → match available) | p99 ≤ **3.5 s** (≈ window + processing) | API + batcher spans |
| 3 | **Request durability** (no dropped ride requests) | **100%** processed or explicitly rejected; **0** silent drops | queue acks vs. ingested count |
| 4 | **Match optimality** | **0%** cost gap vs. brute-force optimum on the verification suite | test suite |
| 5 | **Ingestion availability** (API + WebSocket up) | **99.9%** monthly | uptime probe |
| 6 | **Tenant isolation** | **0** cross-tenant reads | isolation test suite |
| 7 | **GPS freshness** (matched driver's last ping) | p95 location age ≤ **5 s** | Redis TTL / update timestamps |

## 3. Error budgets

| SLO | Budget | Meaning |
|-----|--------|---------|
| Ingestion 99.9% / month | ~43 min downtime/month | if exceeded, halt feature work; fix reliability |
| Compute p99 ≤ 1ms | 1% of batches may exceed | persistent breach → performance investigation (Week 22) |
| Durability 100% | **0 budget** — a hard invariant | any silent drop is a Sev-1 / COE |

## 4. External SLA (illustrative, for B2B contracts)

| Promise | SLA target | Remedy if breached |
|---------|-----------|--------------------|
| API availability | 99.5% monthly | service credits |
| No lost ride requests | 100% | incident review + COE shared with client |
| Match latency | p99 ≤ 5 s | best-effort, monitored |

*SLA is deliberately looser than the internal SLO — the margin absorbs normal variance without breaching contracts.*

## 5. How these tie to the build

- SLO #1 & #4 are validated by the **Week 5** testing/benchmark deliverable and re-checked in **Week 22**.
- SLO #3 is validated by the **Week 10** durable-queue and **Week 16** chaos (kill-the-pod) tests.
- SLO #6 is validated by the **Week 19** multi-tenancy isolation tests.
- All SLIs must be emitted as metrics per [Observability.md](Observability.md) **before** the Week 21 load tests, or the load tests can't measure anything.

## 6. Review cadence
SLOs are reviewed after each major load test (Weeks 15, 21) and adjusted with evidence — never loosened silently to make a dashboard green.
