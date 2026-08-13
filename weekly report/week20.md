# Week 20 — Profiling (measure before you touch anything)

**Date:** Nov 19, 2026 · **Phase:** 6 (Enterprise Hardening) · **Status:** ✅ Complete

## What this week was about

Establishing where time and memory actually go, so Week 22 optimises evidence
rather than intuition.

## pprof belongs on its own port

```go
mux := http.NewServeMux()   // OUR mux, never http.DefaultServeMux
```

`net/http/pprof` registers itself on `http.DefaultServeMux` **as an import side
effect**. A service that imports it and also serves its API from the default mux
publishes, on its public port:

- `/debug/pprof/heap` — your memory layout
- `/debug/pprof/goroutine` — every function you are running
- `/debug/pprof/profile` — 30 seconds of CPU, burned on request, by anyone

That last one is a free denial of service. This is one of the most common
accidental exposures in Go services, and the fix is structural: a separate mux
*and* a separate listener, so the deployment decides who can reach it.

## Two details that would have wasted an afternoon

**`WriteTimeout` is 6 minutes.** A CPU profile is a 30-second *streaming*
response. A conventional 15s write timeout truncates it — and the symptom is not
"timeout", it is `unrecognized profile format` from a corrupt file.

**Block and mutex profiling are opt-in per run.** Both add overhead to every
blocking operation and every mutex acquisition, which is exactly the hot path in
this codebase. Leaving them on changes the thing being measured.

## The script produces an answer, not a directory

`scripts/profile.sh` captures all services **over the same window** — profiling
them one after another compares different moments of the load profile and invites
a wrong conclusion — then prints a ranked summary.

It also refuses to be useful on an idle system: a profile of an idle process
shows you the scheduler.

## Checkpoint

> ✅ You can point at the specific functions consuming CPU and allocations.

```
--- batcherd ---
      cum   cum%
    810ms 64.80%  batcher.(*Batcher).Run
    740ms 59.20%  batcher.(*Batcher).processBatch
    740ms 59.20%  go-redis.(*baseClient).processCommand
    690ms 55.20%  syscall.rawsyscalln
```

**`processBatch` spending ~59% of CPU inside Redis calls.** A specific function,
a specific cause. That is the Week 22 target.

For context, the other two services were exactly as expected and needed nothing:
`ingestd` dominated by `websocket.ReadMessage` (I/O bound with 3,000
connections — 12,009 goroutines, ~4 per connection, matching the design), and
`requestd` by `syscall` in the HTTP path.

## The bug

The first capture produced 47-byte "profiles" that pprof rejected as an
unrecognised format. They were the **index page**.

```go
EnableProfiling bool   // documented "on by default"
```

Callers construct `Config{Addr: ..., ServiceName: ..., Logger: ...}` — without
the field. Its zero value is `false`. Profiling was silently off, and the index
handler served `/debug/pprof/heap` as a fallback.

**A bool whose zero value contradicts its documented default is a trap.**
Inverted to `DisableProfiling`, so the zero value does the thing the package
exists to do and turning it off is an explicit act.

## Files touched

`internal/adminserver/server.go`, `scripts/profile.sh`,
`cmd/{ingestd,requestd,batcherd}/main.go`.
