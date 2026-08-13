#!/usr/bin/env bash
# Week 21 — repeatable load profiles.
#
# Three named shapes, because "I hammered it for a bit" is not a measurement
# you can compare against next month:
#
#   steady   sustained nominal traffic. The baseline everything else is read
#            against, and the only one where a latency number means much.
#   spike    a sudden surge on top of idle — a concert letting out. Tests
#            backpressure and the dual-trigger flush, not throughput.
#   soak     lower rate, much longer. The ONLY profile that finds leaks,
#            unbounded growth, and TTL/reaper bugs, because those need time
#            rather than volume.
#
# Results are appended to results/<profile>-<timestamp>.txt AND summarised to
# stdout, so a run is comparable to previous runs rather than a screenshot.
#
# Usage:
#   ./scripts/loadprofile.sh steady [duration]
#   ./scripts/loadprofile.sh spike
#   ./scripts/loadprofile.sh soak
#   ./scripts/loadprofile.sh all

set -euo pipefail

PROFILE=${1:-steady}
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/profiles/results"
WS=${WS_URL:-ws://localhost:8090/v1/drivers/stream}
REST=${REST_URL:-http://localhost:8091}
BATCHER=${BATCHER_URL:-http://localhost:8092}

mkdir -p "$OUT"
log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
curl -fsS -m 3 "$REST/healthz" >/dev/null 2>&1 || {
  echo "requestd is not reachable at $REST — start the stack first" >&2; exit 1; }

LOADTEST="$ROOT/infrastructure"
BIN=/tmp/loadprofile-loadtest
(cd "$LOADTEST" && go build -o "$BIN" ./cmd/loadtest)

# snapshot <label> — capture the runtime counters that reveal slow leaks.
snapshot() {
  local label=$1
  {
    printf '\n--- %s ---\n' "$label"
    for p in 6060 6061 6062; do
      name=$(curl -fsS -m 3 "http://localhost:$p/" 2>/dev/null | head -1 | cut -d' ' -f1)
      [[ -n "$name" ]] || continue
      printf '%s: ' "$name"
      curl -fsS -m 3 "http://localhost:$p/debug/runtime" 2>/dev/null \
        | awk '/^(goroutines|heap_alloc_bytes|total_alloc_bytes|gc_cycles)/{printf "%s=%s ", $1, $2}'
      echo
    done
    printf 'batcher: '
    curl -fsS -m 3 "$BATCHER/stats" 2>/dev/null | tr '\n' ' '
    echo
  } | tee -a "$REPORT"
}

run_profile() {
  local name=$1 drivers=$2 riders=$3 rate=$4 desc=$5
  REPORT="$OUT/${name}-$(date +%Y%m%d-%H%M%S).txt"

  {
    echo "# Load profile: $name"
    echo "# $desc"
    echo "# $(date -u +%Y-%m-%dT%H:%M:%SZ)  host=$(uname -s)/$(uname -m) cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo ?)"
    echo "# drivers=$drivers riders=$riders rate=${rate}/s"
  } > "$REPORT"

  log "$name — $desc"
  snapshot "before"

  "$BIN" --mode=pipeline --ws "$WS" --rest "$REST" \
    --drivers "$drivers" --riders "$riders" --rate "$rate" 2>&1 \
    | tee -a "$REPORT" | grep -E "connected=|latency:|accepted=" | sed 's/^/    /'

  # Let the last windows drain before the after-snapshot, or the counters read
  # mid-flight and the comparison is meaningless.
  sleep 8
  snapshot "after"

  log "written to $REPORT"
}

case "$PROFILE" in
  steady)
    # Nominal load, held long enough for the system to reach steady state.
    run_profile steady 2000 3000 300 \
      "sustained nominal traffic — the baseline for latency comparisons"
    ;;

  spike)
    # The scenario this whole project exists for. The rate is deliberately far
    # above what the batch window can absorb, so backpressure is EXERCISED
    # rather than merely present.
    run_profile spike 3000 5000 1500 \
      "sudden surge on idle — exercises backpressure and the size-trigger flush"
    ;;

  soak)
    # Fewer requests, far longer. Volume finds throughput problems; TIME finds
    # goroutine leaks, unbounded buffers, TTL bugs, and slow memory growth —
    # and none of those show up in a 30-second run.
    log "soak: this runs for several minutes by design"
    run_profile soak 1500 9000 50 \
      "long low-rate run — the only profile that finds leaks and slow growth"
    ;;

  all)
    "$0" steady
    sleep 15
    "$0" spike
    sleep 15
    "$0" soak
    ;;

  *)
    echo "unknown profile: $PROFILE (want: steady | spike | soak | all)" >&2
    exit 2
    ;;
esac

echo
log "results in $OUT"
ls -1t "$OUT" | head -5 | sed 's/^/    /'
