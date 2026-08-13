# Week 18 — API Key Management

**Date:** Nov 5, 2026 · **Phase:** 5 (Production Orchestration) · **Status:** ✅ Complete

## What this week was about

Authenticating B2B clients, and making the tenant something the *server* determines rather than
something the client asserts. Week 19's isolation is impossible without this.

## Key format

```
rmk_<key_id>_<secret>
     ^^^^^^  ^^^^^^^^
     lookup  the part that is actually secret
```

Splitting the id from the secret makes verification an **O(1) lookup**. Hashing the whole key forces
either a table scan per request or a reversible — i.e. useless — hash.

The `rmk_` prefix is deliberately distinctive so **secret scanners can match it**. GitHub's scanner
and most commercial ones work on known prefixes; that is what turns "a key was committed by
accident" into an automatic revocation instead of a breach.

## SHA-256, not bcrypt — and why that isn't the mistake it looks like

This is the decision most likely to be challenged, so the reasoning is in the package doc.

Password hashing is **deliberately slow** because passwords are low-entropy and guessable. The
slowness is the entire defence.

An API key here is **32 bytes from `crypto/rand` — 256 bits**. Brute force is not the threat model at
that size, so slowness buys nothing and costs a great deal: bcrypt at ~100 ms per verification caps
the API at roughly **ten authenticated requests per second per core**.

SHA-256 over a high-entropy secret is the correct primitive, and it is what Stripe, GitHub and AWS
use for the same reason. What actually matters is that **the raw key is never stored** — a database
dump hands over no working credentials.

## The security details that are easy to skip

**Constant-time comparison.** A byte-wise `==` returns faster on an early mismatch, and that timing
difference recovers a secret one byte at a time given enough requests.

**One error for four failure modes.** Malformed, unknown, revoked and expired all return
`ErrInvalidKey`. Distinguishing them tells an attacker which key ids exist, turning guessing into
enumeration. The real reason is logged server-side.

**The active check runs AFTER the secret comparison.** Checking first would let someone learn that a
key id exists and is revoked *without knowing the secret* — the same enumeration leak by another
route.

## Rotation needs an overlap, or nobody rotates

```go
Rotate(ctx, keyID, overlap) // both keys work during the window
```

Revoking the instant a replacement is minted breaks every client that has not yet redeployed. That
is precisely why teams avoid rotating at all — and **unrotated keys are the actual security
problem**, so the overlap is a security feature, not a convenience.

`RotatedFrom` links the pair, so an operator sees a rotation rather than two unrelated keys.

## Rate limiting

Per-key, so one noisy tenant cannot consume another's capacity.

**Lua, because `INCR` and `EXPIRE` must be atomic.** As two commands, a crash between them leaves a
counter with no TTL — it never resets, and that key is **silently locked out forever**. Invisible
until a customer complains.

**Fails open.** A limiter that fails closed turns a Redis blip into a total outage. The limiter exists
to stop noisy neighbours, and briefly not enforcing it is far less harmful than refusing every
authenticated request.

Fixed window, chosen knowing its flaw: a client can send 2× the limit across a boundary. That is
acceptable for starvation prevention and would **not** be acceptable if it ever backed billing —
noted in the code.

## Status codes carry meaning

| Situation | Code | Why not the obvious alternative |
|---|---|---|
| Over quota | **429** + `Retry-After` | 401 would send them rotating a key that is fine |
| Auth store down | **503** | 401 tells a customer their valid key is invalid **during our outage** |
| Bad/revoked/expired key | 401 | one message for all of them |

Health probes bypass auth entirely — a kubelet has no API key, and requiring one makes every pod
permanently unready. An outage caused by the auth layer.

## Checkpoint

> ✅ A revoked key is rejected instantly.

**Instantly** with no caveats: there is no cache to expire, because every request re-reads the store.
That is what makes revocation a security control rather than a suggestion. A cache here would be a
real tradeoff needing an explicit invalidation story.

The record is **retained rather than deleted**, so "when was this key revoked?" — a question incident
response actually asks — has an answer.

> ⚠️ A dropped socket reconnects without losing session state.

**Partially met, and worth being precise.** The WebSocket upgrade is now authenticated *before*
upgrading (a 401 is far easier for an integrator to debug than a close frame), and driver state
survives a disconnect because positions live in Redis under a 30 s TTL — a reconnect inside that
window loses nothing.

What is **not** built is an explicit resume protocol: a session token, and a server-sent "here is
your last known state" message. State survives; resumption is implicit rather than negotiated. Called
out rather than claimed.

## Files touched

`internal/auth/{apikey,redis_store,middleware,auth_test}.go`, `internal/api/server.go`,
`internal/ingest/server.go`.
