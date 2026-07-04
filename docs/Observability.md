# Observability Plan

**Status:** Draft · **Related:** [SLO_SLA.md](SLO_SLA.md), [Runbook.md](Runbook.md)

The three pillars — **metrics, logs, traces** — plus the dashboards and alerts that must exist *before* launch. You can't tune (Week 22) or load-test (Week 21) what you can't see.

---

## 1. Metrics (Prometheus)

Every SLI in [SLO_SLA.md](SLO_SLA.md) must have a corresponding metric.

| Metric | Type | Labels | Backs SLO |
|--------|------|--------|-----------|
| `match_compute_micros` | histogram | tenant | #1 compute time |
| `match_latency_seconds` | histogram | tenant | #2 end-to-end |
| `ride_requests_total` | counter | tenant, status | #3 durability |
| `ride_requests_dropped_total` | counter | tenant | #3 (must stay 0) |
| `batch_size` | histogram | tenant | batcher tuning |
| `queue_depth` | gauge | — | backpressure |
| `grpc_calls_total` | counter | method, code | bridge health |
| `driver_location_age_seconds` | histogram | tenant | #7 GPS freshness |
| `active_websocket_connections` | gauge | — | capacity |
| `cross_tenant_denials_total` | counter | — | #6 isolation (should be >0 only in tests) |

## 2. Logs (structured, via Go `slog`)
- **Structured JSON**, one event per line.
- **Mandatory fields on every log:** `timestamp`, `level`, `request_id`, `tenant_id`, `component`.
- Log **decisions and transitions**, not noise: request accepted, batch formed, match produced, request redelivered, key rejected.
- **Never log** raw API keys, or full location histories at info level.

## 3. Traces (OpenTelemetry)
A single trace should follow a ride request across the boundary:
```
span: POST /ride-requests
  └─ span: enqueue
       └─ span: batch-window
            └─ span: GEORADIUS candidates
            └─ span: gRPC SolveBatch  (into C++)
                 └─ span: build-cost-matrix
                 └─ span: solve-assignment
            └─ span: publish-match
```
This is what makes "where did the 3.2 seconds go?" answerable in one click.

## 4. Dashboards (before Week 21 load tests)
1. **Golden signals:** latency (p50/p95/p99), traffic (req/s), errors (%), saturation (queue depth, CPU).
2. **Matching quality:** batch size, match rate, unmatched count.
3. **Bridge health:** gRPC success rate, C++ compute time, worker restarts.
4. **Per-tenant:** request volume and latency sliced by tenant.

## 5. Alerts (with runbook links)
| Alert | Condition | Severity | Runbook |
|-------|-----------|----------|---------|
| RequestsDropped | `ride_requests_dropped_total` > 0 | Sev-1 | [Runbook.md](Runbook.md#dropped-requests) |
| ComputeSLOBreach | compute p99 > 1ms for 10m | Sev-3 | [Runbook.md](Runbook.md#high-latency) |
| QueueBacklog | `queue_depth` rising 5m | Sev-2 | [Runbook.md](Runbook.md#queue-backlog) |
| WorkerCrashLoop | C++ restarts > 3 in 5m | Sev-2 | [Runbook.md](Runbook.md#worker-crash-loop) |
| IngestionDown | uptime probe failing | Sev-1 | [Runbook.md](Runbook.md#ingestion-down) |

Every alert links to a runbook section — an alert with no documented response is a 3am guessing game.

## 6. Milestone tie-in
Metrics + `slog` + dashboards are the **Week 23** deliverable, but the metric *names* are fixed here now so earlier components emit them from the start rather than being retrofitted.
