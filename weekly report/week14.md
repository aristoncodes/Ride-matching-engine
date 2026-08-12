# Week 14 — Docker Compose (one command boots the stack)

**Date:** Oct 8, 2026 · **Phase:** 4 (DevOps & Benchmarking) · **Status:** ✅ Complete

## What this week was about

Five processes — Redis, a C++ gRPC server, and three Go services — booting together, in the right
order, reproducibly.

## First: the deliverable contradicted an ADR

The TDD said "Go, C++, Redis, **and Kafka**". That line was written in Week 0. In Week 10,
[ADR-0006](../docs/adr/0006-redis-streams-over-kafka.md) chose **Redis Streams over Kafka**, and
*avoiding exactly this compose file* was one of the stated reasons.

Adding a Kafka broker nothing connects to would have made the stack heavier to contradict its own
architecture. I amended the TDD instead, with the reasoning recorded inline.

**Keeping a plan and a decision log in sync is part of the work.** A plan that disagrees with the
system is a plan people stop trusting.

## Images

| Image | Size | Base |
|---|---|---|
| `engine` | 179 MB | debian-slim + gRPC shared libs |
| `ingestd` | 18.9 MB | **scratch** |
| `requestd` | 18.6 MB | **scratch** |
| `batcherd` | 31.5 MB | **scratch** |

**C++**: a builder stage with gRPC's dev headers (~1 GB) produces a runtime stage carrying only the
shared libraries the binary links against. Shipping the builder would mean a compiler in production
— a bigger image, a bigger attack surface, and a slower pull on every deploy.

**Go**: `CGO_ENABLED=0` makes a statically linked binary, which makes `FROM scratch` possible —
no libc, no shell, no package manager. There is nothing in the image for an attacker to use and
nothing to patch for CVEs. One parameterised Dockerfile builds all three services, because three
near-copies drift.

Both run as a non-root uid. The engine needs no privileges: it opens one port above 1024, reads one
file, and writes nothing.

## The `scratch` problem: you cannot health-check a shell-less image

```dockerfile
HEALTHCHECK CMD curl -f http://localhost:8080/healthz   # needs /bin/sh AND curl
```

`scratch` has neither. So `cmd/healthcheck` is a tiny static Go probe compiled into each image and
invoked in **exec form**:

```dockerfile
HEALTHCHECK CMD ["/healthcheck", "http://127.0.0.1:8080/healthz"]
```

It treats any 2xx as healthy and — deliberately — **503 as unhealthy**, because 503 is exactly how
the readiness endpoint reports a dependency outage.

## Startup ordering, and why the bare form is wrong

```yaml
depends_on:
  redis:
    condition: service_healthy     # NOT `depends_on: [redis]`
```

Plain `depends_on` waits only for the container to **start**. Redis accepts TCP connections while
still loading its AOF, so the Go services would connect, issue commands, and fail.

And Redis's own healthcheck is `redis-cli ping`, not a TCP connect, for the same reason: answering
PING means it is genuinely ready, while accepting a socket means almost nothing.

`requestd` probes **`/readyz`**, not `/healthz` — the Week 11 split exists precisely so that
readiness is the probe that gates traffic.

## Config and durability decisions

**Everything pinned.** `redis:8.0.3-alpine`, `debian:bookworm-20250630-slim`, and every apt package
version-pinned. "Works on my machine" is usually "my machine had a different base image".

**Redis runs with AOF on** (`appendfsync everysec`). ADR-0006 named in-memory durability as the
accepted downside of Redis Streams; this is the mitigation it called for, bounding worst-case loss
to about a second of ride requests rather than all of them.

**`maxmemory-policy noeviction`** is a correctness decision, not a tuning knob. Any `allkeys-*`
policy would silently evict ride requests under pressure. Failing writes loudly is correct.

## Checkpoint

> ✅ `docker compose up` yields a healthy stack every time.

```
run 1: all-healthy=YES  in 12s
run 2: all-healthy=YES  in 13s
run 3: all-healthy=YES  in 12s
```

Three cold cycles with `down -v` in between — the "every time" is measured, not assumed. Then the
full flow through the containers: 50 drivers over WebSockets, 25 riders over REST, 14 matched,
0 solve errors.

## The bug containerising found

The batcher's startup engine-probe **timed out**, putting the Week 12 cold-start bug straight back.

`Client.Health` caps at the client's 2 s default regardless of the context passed in, and a *first*
gRPC connection inside Docker — DNS for `matching-engine`, TCP, HTTP/2 handshake — took longer than
that. So the fix for a cold-connection problem was itself defeated by a cold connection.

Fixed with a longer client timeout (harmless, since `SolveTimeout` still bounds actual solves) plus
retry with a 45 s budget. The probe now succeeds on attempt 1:

```
msg="matching engine reachable" version=week6-dev graphs=1 attempts=1
```

**A fix verified only in one environment is verified in one environment.**

## Files touched

`matching_engine/Dockerfile`, `infrastructure/Dockerfile`, `docker-compose.yml`, `.env.example`,
`.dockerignore`, `infrastructure/cmd/healthcheck/main.go`, `infrastructure/cmd/batcherd/main.go`.
