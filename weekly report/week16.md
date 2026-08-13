# Week 16 — Kubernetes, and the bug the chaos test found

**Date:** Oct 22, 2026 · **Phase:** 5 (Production Orchestration) · **Status:** ✅ Complete

## What this week was about

The project's central resiliency claim, made falsifiable: **kill the C++ engine under load and lose
no ride requests.** Everything from Week 6 onward was built so this would pass.

## The checkpoint

```
submitted & accepted (HTTP 202)  : 200
DISTINCT requests in the stream  : 200
still pending / undelivered      : 0 / 0
dead-lettered                    : 0

PASS  every accepted request reached the durable queue
PASS  nothing dead-lettered — an engine crash is retryable
PASS  queue fully drained
PASS  ZERO DROPPED REQUESTS across a full engine outage
      (125 needed more than one window — retried, not lost)
```

**Both** engine pods deleted mid-traffic, not one — otherwise the Service quietly routes around the
survivor and nothing is actually tested.

The accounting comes from **Redis**, not from a service's `/stats`, because that endpoint is served
by whichever pod the NodePort load-balances to and each batcher only knows its own totals.

## Why it passed

Nothing in the manifests makes this work. It was already true:

| | |
|---|---|
| ADR-0002 | the engine is a separate process, so its death is an error value |
| ADR-0006 | requests live in a durable stream, not memory |
| Week 12 | a request is acked only AFTER it is matched |
| Week 10 | un-acked messages are redelivered via `XPENDING`/`XCLAIM` |
| Week 16 | Kubernetes reschedules the dead pod |

Kubernetes' contribution is only the last line. The durability was designed six weeks earlier.

## Manifest decisions worth defending

**Redis as a StatefulSet, not a Deployment.** It holds the durable queue. A Deployment gives pods
random names and treats storage as interchangeable — a rescheduled Redis with an empty volume loses
every un-acked request, which is the exact failure this phase exists to prevent.

**A startup probe separate from liveness.** The engine parses an OSM extract before it listens.
Without the split you must set a large `initialDelaySeconds` on liveness to cover the worst case,
which then delays detection of a *real* hang by the same amount. A startup probe decouples them:
generous while booting, strict once running.

**`/healthz` never checks Redis.** A liveness probe that fails during a dependency outage gets
every pod killed and restarted — removing capacity while fixing nothing, and often turning a partial
outage into a total one. `requestd`'s *readiness* does check it, which pulls it from the load
balancer instead.

**`batcherd` deliberately has no HPA.** It blocks on `XREADGROUP`, so it looks idle exactly when the
queue is backing up — CPU-based autoscaling would scale it **down** under load. The right signal is
queue depth, which needs KEDA. Deferred rather than faked with a metric that would actively mislead.

**Consumer name from the pod name** (`valueFrom: fieldRef: metadata.name`). Two consumers sharing a
name share a pending list and each treats the other's in-flight work as abandoned.

## The bug

The first run failed, and the failure was real.

`XPENDING` showed **delivery count 4, with `MaxDeliveries` 5.** These riders were one cycle from
being dead-lettered as poison — and they weren't poison. They were unmatched, because `k=8` and
clustered riders compete for the same drivers (`riders=82 drivers=46 matched=20`).

**Two designs collided:**

- Week 10 counts *deliveries* to detect messages that keep failing.
- Week 12 deliberately leaves an unmatched rider *un-acked* so a later window retries.

Both look identical to a broker. So a rider who simply could not find a car accumulated deliveries
until the poison detector threw them away — **silently dropping a real customer, which is precisely
what all the durability work exists to prevent.**

### The fix: separate the two ideas

```
Message.Deliveries    = infrastructure retries  "the consumer died holding this"
Request.MatchAttempts = product outcome         "we looked and there was no car"
```

`Queue.Republish` puts an unmatched rider back for a later window, **resetting** the infrastructure
counter and **advancing** MatchAttempts. Publish-then-ack, so a crash duplicates (recoverable via
`RequestID`) rather than loses.

Past `MaxMatchAttempts` the rider is dead-lettered with a *reason* — "no driver found after N
attempts" — which is a real answer you can give a customer, not an infrastructure error they should
never see.

**No unit test could have found this.** It needed a real cluster, real load, and enough time for a
counter to climb.

## A bug in the test, too

The second run reported *"published (269) != accepted (200) — requests lost at the front door"*.

Nothing was lost. `XLEN` counts stream **entries**, and republishing an unmatched rider appends
another entry with the same `request_id`. Zero-loss requires counting **distinct request ids**.

Worth noting because the fix I'd made was correct and the test then reported it as a failure — it
would have been easy to "fix" the code back.

## Files touched

`k8s/{kind-cluster.yaml,deploy.sh,chaos-test.sh}`, `k8s/base/*.yaml`,
`internal/queue/{queue,redis_stream,redis_stream_test}.go`, `internal/batcher/batcher.go`,
`matching_engine/Dockerfile`.
