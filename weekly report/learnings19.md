# Learnings — Week 19 (Multi-Tenancy and Isolation)

Report: [week19.md](week19.md)

## 1. The tenant must be PROVEN, never ASSERTED

```go
tenant := authenticatedKey.TenantID       // proven by the credential
tenant := r.Header.Get("X-Tenant-ID")     // asserted by the caller
```

If a client can name its own tenant, **every downstream key prefix is decoration**. This one line is
the foundation; everything else is defence in depth on top of it.

Corollary: **the absence of a per-request override is the security control.** There is no header, no
query param, no body field. Adding one "for testing" would quietly delete the whole property.

**Interview soundbite:** "Tenancy is an authentication output, not a request input."

## 2. Scope every layer, and enumerate them explicitly

```
API auth      -> tenant from the key
queue         -> requests:stream:<tenant>
dead-letter   -> requests:dead:<tenant>      <- the one people forget
cache         -> drivers:geo:<tenant>
locks         -> lock:driver:<tenant>:<id>
engine        -> tenant_id on every batch
logs          -> tenant on every line
```

**The dead-letter queue is the classic miss.** The happy path gets scoped and the failure path
doesn't — and dead-lettered requests contain rider ids and pickup coordinates. Reading a competitor's
failures is as damaging as reading their successes.

## 3. In-memory layers can't be protected by key prefixes

The pipeline buffer is a Go map shared across tenants. Keyed by driver id alone, two operators who
both have a `D-001` **collide before any Redis key is involved** — one tenant's ping silently
relocates the other's driver.

That presents as a flapping GPS bug and would take a long time to trace back to multi-tenancy.

**Rule: any shared in-process structure needs the tenant in its key, exactly like the datastore does.**

## 4. Test isolation as a COMPOSITION, in its own package

Each package's tests can only check its own layer. **A leak between two individually-correct layers is
the kind nobody notices.**

So `internal/tenancy` is a separate package that imports everything and tests the seams. That is also
what forces you to write it from the outside, the way an attacker would.

## 5. Write the tests as attacks

Not "does scoping work?" but "**holding only tenant A's key, can I reach tenant B's data?**" — spoof a
header, read their drivers, consume their queue, read their dead-letters, act unauthenticated.

The framing changes what you write. "Does scoping work" produces a happy-path test; "can I break in"
produces the header-spoof test.

## 6. Design the test so ONLY the property under test can pass it

Both fleets are placed at the **same coordinates**. If they were in different cities, a radius query
would separate them by geography and the test would pass even with tenant scoping removed entirely.

**Ask of every test: if I deleted the feature, would this still pass?** If yes, it tests nothing.

## 7. Same-id collisions are a real multi-tenant hazard

Two operators will both have `D-001`, `driver-1`, `1`. Any store, cache, lock, or map keyed on a
customer-supplied id **must** include the tenant. The failure is silent corruption, not an error.

## 8. The test bug: clocks and revocation

The revocation test failed with "the revoked key still works". It didn't — I read `time.Now()` *before*
revoking, and `Active()` compares the supplied clock to `RevokedAt`.

Worth internalising: **revocation is instant with respect to REQUEST time**, not to whatever clock a
caller is holding. Injected clocks make time-dependent logic testable and also make it possible to
write a test that is subtly asking the wrong question.

## 9. Isolation you haven't tested is isolation you don't have

The most quotable line in the whole TDD, and it is right. Tenant scoping is a handful of string
prefixes that are trivial to get right and equally trivial to forget in one place — and the one place
you forget is a data breach, not a bug report.

---

## Self-test
1. Why must the tenant come from the credential rather than the request?
2. Which layer of tenant scoping is most commonly forgotten, and why does it matter?
3. Why can't a key prefix protect an in-memory buffer?
4. Why put isolation tests in their own package instead of each component's tests?
5. Two tenants both have a driver called "D-001". What breaks, and how loudly?
6. You place both tenants' drivers in different cities for a radius test. What's wrong with that?
7. What does "isolation you haven't tested is isolation you don't have" mean concretely?
