# Observability

What this system exposes, what it promises, and how to tell when it is breaking.

Metrics are defined in `infrastructure/internal/metrics/metrics.go` — **the SLO
constants live next to the metrics that measure them**, so the two cannot drift
apart the way a document and a system always do.

---

## Where things are exposed

| | Port | Path | Why |
|---|---|---|---|
| ingestd | 6060 | `/metrics`, `/debug/pprof/`, `/debug/runtime` | admin |
| requestd | 6061 | same | admin |
| batcherd | 6062 | same | admin |
| public API | 8080–8082 | **no metrics, no pprof** | |

**Metrics and profiling are on the admin port, never the public one.** A
`/metrics` endpoint enumerates route names, tenant ids and error rates — exactly
the reconnaissance an attacker wants — and `/debug/pprof/profile` will burn 30
seconds of CPU for anyone who asks, which is a free denial of service. Prometheus
scrapes from inside the cluster.

---

## The SLOs

These are the promises. Every threshold is chosen against something measured,
not because it is a round number.

| SLO | Target | Where it comes from |
|---|---|---|
| **Ping accept p99** | < 50 ms | Week 15 measured p99 8.35 ms; the ping path must never be what falls over in a spike |
| **Request accept p99** | < 100 ms | Week 15 measured p99 8.35 ms at 10k concurrent drivers |
| **Match latency p99** | < 5 s | **bounded by the 3 s batch window** (ADR-0005), not by compute |
| **Match rate** | > 95% | says whether the system does its JOB, as distinct from doing it fast |
| **Availability** | 99.9% | ≈ 43 min of error budget per month |

### The one that is easy to state dishonestly

**Match latency is not solve time.** The engine solves a 100-rider batch in
~0.4 ms, and quoting that as the rider's experience would be a lie: a request
arriving just after a flush waits nearly a full window before it is even
*considered*. The floor is therefore ~3 s, and the SLO has to exceed it.

`ridematch_solve_seconds` and `ridematch_match_latency_seconds` are separate
metrics for exactly this reason.

---

## Alerting rules

```promql
# Requests are being permanently discarded. This is the "never lose a ride
# request" tenet failing, so it pages rather than warns.
increase(ridematch_queue_dead_lettered[10m]) > 0

# Matching is falling behind: requests arrive faster than batchers drain them.
ridematch_queue_depth > 5000

# Consumers are claiming work and not finishing it — a crash loop, or a
# reclaimer that is not running.
ridematch_queue_pending > 1000

# SLO: match latency p99
histogram_quantile(0.99,
  sum(rate(ridematch_match_latency_seconds_bucket[5m])) by (le, tenant)) > 5

# SLO: match rate. The system may be fast and still matching nobody.
sum(rate(ridematch_riders_total{outcome="matched"}[10m])) by (tenant)
  / sum(rate(ridematch_riders_total[10m])) by (tenant) < 0.95

# SLO: availability
sum(rate(ridematch_http_requests_total{status_class="5xx"}[5m]))
  / sum(rate(ridematch_http_requests_total[5m])) > 0.001

# The engine is unreachable or timing out. Retryable errors are survivable —
# requests requeue — but a sustained rate means matching has stopped.
rate(ridematch_dependency_errors_total{dependency="engine",retryable="true"}[5m]) > 1

# The driver pool has collapsed: ingestion has stopped, or the TTL is reaping
# everything. Either way there is nobody to match riders to.
ridematch_drivers_indexed < 10
```

### Two alerts that would be wrong

**Do not alert on `ridematch_riders_total{outcome="requeued"}`.** Requeuing is
*normal* — it is how an unmatched rider waits for the next window. Alerting on it
pages someone every quiet Tuesday.

**Do not alert on solve time alone.** A slow solve is requeued and retried; the
rider-visible symptom is match latency, which is what the SLO measures.

---

## Cardinality

Every label in this system is **bounded**:

| Label | Values | Bounded by |
|---|---|---|
| `tenant` | tens | number of B2B customers |
| `result`, `outcome`, `reason` | 3–4 | enumerations in code |
| `status_class` | 5 | 1xx–5xx |
| `route` | ~5 | explicit allowlist, everything else → `other` |

**There is no `driver_id` or `rider_id` label anywhere, and there must never be.**
Prometheus creates one time series per label combination, so a `driver_id` label
across a 10,000-driver fleet is 10,000 series *per metric* — a cardinality
explosion that kills the monitoring system long before it helps anyone.

`route` is an allowlist rather than `r.URL.Path` for the same reason: one path
containing an id turns every request into its own series.

---

## Histogram buckets straddle the SLO

Prometheus computes a quantile by interpolating *within* a bucket, so the
accuracy of "are we meeting the SLO?" depends entirely on there being a bucket
boundary at the threshold.

`ridematch_request_accept_seconds` therefore has a boundary at exactly `0.1`
(the SLO), and `ridematch_match_latency_seconds` has boundaries at `3` (the batch
window) and `5` (the SLO). Evenly-spaced buckets would make the number that
matters the least accurate one on the chart.

---

## Structured logging

`log/slog`, JSON in production, with a `request_id` on every line. The id is
accepted from an inbound `X-Request-ID` (sanitised — see Week 11) so a trace
spans services: one id links the rider's HTTP call, the queue entry, and the
batch that matched them.

**What is never logged:** API keys, key secrets, or anything that would let a log
reader authenticate. `internal/auth` logs the *reason* a key was rejected but
never the key.

---

## Reproducing the numbers

```bash
docker compose up -d
./scripts/loadprofile.sh steady     # or spike / soak
./scripts/profile.sh baseline 30    # pprof, while load is running
```

Committed measurements: [Benchmarks.md](Benchmarks.md),
[Load_Test_Pipeline.md](Load_Test_Pipeline.md), [Load_Test_Sweep.md](Load_Test_Sweep.md).
