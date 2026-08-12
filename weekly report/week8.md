# Week 8 — WebSockets: Thousands of GPS Streams Without Leaking

**Date:** Aug 27, 2026 · **Phase:** 2 (Go Bridge & State Ingestion) · **Status:** ✅ Complete

## What this week was about

The part of the system that faces thousands of phones on flaky mobile networks. Almost none of the
code is about the happy path; it is about what happens when a client misbehaves or vanishes.

Three things matter:

| | Why |
|---|---|
| **Dead clients** | TCP happily holds a "connection" open long after the phone is in a tunnel or the app was force-quit. |
| **Limits** | One abusive client must not exhaust the process. |
| **No leaks** | A leaked goroutine per connection is invisible for hours, then fatal. |

## Dead-client detection

TCP will not tell you the client is gone. Only an application-level heartbeat will.

```
PongWait     60s   ← the read deadline: silence longer than this closes the connection
PingInterval 54s   ← 90% of PongWait, so one lost ping is survivable
```

The read deadline is **absolute**, so it must be pushed forward on every sign of life — on every
pong *and* on any traffic. Setting it once would disconnect every client after 60 seconds no matter
how healthy they are.

If `PingInterval` were ever ≥ `PongWait`, the server would time out clients it never gave a chance
to answer. `TestConfigDefaultsAreSane` asserts the ratio so that can't drift.

Both directions are tested, and the second one is the one that catches a bad ratio:

- a client that **stops** answering is dropped (with the socket still open at the TCP level);
- a client that **does** answer survives 3× `PongWait`.

## Limits

**`MaxConnections`, checked *before* the upgrade.** Returning a 503 with `Retry-After` is cheaper
and kinder than completing a handshake and immediately tearing it down. The slot is reserved with
an atomic increment, then released if the upgrade fails.

**`MaxMessageBytes` = 4 KB** against a ~100-byte GPS ping. Without a read limit a client can
announce a 2 GB message and the server will dutifully try to buffer it.

**Write deadlines.** A client that stops *reading* applies TCP backpressure; without a deadline the
write blocks forever, pinning a goroutine and its buffer.

## Two goroutines per connection, and exactly two

gorilla/websocket permits **one concurrent reader and one concurrent writer** — more is a data
race, not a queue. So:

- `readPump` — the only reader. Parses pings, forwards to the sink.
- `writePump` — the only writer. Today it writes only heartbeats and the closing handshake, but it
  exists as its own goroutine so that when a later week pushes match results to drivers, there is
  one obvious place for it rather than a race waiting to happen.

They share a per-connection context, so whichever notices the connection has ended cancels the
other.

## The bug the tests caught

`Shutdown` timed out with 5 connections still active.

**`conn.ReadMessage()` does not return when a context is cancelled.** gorilla has no context-aware
read — the goroutine sits in a socket read until its deadline (up to 60 s) or until the connection
is closed underneath it. Cancelling the context told `writePump` to stop and did nothing at all to
`readPump`.

The fix is a third small goroutine per connection whose only job is:

```go
<-ctx.Done()
conn.Close()      // the only thing that unblocks a blocked reader
```

It cannot leak, because the context is always cancelled on every exit path. This is the kind of
thing that is obvious once seen and invisible until a test demands a *bounded* shutdown.

## Errors that must not kill the connection

- **A malformed frame** is counted and dropped. A driver app emitting one bad message during an
  upgrade should not lose its GPS stream — but a permanently broken client must be *visible*, which
  is what `PingsRejected` is for.
- **A sink error** (Redis down) does not disconnect anyone. Dropping every connection in the city
  would turn a recoverable dependency failure into a reconnect storm.

## Checkpoint

> ✅ Clients connect, stream, and disconnect cleanly; killing a client frees its goroutine.

```
goroutines: 3 before, 3 after 100 connect/disconnect cycles
```

The connections are closed **abruptly**, with no closing handshake — what a force-quit or a dead
battery looks like from the server's side. 100 cycles rather than one, because a leak of one is
easy to lose in noise and a leak of 200 is not.

## Also verified

- **`Shutdown` waits**, rather than merely signalling. Returning early would report a clean exit
  while goroutines were still running and still writing to Redis.
- **Shutdown is idempotent** (`sync.Once` on a channel close).
- **The connection limit is a live gauge**, not a one-way counter: freeing a slot lets a new client
  in.
- **25 concurrent clients × 20 pings under `-race`**, which is what proves the counters and sink
  are genuinely shareable.

## A note left in the code

`CheckOrigin` currently returns `true` for everything. Drivers connect from a native app with an
API key (Week 18), not a browser, so authentication is the real defence — but leaving that
unremarked is exactly how it silently ships, so it is called out at the definition site.

## Files touched

`internal/ingest/{server,server_test}.go`, `infrastructure/go.mod`.
