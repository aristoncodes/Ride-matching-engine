#!/usr/bin/env bash
# Capture a pprof baseline while the system is under load.
#
# The point of a BASELINE is that Week 22's optimisations get measured against
# a real starting point rather than a memory of one. Profiles are written to
# profiles/<label>/ and can be diffed later with:
#
#   go tool pprof -base profiles/baseline/batcherd.cpu.pb.gz \
#                       profiles/after-fix/batcherd.cpu.pb.gz
#
# A profile of an IDLE system is worthless — it shows you the sleep loop. This
# script therefore expects load to already be running, and says so if the
# process looks idle.
#
# Usage:
#   ./scripts/profile.sh [label] [seconds]
#
# Example:
#   ./k8s/deploy.sh                                   # or docker compose up -d
#   go run ./cmd/loadtest --mode=pipeline --drivers 5000 &
#   ./scripts/profile.sh baseline 30

set -euo pipefail

LABEL=${1:-baseline}
SECONDS_TO_PROFILE=${2:-30}
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/profiles/$LABEL"

# service:admin-port
SERVICES=(
  "ingestd:6060"
  "requestd:6061"
  "batcherd:6062"
)

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null || die "go is required"
mkdir -p "$OUT"

log "profiling to $OUT (${SECONDS_TO_PROFILE}s CPU samples)"

reachable=0
for spec in "${SERVICES[@]}"; do
  name="${spec%%:*}"
  port="${spec##*:}"
  base="http://localhost:$port"

  if ! curl -fsS -m 3 "$base/healthz" >/dev/null 2>&1; then
    warn "$name: no admin server on $port (skipping)"
    continue
  fi
  reachable=$((reachable + 1))

  log "$name"

  # Runtime snapshot first: it is instant, and it is what tells you whether the
  # process is doing anything at all before you spend 30 seconds sampling it.
  curl -fsS -m 5 "$base/debug/runtime" > "$OUT/$name.runtime.txt" || true
  goroutines=$(awk '/^goroutines /{print $2}' "$OUT/$name.runtime.txt" 2>/dev/null || echo 0)
  printf '    goroutines=%s\n' "$goroutines"

  # Heap and allocs are cheap snapshots, so take them before the long CPU run.
  #   heap   = what is live NOW (leak hunting)
  #   allocs = everything ever allocated (GC pressure; the Week 22 target)
  curl -fsS -m 20 "$base/debug/pprof/heap" > "$OUT/$name.heap.pb.gz" || warn "$name: heap failed"
  curl -fsS -m 20 "$base/debug/pprof/allocs" > "$OUT/$name.allocs.pb.gz" || warn "$name: allocs failed"
  curl -fsS -m 20 "$base/debug/pprof/goroutine" > "$OUT/$name.goroutine.pb.gz" || warn "$name: goroutine failed"

  # CPU last, and in the background, so all services are sampled over the SAME
  # window. Profiling them one after another would compare different moments of
  # the load profile and invite the wrong conclusion.
  (
    curl -fsS -m "$((SECONDS_TO_PROFILE + 30))" \
      "$base/debug/pprof/profile?seconds=$SECONDS_TO_PROFILE" \
      > "$OUT/$name.cpu.pb.gz" 2>/dev/null || warn "$name: cpu profile failed"
  ) &
done

[[ "$reachable" -gt 0 ]] || die "no admin servers reachable — is the stack running?"

log "sampling CPU for ${SECONDS_TO_PROFILE}s across $reachable service(s)..."
wait
log "capture complete"

# ---- Summarise, so the script produces an ANSWER and not just files ---------
echo
log "top CPU consumers"
for spec in "${SERVICES[@]}"; do
  name="${spec%%:*}"
  f="$OUT/$name.cpu.pb.gz"
  [[ -s "$f" ]] || continue
  echo
  printf '  --- %s ---\n' "$name"
  # -nodecount keeps it readable; cum sorts by cumulative time, which is what
  # points at the CALL PATH responsible rather than the leaf that happens to be
  # hot (usually runtime internals, which you cannot act on).
  go tool pprof -top -cum -nodecount=12 "$f" 2>/dev/null \
    | sed -n '/flat/,$p' | head -14 | sed 's/^/    /' || true
done

echo
log "top allocation sites (cumulative bytes)"
for spec in "${SERVICES[@]}"; do
  name="${spec%%:*}"
  f="$OUT/$name.allocs.pb.gz"
  [[ -s "$f" ]] || continue
  echo
  printf '  --- %s ---\n' "$name"
  go tool pprof -top -cum -sample_index=alloc_space -nodecount=10 "$f" 2>/dev/null \
    | sed -n '/flat/,$p' | head -12 | sed 's/^/    /' || true
done

cat <<EOF

Files in $OUT

Explore interactively:
  go tool pprof -http=: $OUT/batcherd.cpu.pb.gz

Compare against a later run (this is the point of a baseline):
  go tool pprof -base $OUT/batcherd.cpu.pb.gz profiles/after-fix/batcherd.cpu.pb.gz
EOF
