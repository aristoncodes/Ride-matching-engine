# Learnings — Weeks 23–24 (Telemetry, SLOs, Documentation)

Reports: [week23.md](week23.md) · [week24.md](week24.md)

## 1. Cardinality is what kills a monitoring system

Prometheus creates **one time series per label combination**. A `driver_id` label
on a 10,000-driver fleet is 10,000 series *per metric*.

| Safe | Dangerous |
|---|---|
| `tenant` (tens) | `driver_id`, `rider_id`, `request_id` |
| `status_class` (5) | raw `status_code` |
| `route` from an allowlist | `r.URL.Path` |

`r.URL.Path` is the classic mistake: one path containing an id and **every
request becomes its own series**. Use an allowlist and collapse the rest to
`other`.

**Interview soundbite:** "Before adding a label I ask what bounds its values. If
the answer is 'the number of users', it is not a label — it is a log field."

## 2. Status CLASS, not status code

Nobody alerts on 418. Availability SLOs are computed from the class, and five
values instead of forty is free accuracy.

## 3. Histogram buckets must straddle the SLO

Prometheus computes quantiles by interpolating **within** a bucket. If your SLO
is 100 ms and your buckets jump 50 ms → 250 ms, the number you care about is the
least accurate one on the chart.

Put a boundary **at** the threshold. Mine has boundaries at 0.1 s (the request
SLO), and at 3 s and 5 s for match latency (the batch window and its SLO).

## 4. Define SLOs next to the metrics that measure them

```go
const SLOMatchLatencyP99 = 5 * time.Second
```

An SLO in a wiki drifts from the system within a month. In the same file as the
histogram, the two cannot disagree.

## 5. Measure what the user experiences, not what is impressive

The solver runs in **0.4 ms**. "Sub-millisecond matching" would be a great
headline and a lie: a request arriving just after a flush waits nearly a full
3-second window before it is even *considered*.

So there are two metrics — `solve_seconds` and `match_latency_seconds` — and the
SLO is on the second. **When a fast number and an honest number differ, publish
the honest one and keep the fast one for tuning.**

## 6. Alerts that fire on healthy behaviour are worse than no alerts

Two I deliberately did *not* write:

- `outcome="requeued"` — that is the **normal** path for an unmatched rider.
  Alerting on it pages someone every quiet Tuesday.
- solve time alone — a slow solve is retried; the rider-visible symptom is match
  latency.

**Every false page trains someone to ignore the next real one.**

## 7. Alert on the tenet, not the symptom

The strongest alert in the system is:

```promql
increase(ridematch_queue_dead_lettered[10m]) > 0
```

Dead-lettering means a ride request was permanently discarded — the "never lose a
request" tenet failing. That is what pages. Latency warns.

## 8. /metrics belongs on the admin port

Same argument as pprof: it enumerates route names, tenant ids and error rates
(reconnaissance), and is a free scrape-amplification target.

## 9. Documentation: lead with ratios, state the machine

An absolute number from an unknown machine cannot be checked. What travels
between machines is the **shape** — 32× drivers → 2× time — not the milliseconds.

## 10. Document what is wrong, not just what works

The README has a section titled *"Where the claims turned out to be wrong."* Three
entries: the O(N log M) claim, ADR-0004's Redis TTL premise, and my own
misreading of the Week 22 profile.

**A repository that only documents successes reads as marketing.** The
corrections are what show the work happened — and in an interview they are far
more interesting than the successes, because anyone can claim a benchmark.

## 11. Write the quickstart for someone who has never seen it

Mine walks through the case where **nothing matches** (no drivers yet), because
that is what actually happens on a first run. Skipping it leaves a newcomer
thinking the system is broken thirty seconds in.

**Run your own quickstart on a clean machine.** Every assumption you forgot to
write down surfaces immediately.

---

## Self-test
1. Why is `driver_id` an unacceptable Prometheus label?
2. What is wrong with using `r.URL.Path` as a label?
3. Why must a histogram bucket boundary sit at your SLO threshold?
4. Your solver takes 0.4 ms and users wait 3 s. Which do you publish as the SLO?
5. Give an example of an alert that would fire on perfectly healthy behaviour.
6. Why put SLO constants in the code rather than a document?
7. Why does a README benefit from a section about what you got wrong?
