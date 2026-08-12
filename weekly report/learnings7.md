# Learnings — Week 7 (Redis Geospatial, TTLs, Client Resilience)

The live-state week. Concepts and interview-ready takeaways.
Report: [week7.md](week7.md)

## 1. Redis geo sets ARE sorted sets

`GEOADD` stores a **geohash as the score** of a sorted set member. Everything follows from that:

- There is no `GEOREM` — you delete with **`ZREM`**.
- `ZCARD` counts your indexed drivers.
- `GEOSEARCH` (Redis 6.2+; `GEORADIUS` is deprecated) is a range query over that encoding.

**Interview soundbite:** "Redis geo is a sorted set with a geohash score. Knowing that tells you
what's cheap, what's missing, and why you delete with ZREM."

## 2. The trap: Redis has no per-member TTL

Expiry is **per key**, not per member. So `EXPIRE` on the geo key deletes **every driver at once**
— useless.

**The pattern:** a companion sorted set with `score = last-seen timestamp`.

```
drivers:geo:<tenant>    score = geohash
drivers:seen:<tenant>   score = unix ms
```

Freshness becomes an O(log N) score range.

**And you need BOTH a read filter and a reaper:**

| | Keeps stale drivers out of… |
|---|---|
| Filter on read | **answers** |
| Reap on a ticker | **memory** |

Filter alone → the index grows forever. Reap alone → stale drivers served between sweeps. This is
the kind of thing worth being able to explain unprompted.

## 3. Fail closed on freshness

A driver in the geo index with **no** freshness score is treated as stale — exactly as untrustworthy
as one whose ping is ancient. When the safe default and the simple default coincide, take it.

The consequence in the domain is concrete: serving a stale driver means dispatching a car that
isn't there, and the rider waits for someone who left ten minutes ago.

## 4. Round trips are the currency

- **`ZMSCORE` once**, not `ZSCORE` per candidate. N round trips inside a 3-second window is how a
  fast store becomes a slow one.
- **Pipeline** the two writes (`GEOADD` + `ZADD`) into one round trip.
- **Over-fetch before filtering**: ask for 3× the limit, because some results will be dropped as
  stale. Asking for exactly `limit` silently returns short lists precisely when the fleet is
  churning.

**Pipelining is not a transaction.** My two writes are not atomic with respect to each other, and
that's a deliberate, benign choice: the only interleaving is a driver briefly in the geo index
without a freshness score, which reads treat as stale. **Design the interleaving to fail safe and
you don't need the transaction.**

## 5. Redis is single-threaded — bound your commands

One slow command blocks **everything**. So the reaper deletes at most 10,000 per sweep. An
unbounded reap after an outage would pull millions of ids into one command and stall the server —
the reaper causing the outage it exists to clean up after.

Same reason `KEYS` is banned in production and `SCAN` exists.

## 6. Retry with backoff AND jitter (know why jitter)

Without jitter, every client that failed at the same instant retries at the same instant. A Redis
restart makes *all* of them fail simultaneously, so the recovering server takes a **synchronised
thundering herd** exactly when it is least able to cope. Jitter spreads them out.

**Retry only what is transient:**

| Error | Retry? |
|---|---|
| Connection refused, timeout, pool exhausted | ✅ transient |
| `WRONGTYPE`, `NOSCRIPT`, OOM | ❌ a bug — retrying fails 3× slower |
| Context cancelled | ❌ the caller already gave up |

## 7. Always set client timeouts

Dial, read, and write timeouts. Without them, a network partition leaves a goroutine blocked on a
socket read until the OS gives up — which can be minutes. **The point of a *live* store is that it
answers now or not at all.**

## 8. Injectable clocks

```go
type Options struct { Now func() time.Time }
```

A TTL test that sleeps 30 real seconds is a test nobody runs. One that shortens the TTL to 50 ms
tests a *different system*. An injected clock lets you jump forward and test the real configuration
instantly.

**Interview soundbite:** "Time is a dependency. Inject it and time-dependent logic becomes
ordinary, fast, deterministic unit tests."

## 9. Interface first, implementation second

Defining `Repository` before the Redis type meant callers could be tested against an in-memory fake
with no Redis at all — and the store stays swappable.

`var _ Repository = (*RedisRepository)(nil)` is a free compile-time check that the implementation
still satisfies the interface. Cheaper than finding a signature drift at a call site.

## 10. Test against the real dependency where the risk lives

The Redis tests run a **real redis-server**, one private instance per test on a unix socket with
persistence off. A fake Redis would not have taught me that geo sets have no per-member TTL — the
whole design of this package came from a real constraint that a mock would have hidden.

Rule of thumb: **mock what you own, run what you don't.**

## 11. The bug: unix socket paths have a hard length limit

A unix socket address is a fixed-size `sun_path` array in the kernel: **104 bytes on macOS**, 108 on
Linux. Go's `t.TempDir()` embeds the test's *name*, so:

```
/var/folders/.../T/TestStaleDriversAreNotReturned2114060021/001/redis.sock  = exactly 104
```

Only long-named tests failed. Whether the test passed was a function of **how long its name was** —
which is about as misleading a symptom as debugging offers.

---

## Self-test

1. What data structure backs a Redis geo set, and what does that imply about deleting a member?
2. Why can't you use `EXPIRE` to age out individual drivers? What do you do instead?
3. Why do you need both a read-time freshness filter and a background reaper?
4. Why bound the number of keys a reaper deletes per sweep?
5. Why does retry backoff need jitter?
6. Which Redis errors should you never retry, and why?
7. How do you test a 30-second TTL without a 30-second test?
8. Your pipelined `GEOADD` + `ZADD` isn't atomic. Why is that acceptable here?
9. Your integration tests pass locally and fail in CI with "no such file" on a socket. What would
   you check first?
