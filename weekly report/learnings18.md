# Learnings — Week 18 (API Keys, Credential Handling)

Report: [week18.md](week18.md)

## 1. SHA-256 for API keys, bcrypt for passwords — know why

This is the one people get wrong in both directions.

| | Passwords | API keys |
|---|---|---|
| Entropy | low, human-chosen | 256 bits from a CSPRNG |
| Threat | offline brute force | key theft |
| Hash | **bcrypt/argon2 — slow on purpose** | **SHA-256 — fast** |

Password hashing is slow *because* passwords are guessable; the slowness is the defence. For a
256-bit random key brute force is not the threat model, so slowness buys nothing — and bcrypt at
~100 ms caps you at **~10 authenticated requests per second per core**.

Stripe, GitHub and AWS all hash API keys with SHA-256 for this reason. **What actually matters is
that the raw key is never stored**, so a database dump yields no working credentials.

**Interview soundbite:** "Slow hashing defends low-entropy secrets. For a 256-bit random token it is
pure cost — the property you need is non-recoverability, not slowness."

## 2. Split the key so lookup is O(1)

```
rmk_<key_id>_<secret>
```

Hash the whole key and you must either scan every row per request or use a reversible hash. The id
gives you a direct lookup; the secret is what you verify.

**A distinctive prefix is a security feature.** Secret scanners match on known prefixes — that is what
turns an accidental commit into an automatic revocation instead of a breach.

## 3. Constant-time comparison

```go
subtle.ConstantTimeCompare(expected, presented)
```

A byte-wise `==` returns faster on an early mismatch. That timing difference recovers a secret one
byte at a time given enough requests. Cheap to do right; genuinely exploitable to get wrong.

## 4. Error messages leak

**One error for malformed, unknown, revoked and expired.** Distinguishing them tells an attacker which
key ids exist — guessing becomes enumeration.

And the ordering matters: **check validity AFTER comparing the secret.** Checking first leaks "this
id exists and is revoked" to someone who never knew the secret.

Log the real reason server-side. The operator needs it; the caller must not have it.

## 5. Rotation without an overlap is rotation nobody does

Revoke-on-mint breaks every client that has not yet redeployed. So teams don't rotate — and
**unrotated keys are the actual security problem**.

An overlap window makes rotation safe, which makes it happen. A `RotatedFrom` link shows an operator
a rotation rather than two unrelated keys appearing.

**Interview soundbite:** "Security controls that are painful get bypassed. The overlap window exists
so rotation actually happens."

## 6. Atomicity in the rate limiter

```lua
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("EXPIRE", KEYS[1], ARGV[1]) end
```

As two round trips, a crash between them leaves a counter **with no TTL** — it never resets, and that
key is silently locked out forever. Invisible until a customer complains. Lua makes it one operation.

**Fail OPEN.** A limiter exists to stop noisy neighbours; failing closed turns a Redis blip into a
total outage. The failure mode you choose *is* the design.

**Fixed vs sliding window:** fixed allows 2× across a boundary. Fine for starvation prevention,
**not** fine if it ever backs billing. Write down which one you're relying on.

## 7. Status codes are instructions

| Code | Means | Client does |
|---|---|---|
| 401 | your credential is bad | fix the credential |
| **429** | credential fine, too fast | **back off** |
| **503** | **our** auth store is down | **retry unchanged** |

Returning 401 when *your* Redis is down tells a customer their valid key is invalid, and they will go
rotate credentials that were never the problem. **During an outage, be careful what you blame.**

## 8. Health probes must bypass auth

A kubelet has no API key. Require one and every pod is permanently unready — an outage caused
entirely by the auth layer.

## 9. Revocation must be instant, which means no cache

Every request re-reads the store. That is what makes revocation a **control** rather than a
suggestion. A cache would be a real tradeoff needing an explicit invalidation story — and "revoked
keys work for another 60 seconds" is a sentence you must be willing to say out loud.

**Retain the record, don't delete it.** "When was this revoked?" is a question incident response
actually asks.

## 10. Take the tenant from the credential, never the request

```go
tenant := key.TenantID   // proven
// NOT r.Header.Get("X-Tenant-ID")  // asserted
```

This is the hinge the whole of Week 19 turns on, and it is one line.

---

## Self-test
1. Why SHA-256 for API keys but bcrypt for passwords?
2. Why split the key into an id and a secret?
3. Why compare hashes in constant time?
4. Why must "unknown key" and "revoked key" return the same error — and why does check ORDER matter?
5. Why does rotation need an overlap window?
6. What breaks if `INCR` and `EXPIRE` aren't atomic?
7. Should a rate limiter fail open or closed? Justify it.
8. Your auth Redis is down. What status code, and what happens if you choose 401?
9. Why can't health probes require authentication?
