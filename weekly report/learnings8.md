# Learnings — Week 8 (WebSockets, Goroutine Lifecycles, Resource Limits)

The concurrency-discipline week. Concepts and interview-ready takeaways.
Report: [week8.md](week8.md)

## 1. TCP will not tell you the client is gone

This is the single most important fact about long-lived connections.

A phone entering a tunnel, a force-quit app, a cut network cable — TCP holds the socket "open"
indefinitely, because it has no idea. Without an application-level heartbeat your server
accumulates connections to clients that no longer exist.

**The mechanism:**

```
PongWait     60s   ← read deadline; silence past this closes the connection
PingInterval 54s   ← 90% of PongWait, so one lost ping is survivable
```

Server pings; client pongs; **the pong handler pushes the read deadline forward**.

**The read deadline is absolute, not a rolling idle timer.** Set it once and every client is
disconnected after 60 seconds no matter how healthy. It must be reset on every sign of life.

**The ratio is a real constraint.** If `PingInterval ≥ PongWait`, you time out clients you never
gave a chance to answer. I assert it in a test so it can't drift.

**Interview soundbite:** "TCP keepalive defaults are two hours and OS-level. Application heartbeats
are how you detect a dead peer in seconds, and the read deadline is what actually enforces it."

## 2. Test both directions of a health check

Almost everyone tests "silent client gets dropped". The test that catches a bad ping/deadline ratio
is the *other* one: **a responsive client must survive well past `PongWait`**. Without it, a
too-slow ping interval passes your suite and disconnects every healthy driver in production.

**General principle: for any rule that rejects things, test that it also accepts the things it
should.**

## 3. Limits, and where to enforce them

**Connection limit — check BEFORE the upgrade.** Returning 503 with `Retry-After` costs nothing;
completing a handshake and immediately tearing it down wastes both sides' work and looks like a
server bug from outside.

**Message size limit.** Without `SetReadLimit`, a client can announce a 2 GB frame and the server
will try to buffer it. Set it against the real payload (a 100-byte GPS ping → 4 KB is generous).

**Write deadline.** A client that stops *reading* applies TCP backpressure. Without a deadline the
write blocks forever, pinning a goroutine and its buffer — a slow client becomes a memory leak.

## 4. One reader, one writer — this is a hard rule

gorilla/websocket allows **exactly one concurrent reader and one concurrent writer**. Two goroutines
writing is a data race, not a queue.

Hence the read pump / write pump pattern. All writes — including heartbeats — go through the single
writer. Even when there's nothing else to send, keeping the write pump as its own goroutine means
that when you *do* need to push, there's one obvious place for it rather than a race waiting to
happen.

## 5. Cancelling a context does NOT unblock a blocked read

**The bug of the week, and a genuinely valuable one.**

```go
conn.ReadMessage()   // blocks. Ignores your context entirely.
```

There is no context-aware read. Cancelling told the write pump to stop and did **nothing** to the
reader, which sat in a socket read until its deadline (up to 60 s). `Shutdown` timed out.

**The fix — the only thing that unblocks a blocked reader:**

```go
go func() {
    <-ctx.Done()
    conn.Close()      // forces the pending read to return an error
}()
```

**Generalise it:** cancellation only works if *something* is selecting on `ctx.Done()`. A goroutine
blocked in a syscall — a socket read, a file read, a CGO call — cannot be cancelled. You must close
the underlying resource out from under it.

## 6. Proving the absence of a goroutine leak

```go
before := runtime.NumGoroutine()
// 100 connect / abrupt-disconnect cycles
after := runtime.NumGoroutine()   // 3 before, 3 after
```

Three details that make it a real test:

- **Warm up first.** The first connection allocates HTTP-server internals that would otherwise look
  like a leak.
- **Many cycles.** A leak of one hides in noise; a leak of 200 does not.
- **Close abruptly**, with no closing handshake — a force-quit, not a polite goodbye.

`goleak` (Uber) does this properly for real projects, and is worth naming in an interview.

## 7. Shutdown must WAIT, not just signal

```go
func (s *Server) Shutdown(ctx context.Context) error {
    s.closeOnce.Do(func() { close(s.shutdown) })  // signal
    // ...then WAIT on the WaitGroup, bounded by ctx
}
```

Returning as soon as the listener closes reports "shut down" while goroutines are still running and
still writing to Redis. That's how a process that "exited cleanly" corrupts its last batch.

**Two idioms doing real work:**
- **`sync.Once` around a channel close** — closing a closed channel panics, and `Shutdown` is
  routinely called twice (signal handler *and* defer).
- **`close(channel)` as a broadcast** — every goroutine selecting on it wakes at once. That's how
  one signal reaches N goroutines.

## 8. Decide what is fatal to a connection

Not every error should disconnect a client:

| Event | Response | Why |
|---|---|---|
| Malformed frame | count, drop, **keep going** | one bad message shouldn't cost a driver their stream |
| Sink error (Redis down) | log, **keep going** | dropping every connection turns a recoverable failure into a reconnect storm |
| Oversized frame | **close** | the client isn't speaking the agreed protocol |
| Read deadline | **close** | the client is gone |

And **count** the non-fatal ones. A permanently broken client should be visible, not silent.

## 9. Atomics vs mutexes

Counters incremented on every message from every connection are the hottest shared state in the
process — the one place a mutex is actually felt. `atomic.Int64` for independent counters;
`sync.Mutex` when you need several fields to change together consistently.

## 10. `-race` is not optional for this code

Every test in this package runs under `-race`. The Go race detector finds real bugs that are
otherwise nondeterministic and load-dependent — exactly the ones that survive review and appear in
production at 6pm.

---

## Self-test

1. Why can't you rely on TCP to tell you a WebSocket client is gone?
2. Why must `PingInterval` be shorter than `PongWait`, and what breaks if it isn't?
3. Where should a connection limit be enforced relative to the upgrade, and why?
4. Why does gorilla/websocket require a single writer goroutine?
5. Your `Shutdown` hangs for 60 seconds per idle connection. What is happening and how do you fix it?
6. How do you prove a server doesn't leak goroutines?
7. Why wrap a channel close in `sync.Once`?
8. Which client errors should close the connection and which should not?
9. Redis goes down. Should your WebSocket server disconnect its clients? Why not?
