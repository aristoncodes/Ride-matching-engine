# Learnings — Week 13 (Distributed Locking, Leases, Contention)

The distributed-systems week. Concepts and interview-ready takeaways.
Report: [week13.md](week13.md)

## 1. A per-batch invariant says nothing about concurrent batches

The C++ solver guarantees no driver is used twice **within one batch**. Two batchers solving two
*different* batches can both include the same driver. Each solve is internally correct; the
combination dispatches one car to two riders.

**Interview soundbite:** "Correctness within a unit of work does not compose across concurrent units.
Scope every invariant explicitly — mine was per-batch, and the system is multi-batch."

## 2. Leases, not locks — always a TTL

A lock without a TTL is a deadlock waiting for a crash. The holder dies and the resource is locked
forever, with nothing able to distinguish "in use" from "abandoned".

With a TTL, a crash costs a few seconds of unavailability.

**And `Extend` is what makes a short TTL viable.** The alternative — a TTL long enough for the worst
case — strands the resource for exactly that long after every crash. Keep it short; let a live holder
renew.

## 3. Acquire must be one atomic operation

```
SET key token NX PX ttl     ✅ atomic
GET; if empty then SET      ❌ race between the two calls
```

That race is exactly wide enough for two clients to both see "free" and both write.

## 4. Fencing tokens — the bug almost everyone misses

```
A acquires (token=abc)
A stalls; lease expires
B acquires (token=xyz)     legitimately
A wakes, calls Release
   -> DEL removes B's lease
   -> the resource is now double-held
```

**The lock mechanism itself causes the double-booking.**

Fix: Release and Extend check the token first, **atomically** (Lua in Redis). As two round trips the
same race reopens between the check and the delete.

Martin Kleppmann's fencing-token argument goes further: a truly correct system has the *storage
layer* reject writes carrying a stale token, because a stalled holder can also be stalled between
checking and writing. Worth naming — it is the standard critique of Redlock.

## 5. Partition by the dimension contention actually has

The naive fix for a global lock is "shard it". The question is *by what*.

| Key | Result |
|---|---|
| Driver id | scatters one neighbourhood across every shard → a busy area touches all of them → re-serialised |
| **Geohash** | a shared prefix means physical proximity → a busy district contends only with itself |

**Contention here is spatial, so the partition key must be spatial.** Uniform hashing is exactly
wrong: it destroys the locality that makes partitioning work.

**Interview soundbite:** "Sharding only helps if the shard key matches the contention pattern.
Hashing by id gives you perfect uniformity and no benefit when the contention is geographic."

## 6. Geohash: what it is and why the prefix property holds

Interleaved latitude/longitude bits — a Z-order (Morton) curve — base32 encoded. Each character adds
5 bits, halving the cell alternately in each axis, so **a shared prefix means a shared region**.

Cell sizes worth remembering: precision 5 ≈ 4.9 km, 6 ≈ 1.2 km, 7 ≈ 153 m.

Choose precision to match the work: mine is 5, because a candidate search radius is ~5 km, so a batch
touches one or two cells.

**The known caveat:** two points either side of a cell boundary can be metres apart with different
prefixes. Harmless here (they land in different partitions and simply do not contend), but fatal if
you use geohash prefixes for *proximity search* — which is why Redis GEO uses the score, not the
prefix.

## 7. Prove scalability with a number

"Partitioning reduces contention" is an intuition. This is evidence:

| | Concurrent acquisitions | Wall clock |
|---|---|---|
| One global lock | 3 / 40 | 11.0 ms |
| Geohash partitions | 40 / 40 | 3.1 ms |

**Always benchmark the contended path specifically.** Uncontended microbenchmarks tell you nothing
about behaviour under load, and contention is where distributed systems actually fail.

## 8. Partial success beats all-or-nothing here

`AcquireMany` takes what it can. A rider matched to an *available* driver should not be denied
because a different driver in the same batch was taken.

All-or-nothing would also need a distributed transaction to be correct, and would **live-lock** under
contention as competing batchers repeatedly grabbed overlapping subsets and rolled back.

## 9. Test concurrency with a barrier

```go
start := make(chan struct{})
// N goroutines each block on <-start
close(start)   // release them simultaneously
```

Without it, goroutines trickle in and the race is never exercised — the test passes while the bug
remains.

## 10. The test bug: unrealistic inputs, not broken code

The contention test failed with a spread of 0.28, because riders were 2.2 km apart while cells are
4.9 km — so most shared a partition.

**That was correct behaviour.** Two riders 2 km apart genuinely are in one neighbourhood and should
contend. The test was unrealistic.

It would have been easy and wrong to "fix" it by lowering the threshold or shrinking the cells. **When
a test fails, decide whether the code or the test encodes the wrong belief — and shrinking a
threshold until it passes is almost always the wrong answer.**

---

## Self-test

1. Your solver guarantees no double-booking. Why do you still need distributed locks?
2. Why must every distributed lock have a TTL, and what does `Extend` buy you?
3. Write the atomic acquire. Why is GET-then-SET wrong?
4. Walk through the bug fencing tokens prevent.
5. Why does Release need Lua rather than two Redis calls?
6. Why partition by geohash instead of hashing the driver id?
7. What is the geohash boundary caveat, and when does it actually bite?
8. How would you demonstrate that partitioning reduces contention?
9. Why is a barrier necessary in a concurrency test?
