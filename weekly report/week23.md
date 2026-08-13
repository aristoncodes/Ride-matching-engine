# Week 23 — Telemetry and SLOs

**Date:** Dec 10, 2026 · **Phase:** 6 (Enterprise Hardening) · **Status:** ✅ Complete

## What this week was about

Making the running system observable, and writing down the promises it is held
to — in a form that cannot drift away from the code.

## SLOs live next to the metrics that measure them

```go
// internal/metrics/metrics.go
const SLOMatchLatencyP99 = 5 * time.Second
```

An SLO in a document drifts from the system within a month. Defined beside the
metric, the two cannot disagree.

| SLO | Target | Why that number |
|---|---|---|
| Ping accept p99 | 50 ms | measured 8.35 ms; this path must never be what fails in a spike |
| Request accept p99 | 100 ms | measured 8.35 ms at 10k concurrent drivers |
| **Match latency p99** | **5 s** | **bounded by the 3 s batch window, not by compute** |
| Match rate | 95% | says whether it does its JOB, not whether it is fast |
| Availability | 99.9% | ~43 min error budget/month |

## The SLO that is easy to state dishonestly

The engine solves a 100-rider batch in **0.4 ms**. It would be very tempting to
publish "sub-millisecond matching".

It would also be a lie. A request arriving just after a flush waits nearly a full
window before it is *considered*, so the floor on rider-visible latency is ~3
seconds. `ridematch_solve_seconds` and `ridematch_match_latency_seconds` are
separate metrics precisely so nobody can accidentally quote one as the other.

## Cardinality is the thing that kills monitoring

Every label is bounded: `tenant` (tens), `outcome`/`reason`/`result` (3–4),
`status_class` (5), `route` (an explicit allowlist).

**There is no `driver_id` or `rider_id` label, and there must never be.**
Prometheus creates one time series per label combination — a driver label across
a 10,000-driver fleet is 10,000 series *per metric*, which kills the monitoring
system long before it helps anyone.

`route` is an allowlist rather than `r.URL.Path` for the same reason: one path
containing an id and every request becomes its own series. A 404 collapses to
`route="GET other"`, which is visible in the verified output.

## Buckets straddle the SLO

Prometheus computes a quantile by interpolating **within** a bucket. So the
accuracy of "are we meeting the SLO?" depends entirely on there being a boundary
at the threshold:

```go
Buckets: []float64{..., .05, .1, .25, ...}   // 0.1 IS the SLO
```

Evenly-spaced buckets would make the number that matters most the least accurate
one on the chart.

## Two alerts that would be wrong

Written down in `docs/Observability.md` alongside the ones that are right:

- **Never alert on `outcome="requeued"`.** Requeuing is the *normal* path for an
  unmatched rider. Alerting on it pages someone every quiet Tuesday.
- **Never alert on solve time alone.** A slow solve is requeued and retried; the
  rider-visible symptom is match latency.

An alert that fires on healthy behaviour trains people to ignore alerts, which is
worse than having none.

## /metrics is on the admin port

Same reasoning as pprof in Week 20: a metrics endpoint enumerates route names,
tenant ids and error rates — reconnaissance — and is a free scrape-amplification
target. Prometheus scrapes from inside the cluster.

## Checkpoint

> ✅ Live throughput and latency visible against explicit SLO thresholds.

Verified end to end:

```
ridematch_ride_requests_total{result="accepted",tenant="demo"} 15
ridematch_http_requests_total{route="POST /v1/ride-requests",status_class="2xx"} 15
ridematch_http_requests_total{route="GET other",status_class="4xx"} 1
ridematch_request_accept_seconds_bucket{tenant="demo",le="0.0005"} 13
```

13 of 15 requests inside 500 µs, and the 404 correctly collapsed rather than
creating a new series.

## Files touched

`internal/metrics/metrics.go`, `internal/api/server.go`,
`internal/batcher/batcher.go`, `cmd/*/main.go`, `docs/Observability.md`.
