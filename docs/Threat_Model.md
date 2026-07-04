# Threat Model

**Status:** Draft · **Method:** STRIDE · **Related:** [Data_Model.md](Data_Model.md), [api/rest-openapi.yaml](api/rest-openapi.yaml)

Multi-tenancy and API-key handling are the security-critical parts of this system. This model is written before those features are built (Weeks 18–19) so the controls are designed in, not bolted on.

---

## 1. Assets to protect
- **Tenant data isolation** — the core B2B promise; a leak is existential.
- **API keys** — authentication credentials for institutional clients.
- **Ride request / location data** — potentially sensitive movement data.
- **Availability** — a dropped-request or downtime event breaches SLAs.

## 2. Trust boundaries
```
[ Client ] --(1)--> [ Go API / WebSocket ] --(2)--> [ Queue / Redis / DB ] --(3)--> [ C++ Engine ]
```
1. Untrusted client → Go: **authentication + validation boundary**.
2. Go → data stores: **tenant-scoping boundary**.
3. Go → C++: **internal, trusted** (private network), but still deadline-bounded.

## 3. STRIDE analysis

| Threat | Example | Mitigation | Milestone |
|--------|---------|------------|-----------|
| **S**poofing | Attacker uses another tenant's identity | API keys hashed at rest; per-request auth resolves exactly one tenant | Wk 18 |
| **T**ampering | Modified request payload | Input validation on every field; TLS in transit | Wk 11/18 |
| **R**epudiation | "We never sent that request" | Structured logs with request IDs + tenant IDs (immutable) | Wk 23 |
| **I**nformation disclosure | Tenant A reads Tenant B's drivers/matches | **Tenant ID enforced at every layer** (API scope, cache-key prefix, queue partition, DB row filter); automated cross-tenant denial test | Wk 19 |
| **D**enial of service | Flood of requests / giant payloads | Per-key rate limiting; max payload + max connection caps; backpressure/load-shedding | Wk 9/18 |
| **E**levation of privilege | Client reaches admin/internal endpoints | No admin surface on the public API; internal gRPC not internet-exposed | Wk 6/18 |

## 4. Multi-tenancy: the isolation contract (highest priority)
Tenant isolation must hold at **all four** layers — a gap in any one defeats the others:
1. **API:** the API key maps to exactly one `tenant_id`; requests can only reference their own tenant.
2. **Cache (Redis):** keys are prefixed with `tenant_id`; queries can't span prefixes.
3. **Queue:** partitioned/namespaced by tenant so a consumer only sees its tenant's messages.
4. **Database:** every row carries `tenant_id`; every query filters on it (ideally enforced at the ORM/repo layer, not per-hand-written-query).

**Verification (Week 19):** an automated test authenticates as Tenant A and attempts, at each layer, to read Tenant B's data. All attempts must be denied. *Untested isolation is not isolation.*

## 5. API-key handling
- Stored **hashed** (e.g., SHA-256 + salt), never in plaintext.
- Supports **rotation** without downtime (accept old+new during a window).
- **Rate-limited per key** to bound abuse.
- Revocation takes effect immediately.

## 6. Out of scope (v1) / accepted risks
- No end-user (rider) authentication — clients authenticate on their riders' behalf.
- No encryption-at-rest requirement beyond the DB's default (revisit if handling regulated data).
- DDoS at the network edge is delegated to the cloud provider / ingress, not this app.

## 7. Open questions
- Do any tenants require data residency (region pinning)?
- Is location data classified as PII under any target client's jurisdiction? (would raise retention/encryption requirements)
