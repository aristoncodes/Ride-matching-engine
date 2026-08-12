# Learnings — Week 14 (Containers, Multi-Stage Builds, Startup Ordering)

The packaging week. Concepts and interview-ready takeaways.
Report: [week14.md](week14.md)

## 1. Multi-stage builds — the point is what you DON'T ship

```dockerfile
FROM debian AS builder     # compiler, headers, ~1 GB
RUN cmake --build ...
FROM debian-slim AS runtime
COPY --from=builder /build/binary /usr/local/bin/
```

Shipping the builder means a compiler in production: a bigger image, a bigger attack surface, and a
slower pull on every deploy and every autoscale event.

**Interview soundbite:** "Build dependencies and runtime dependencies are different sets. Multi-stage
lets you install the first and ship only the second."

## 2. `FROM scratch` for Go, and what makes it possible

```dockerfile
CGO_ENABLED=0 go build   # static binary — no libc needed
FROM scratch
COPY --from=builder /out/service /service
```

18 MB, no shell, no package manager, no libc. Nothing for an attacker to pivot with and nothing to
patch for CVEs.

**Two things `scratch` still needs**, and both are easy to forget until something fails oddly:

- **CA certificates** — without them any outbound TLS fails with an opaque x509 error.
- **`/etc/passwd`** — `USER 10001` with no user database leaves a uid the image cannot name.

`CGO_ENABLED=0` is the load-bearing part. With cgo on, the binary links the builder's glibc, which
does not exist in `scratch`, and the container exits immediately with a confusing error.

## 3. Health-checking a shell-less image

```dockerfile
HEALTHCHECK CMD curl -f http://localhost:8080/healthz   # needs /bin/sh AND curl
```

`scratch` and distroless have neither. Options: a static probe binary compiled into the image
(what I did), or a `--health-check` flag on the service itself.

**Exec form is mandatory** — `CMD ["/healthcheck", "..."]`. The shell form invokes `/bin/sh -c`.

And make the probe treat **503 as unhealthy**, since that is exactly how a readiness endpoint
reports a dependency outage.

## 4. `depends_on` does not mean what people think

```yaml
depends_on: [redis]                    # waits for the container to START
depends_on:
  redis: { condition: service_healthy } # waits for it to be READY
```

Redis accepts TCP connections while still loading its AOF. Postgres does the same during recovery.
The bare form produces a race that passes on a fast laptop and fails in CI.

**And healthcheck the protocol, not the port.** `redis-cli ping` proves Redis will answer; a TCP
connect proves almost nothing.

**Interview soundbite:** "Container started, process running, and service ready are three different
states, and orchestration bugs usually come from conflating them."

## 5. Pin everything

`redis:8.0.3-alpine`, `debian:bookworm-20250630-slim`, and apt packages down to the version.
"Works on my machine" is very often "my machine pulled a different `:latest` last Tuesday".

**Practical note:** pin to versions the base image *actually ships*. I guessed `3.21.12-3` and the
build failed; the real string was `3.21.12-3+deb12u1`. `apt-cache policy` in the base image tells
you, and guessing costs a full build cycle.

## 6. Layer caching follows COPY order

```dockerfile
COPY go.mod go.sum ./     # changes rarely -> cached
RUN go mod download
COPY . .                  # changes constantly
RUN go build
```

Copy the least volatile thing first. I copy the `.proto` on its own before any source, so editing a
`.cpp` does not re-run protoc.

**`.dockerignore` matters too**: every byte sent to the daemon slows the build, and copying `.git`
or a `build/` directory can bust the cache on every commit.

## 7. Config in env files, not in the compose file

The compose file should be identical across environments; only values change. `.env.example` is
committed, `.env` is not.

**And gitignored is not secure.** Real secrets belong in a secret manager (Week 18), not in a file
that merely is not tracked.

## 8. Durability and eviction are correctness decisions

For the Redis holding the ride-request queue:

- `appendonly yes`, `appendfsync everysec` — bounds worst-case loss to ~1 s of requests. This is
  the exact mitigation ADR-0006 promised when it accepted Redis's in-memory nature.
- `maxmemory-policy noeviction` — **any `allkeys-*` policy would silently evict ride requests under
  memory pressure.** Failing writes loudly is correct; silently dropping customer intent is not.

**Interview soundbite:** "Eviction policy is a correctness decision when the cache is also the queue."

## 9. Non-root by default

The engine opens one port above 1024, reads one file, writes nothing. Root buys it nothing and
gives a container escape somewhere to go.

## 10. A fix verified in one environment is verified in one environment

The Week 12 cold-connection fix worked on the host and **failed in Docker**, because the first gRPC
connection there (DNS + TCP + HTTP/2) exceeded a 2 s client default. The fix for a cold-start
problem was itself defeated by a cold start.

**Containerising is not just packaging — it changes timing, DNS, and network behaviour, and it will
find latent assumptions about all three.**

---

## Self-test

1. What do you avoid shipping with a multi-stage build, and why does it matter beyond image size?
2. What does `CGO_ENABLED=0` have to do with `FROM scratch`?
3. Name two things a `scratch` image still needs.
4. How do you health-check an image with no shell?
5. What is wrong with `depends_on: [redis]`?
6. Why `redis-cli ping` rather than a TCP port check?
7. Why does COPY order affect build speed?
8. Your Redis is both cache and queue. What `maxmemory-policy` do you choose, and why?
9. A timeout fix works locally and fails in Docker. What changed?
