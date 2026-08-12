# Learnings — Week 15 (Load Testing, Percentiles, Measuring Complexity)

The evidence week. Concepts and interview-ready takeaways.
Report: [week15.md](week15.md)

## 1. Percentiles, never averages

Measured: **mean 857 µs, p99 8.35 ms** — a 10× gap. Reporting the mean would have described a
system nobody experiences.

An average is dominated by the common case; **tail latency is the whole point of surviving a
spike**. p99 means 1 request in 100, which at 400 req/s is four *every second*.

**Compute percentiles from the full sample set** (exact, nearest-rank) or from a proper structure
(HDR histogram, t-digest). You cannot average two p99s — a mistake that shows up constantly in
dashboards that aggregate per-instance percentiles.

**Interview soundbite:** "I never report a mean latency. The mean is the number that makes a broken
system look fine."

## 2. Measure the claim, not the code

The claim was O(N log M). That is **falsifiable**, which is what makes it worth testing:

1. Doubling N should double the time.
2. Doubling M should barely move it.

Then vary **one dimension at a time**, with a **control** (the dense path) that is known to be
O(N·M) so you have something to compare shape against.

If you cannot state what measurement would prove you wrong, you are not testing — you are
confirming.

## 3. The result: half the claim was wrong

| Sweep | Prediction | Measured | Verdict |
|---|---|---|---|
| M: 500 → 16000 (32×) | barely moves | **~2× total** | ✅ confirmed |
| N: 100 → 3200 (32×) | ~32× | **~100× (N^1.33)** | ❌ refuted |
| dense control | O(N·M) | ~N^2.9 | ✅ as expected |

**Why N is superlinear:** Successive Shortest Paths runs **one augmentation per matched rider**, and
each augmentation is a shortest-path search over a graph with O(N·k) edges. So the solve is nearer
O(N²k log N). The `log M` term is real — it just belongs to the *quadtree candidate lookup*, not to
the solve.

**The lesson: know which component each term of your complexity comes from.** "O(N log M)" conflated
a lookup cost with a solve cost, and only measuring separated them.

## 4. Publish the contradiction

It would have been easy to report only the M sweep, which supports the claim beautifully.

**A benchmark that only ever confirms what you already believed is decoration, not evidence.** The
report states which prediction held, which did not, why, and what changes as a result.

**Interview soundbite:** "I benchmarked our complexity claim and found it was right about one
dimension and wrong about the other. The write-up says so, because the number that contradicts you
is the only one that was worth measuring."

## 5. The measurement changed a decision

Because cost is superlinear in N:

- one 3200-rider batch: ~328 ms
- four 800-rider batches: ~158 ms total

**So `MAX_BATCH` is a latency control, not just a memory bound** — and splitting large batches is
strictly faster in wall clock, at some cost in match quality since each optimises over a smaller
pool.

**A benchmark that changes no decision was not worth running.**

## 6. Load generator hygiene (these are all real failure modes)

- **Bound the dial rate.** Opening 10k sockets at once measures the kernel's accept queue and
  exhausts ephemeral ports. A semaphore on the dial fixes it.
- **Raise `MaxIdleConnsPerHost`.** Go's default of 2 forces a TCP handshake per request, and you
  measure connection setup instead of the service.
- **Stagger periodic clients.** Otherwise every driver fires on the same millisecond and the load is
  a spike per interval, not a stream.
- **Per-goroutine RNG.** The global `math/rand` source has a mutex; sharing it makes you measure
  that.
- **Warm up before timing.** The first call has a different allocation profile than steady state.
- **Count refusals separately from failures.** A 503 from a connection limit is the server working
  correctly — a successful test of backpressure, not a failure of the harness.
- **`ulimit -n`.** 10k sockets needs a raised file-descriptor limit.

## 7. Measure at the right layer

The sweep uses the engine's own `compute_micros` rather than wall clock, excluding serialization and
the loopback round trip. That isolates the *algorithm*.

The pipeline test uses client-side wall clock, because that is what a rider's app experiences.

**Both are correct for their question, and neither answers the other's.** Be explicit in the report
about which one a number is.

## 8. Report what a number does NOT cover

`POST /v1/ride-requests` at p99 8.35 ms measures the front door only — validate, publish, 202. It
does **not** include matching, which happens in a later window.

Leaving that unstated would let "p99 8 ms" be read as time-to-match, which is bounded by the batch
window and is hundreds of times larger. **The caveat is part of the result.**

## 9. Commit the report

A benchmark printed to a terminal cannot be compared next month. Committed reports make a regression
show up in a diff — and record the machine, since an absolute number from an unknown machine is
uncheckable. **What should hold across machines is the shape: the ratios between rows.**

---

## Self-test

1. Why is a mean latency misleading? Why can't you average two p99s?
2. What makes a complexity claim testable, and what is the role of a control?
3. Your solve is O(N log M) in theory and N^1.33 in practice. Where did the extra come from?
4. Why does one big batch cost more than several small ones for the same total riders?
5. Name four ways a naive load generator ends up measuring itself.
6. When would you measure server-side compute time rather than client-side wall clock?
7. Your p99 is 8 ms end to end. What does that number NOT include here?
8. You benchmark your own design claim and it fails. What do you do?
