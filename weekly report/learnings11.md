# Learnings — Week 11 (API Design, Defensive Input Handling)

The public-contract week. Concepts and interview-ready takeaways.
Report: [week11.md](week11.md)

## 1. One error envelope, for the whole API

```json
{ "error": { "code": "...", "message": "...", "field": "...", "request_id": "..." } }
```

A client writes its error handling **once**. Mixed shapes force a parser per endpoint.

**Clients branch on `code`, never on `message`.** Codes are a stable contract; messages get reworded
and a client matching on prose breaks silently.

Include your framework's defaults: Go's `http.NotFound` returns plain text, so unmatched routes need
an explicit handler or the envelope has a hole in it.

## 2. Presence vs zero — use pointers

```go
Lat *float64 `json:"lat"`
```

Without the pointer, `{"lat": 0}` and `{}` are identical after decoding. **(0,0) is a real
coordinate**, so "field missing" and "Null Island" become indistinguishable.

Same idea as `optional` in proto3 (Week 6). The question "can I tell absent from zero?" recurs
constantly.

## 3. `DisallowUnknownFields`

A client sending `pickupp` has a bug. Silently ignoring it means they ship, and the field is
mysteriously absent in production. Failing loudly finds it during integration.

The tradeoff: it makes adding a field a breaking change for strict clients. For a versioned B2B API
that is the right side to err on.

## 4. Bound the body *before* decoding

```go
r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
```

This makes the decoder itself fail at the limit. Checking `Content-Length` is trivially bypassed with
chunked encoding, and checking after reading means you already buffered it.

## 5. Translate decoder errors

`json: cannot unmarshal string into Go struct field latLng.pickup.lat of type float64` leaks internal
type names and reads as gibberish. Map `*json.UnmarshalTypeError`, `*json.SyntaxError`,
`*http.MaxBytesError`, and `io.EOF` to messages that say what was expected.

## 6. Status codes are semantics, not decoration

| Code | Means | Client should |
|---|---|---|
| **202** | queued, not created | poll or wait |
| 400 | your input is wrong | fix it |
| 413 | too big | send less |
| **503** + `Retry-After` | **our** problem, transient | **retry unchanged** |
| 500 | our problem, unexpected | report the request id |

**202 vs 201** matters: nothing was created and no driver assigned. Returning 201 is a lie a client
might act on.

**503 vs 500** matters more: a valid request that failed on a dependency is not a bug in the client
and not an unexpected fault. `Retry-After` makes the difference machine-readable.

## 7. Never leak internal errors

A Redis address, a stack frame, or a SQL fragment in an error body is information disclosure — and
useless to the caller anyway. Detail goes to the log; the client gets the request id.

## 8. Request IDs, and sanitising them

Honour an inbound `X-Request-ID` so traces span services. But **it is attacker-controlled**: cap the
length and strip everything outside `[A-Za-z0-9._-]`.

A CRLF in a value echoed into a response header injects a header. The same string in a log line
forges log entries. **Any client-controlled string that reaches a log or a header needs sanitising.**

Return it in a header *and* the body, so it is available even on a response the client failed to
parse.

## 9. Liveness ≠ readiness (an interview favourite)

| | Checks dependencies? | Failure means |
|---|---|---|
| Liveness | **no** | kill and restart the container |
| Readiness | **yes** | remove from the load balancer |

**Checking Redis in your liveness probe means a Redis outage restarts every instance you have** —
removing capacity while fixing nothing, and often turning a partial outage into a total one.

**Interview soundbite:** "Liveness answers 'is this process wedged?'. Readiness answers 'should
traffic come here right now?'. Conflating them turns a dependency blip into a rolling restart."

## 10. Panic recovery is a backstop, not a strategy

A recovery middleware turns an unforeseen panic into a structured 500 with a request id rather than a
dropped connection. It does not excuse missing validation — but the goal is that no client input can
reach it.

---

## Self-test

1. Why must clients branch on an error code rather than the message?
2. `{"lat": 0}` vs `{}` — why does the difference matter, and how do you capture it?
3. Why bound the request body before decoding rather than after?
4. When is 503 correct and 500 wrong?
5. Why 202 rather than 201 for an enqueued request?
6. What can go wrong if you echo an inbound `X-Request-ID` unsanitised?
7. Your liveness probe checks Redis. Redis goes down. What happens to your fleet?
8. What does `DisallowUnknownFields` buy you, and what does it cost?
