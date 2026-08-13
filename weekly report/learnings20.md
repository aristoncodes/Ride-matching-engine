# Learnings — Weeks 20–22 (Profiling, Load Testing, Optimisation)

Reports: [week20.md](week20.md) · [week21.md](week21.md) · [week22.md](week22.md)

## 1. pprof is a security surface

`net/http/pprof` **registers on `http.DefaultServeMux` as an import side effect**.
Import it in a service that serves its API from the default mux and you have
published:

- `/debug/pprof/heap` — memory layout
- `/debug/pprof/goroutine` — every function you are running
- `/debug/pprof/profile` — 30 seconds of CPU, on demand, to anyone (a free DoS)

**Fix structurally:** your own mux, your own listener, a port the deployment can
firewall.

**Interview soundbite:** "Importing net/http/pprof is enough to expose it. If your
service uses the default mux, that import is a production incident waiting for
someone to find the port."

## 2. Profiling details that waste an afternoon

- **`WriteTimeout` must exceed the profile duration.** A CPU profile is a 30s
  streaming response; a 15s timeout truncates it and pprof reports
  `unrecognized profile format`, not a timeout.
- **Block/mutex profiling changes what you measure.** Both add overhead to
  blocking and locking — the hot path. Opt in per run.
- **Profile all services over the SAME window.** Sequential captures compare
  different moments of the load.
- **Never profile an idle system.** You will profile the scheduler.

## 3. `-cum` not `-flat`

Flat time points at leaves — usually `syscall` or runtime internals you cannot
act on. Cumulative points at the **call path responsible**, which is where the
fix goes.

## 4. Load profiles: soak is the one people skip

| Profile | Finds |
|---|---|
| steady | your baseline latency |
| spike | whether backpressure is exercised, not merely present |
| **soak** | **leaks, unbounded growth, TTL/reaper bugs** |

Volume finds throughput problems. **Only TIME finds a goroutine leak of one per
connection**, which is invisible at 30 seconds and fatal after a day.

Snapshot goroutines/heap/GC before and after every run so slow growth is a diff
rather than something someone has to be watching for.

## 5. A profiler tells you WHERE, not WHY

**The most valuable lesson of the phase.**

The profile said *"59% of CPU inside Redis calls."* I read *"too many round
trips"* and pipelined them. Expected ~10×; got **1.2–1.5×**.

The measurement that corrected me:

```
loopback RTT       ~0.10 ms
500 sequential      131 ms   ← only ~50 ms is round trips
500 pipelined       110 ms   ← the other ~80 ms is Redis EXECUTING queries
```

Redis is single-threaded. Pipelining removes network round trips and **cannot**
make the server execute 500 GEOSEARCHes any faster.

**Time attributed to a dependency is not the same as time wasted on it.** Most of
that CPU was real work, correctly attributed, that I misdiagnosed as overhead.

## 6. Write the test that can prove you wrong

My regression test asserted `>1.5×` and **failed at 1.2×**. That failure is where
the understanding came from. Had I written `>1.0×`, it would have passed and I
would still believe the wrong explanation.

**A test that encodes your expectation will tell you when your expectation is
wrong — but only if you write it before you know the answer.**

## 7. Performance ratios are properties of the deployment

The pipelining win is 1.4× on loopback (RTT 0.1 ms) and would be far larger at a
realistic 0.5 ms RTT, where 500 round trips cost 250 ms instead of 50 ms.

So the change is *more* valuable in production than on the machine that measured
it — and the test asserts only "not reverted", with the reasoning written down,
rather than a number that holds on one laptop.

## 8. Guard the ANSWER before you optimise

Before touching `processBatch`, the new path had to prove it returns exactly what
the old one returns — **including the freshness filter**, whose shape changed
(union ZMScore vs per-query) and is therefore exactly where it would have been
lost.

**An optimisation that quietly changes results is a correctness bug wearing a
performance win's clothing.**

## 9. Know which claim you are making

"Sub-millisecond" was true of the *solve* (0.4 ms) and false of the rider's
*experience* (bounded by a 3-second batch window). Both numbers are real. Quoting
the first as the second would have been dishonest, so they are separate metrics.

---

## Self-test
1. What does importing `net/http/pprof` do that you did not ask for?
2. Why must a pprof server's write timeout be unusually long?
3. Why is `-cum` more useful than `-flat` when reading a CPU profile?
4. Which load profile finds a goroutine leak, and why can the others not?
5. Your profile shows 60% of time in a database client. Name two very different
   root causes consistent with that.
6. Why assert a specific speedup in a test, given it depends on the machine?
7. What must you verify before shipping any optimisation to a query path?
