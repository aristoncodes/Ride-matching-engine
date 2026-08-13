# Learnings — Week 16 (Kubernetes, Probes, Chaos Testing)

Report: [week16.md](week16.md)

## 1. Deployment vs StatefulSet — it's about identity, not "state"

A Deployment's pods are interchangeable: random names, and storage that may not follow them. A
StatefulSet gives **stable identity** (`redis-0`) bound to the **same PVC** across restarts.

Redis holds the durable queue here, so a rescheduled pod with an empty volume loses every un-acked
request. **Ask "does this pod's identity matter?"** — not "is it stateful?", which people answer by
vibe.

## 2. Three probes, three different questions

| Probe | Question | Failure does |
|---|---|---|
| **startup** | has it finished booting? | keeps the other two from firing yet |
| **liveness** | is it wedged? | **kills and restarts** the container |
| **readiness** | should traffic come here? | **removes it from the load balancer** |

**Startup probes exist to decouple slow boots from hang detection.** Without one you set a large
`initialDelaySeconds` on liveness to cover the worst-case boot — and that same delay then postpones
detection of a real hang.

**Never check a dependency in a liveness probe.** Redis goes down → every pod fails liveness → the
whole fleet restarts → capacity disappears while nothing is fixed. Check it in *readiness*, which
pulls the instance from the LB without killing it.

**Interview soundbite:** "Liveness is 'restart me'. Readiness is 'don't route to me'. Conflating them
turns a dependency blip into a fleet-wide rolling restart."

## 3. Requests vs limits

- **requests** = what the scheduler reserves, and **what HPA utilisation is measured against**.
- **limits** = the hard ceiling.

A pod using 70m of a 100m request is at **70%**, even if its limit is 1000m. That is the most
commonly misread number in an HPA.

CPU limits **throttle**; memory limits **kill**. So an over-tight CPU limit makes you slow (which the
batcher handles by requeueing) while an over-tight memory limit makes you crash. Set memory limits
with headroom — Redis needs room above `maxmemory` for copy-on-write during an AOF rewrite.

## 4. Autoscaling on the wrong metric is worse than none

`batcherd` blocks on `XREADGROUP`. It looks **idle precisely when the queue is backing up**, so
CPU-based autoscaling would scale it **down** under load.

The right signal is queue depth, which needs a custom/external metric adapter (KEDA,
prometheus-adapter). I left it un-autoscaled and wrote down why, rather than adding a metric that
would actively make things worse.

**Interview soundbite:** "Before autoscaling on CPU, ask whether the work is CPU-bound. For an I/O-
blocked consumer, CPU is inversely correlated with the thing you care about."

## 5. Chaos testing: measure from the authority

The accounting came from **Redis**, not from a service's `/stats` — that endpoint is served by
whichever pod the load balancer picked, and each batcher only knows its own totals.

**Under a load balancer, per-instance metrics do not aggregate by asking one instance.**

Other things that made the test real:
- **Kill ALL replicas**, or the Service routes around the survivor and nothing is tested.
- **Load must be in flight** — killing a pod on an idle system proves nothing.
- **Reset to a clean baseline**, or a previous run's leftovers look like this run's losses.

## 6. The bug: two mechanisms, one signal

Week 10 counted **deliveries** to find poison messages. Week 12 left an unmatched rider **un-acked**
so a later window would retry. To a broker those are identical, so valid riders accumulated
deliveries until the poison detector discarded them.

The fix is to stop overloading one counter:

```
Deliveries    = infrastructure retries  "the consumer died"
MatchAttempts = product outcome         "there was no car"
```

**Generalise it: when two subsystems encode different meanings in the same signal, one of them will
eventually misread the other.** The tell is a retry counter that increments for reasons the retry
policy was never designed for.

And note *what* found it: not a unit test, not a review — a real cluster under sustained load, long
enough for a counter to climb.

## 7. kind specifics worth knowing

- `kind load docker-image` puts a local image into the cluster's store. Without it **and**
  `imagePullPolicy: Never`, kubelet goes to Docker Hub for an image that was never pushed.
- **`kubectl apply -f <dir>` processes files ALPHABETICALLY**, so `namespace.yaml` is applied after
  `go-services.yaml`. Apply the namespace explicitly first. (`redis.yaml` worked by pure luck of the
  alphabet, which made the bug look intermittent.)
- metrics-server needs `--kubelet-insecure-tls` on kind, or every HPA reports `<unknown>` forever.
- Pin the node image **to a digest that exists** — an invented one fails with a confusing registry
  "not found". Read it out of the kind binary.

---

## Self-test
1. When do you need a StatefulSet rather than a Deployment?
2. What does a startup probe let you avoid setting, and why does that matter?
3. Your liveness probe checks the database. The database goes down. Describe what happens to your fleet.
4. HPA target is 70% and the pod uses 700m with a 1000m limit. Is it scaling up?
5. Why is CPU the wrong autoscaling signal for a queue consumer?
6. Why read chaos-test accounting from the datastore rather than a service endpoint?
7. Two subsystems increment the same counter for different reasons. What kind of bug should you expect?
