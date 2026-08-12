# Week 11 — REST API Standards (the rider-facing front door)

**Date:** Sep 17, 2026 · **Phase:** 3 (Message Brokering & Batching) · **Status:** ✅ Complete

## What this week was about

Week 10 built a durable queue with nothing to fill it. This is the front door: the HTTP service
riders' apps actually talk to. It is deliberately thin — validate, assign a request id, publish,
return 202. It does not match, route, or talk to the C++ engine, which is what lets the matcher be
restarted or crash without riders seeing anything worse than a longer wait.

## One error envelope, everywhere

```json
{ "error": {
    "code": "invalid_argument",
    "message": "pickup.lat must be between -90 and 90",
    "field": "pickup.lat",
    "request_id": "req_9f2c4a1b8e7d6c5f4a3b2c1d" } }
```

A B2B client writes its error handling **once**. An API that returns a JSON object here, a bare
string there, and Go's default plain-text 404 somewhere else forces an integrator to write a parser
per endpoint — and they will get it wrong.

**`code` is stable and machine-readable; clients branch on it, never on `message`.** Messages get
reworded, and a client matching on prose breaks silently the first time someone fixes a typo.

## Validation, and the traps

**Pointer fields for coordinates.**

```go
type latLng struct {
    Lat *float64 `json:"lat"`
    Lng *float64 `json:"lng"`
}
```

Without pointers, `{"lat": 0}` and `{}` decode identically. And (0,0) is a real coordinate in the
Gulf of Guinea, so "the client forgot a field" and "the client is at Null Island" would be
indistinguishable.

**`DisallowUnknownFields`.** A client sending `pickupp` has a bug. Failing loudly finds it during
their integration rather than in production as a mysteriously absent pickup.

**Non-finite numbers.** A large float literal decodes to `+Inf`. Unchecked, it flows into the geo
index and the solver, where it produces nonsense rather than an error.

**`MaxBytesReader` before decoding**, so an oversized body is never fully buffered — the limit is
enforced *by* the decoder rather than checked after the damage.

**Decoder errors translated.** Go's raw `json: cannot unmarshal string into Go struct field ... of
type float64` leaks internal type names and reads as gibberish to an integrator.

## Request IDs

Honoured from an inbound `X-Request-ID` so a trace spans services — the batcher's log line should be
findable from the rider's original call. But **sanitised**: length-capped and stripped to
`[A-Za-z0-9._-]`.

An unbounded client-controlled string flowing into log lines and response headers is a
log/header-injection vector. There is a test that `evil\r\nX-Injected: yes` cannot inject a header.

## Status codes that mean something

| Situation | Code | Why |
|---|---|---|
| Accepted | **202** | not 201 — nothing is created, no driver assigned. Claiming otherwise is a lie the client might act on |
| Bad input | **400** | with a field name |
| Body too big | **413** | |
| Queue unreachable | **503** + `Retry-After` | the request was VALID; the fault is ours and transient, so the client should retry, not fix its input |

The internal error string never reaches the client — a Redis address or stack frame in an error body
is information disclosure and useless to the caller anyway. The request id is how the log is found.

## Liveness vs readiness are not the same probe

| | Checks Redis? | Effect of failing |
|---|---|---|
| `/healthz` (liveness) | **no** | container killed and restarted |
| `/readyz` (readiness) | **yes** | instance pulled from the load balancer |

A liveness probe that fails during a Redis outage gets every instance killed and restarted, which
fixes nothing and removes capacity exactly when the system is already degraded. A readiness probe
failing is the *correct* response. There is a test that fails the dependency and requires the two
endpoints to diverge.

## Checkpoint

> ✅ A malformed request gets a clear 4xx with a request ID, not a 500.

14 malformed-input cases — empty body, non-JSON, truncated JSON, every missing field, out-of-range
lat/lng, wrong types, unknown fields, two concatenated objects, empty and oversized `rider_id` —
each asserting:

1. a 4xx, **never** a 5xx;
2. a parseable envelope with a machine-readable code;
3. a request id;
4. **nothing invalid reached the queue.**

Plus a panic-recovery backstop so even an unforeseen 500 is structured and traceable rather than a
dropped connection.

## The OpenAPI spec was lying

It described `{"error": string, "request_id": string}` — flat — which the implementation never
produced. **A spec that lies is worse than no spec**, because integrators build against it.

Updated to the real envelope, with the code enum, the 413 and 503 responses, `Retry-After`, and the
split health/readiness endpoints.

## Files touched

`internal/api/{server,errors,server_test}.go`, `cmd/requestd/main.go`,
`docs/api/rest-openapi.yaml`.
