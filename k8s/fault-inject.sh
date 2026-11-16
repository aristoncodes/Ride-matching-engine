#!/usr/bin/env bash
# WEEK 1 — the fault injector.
#
# The copilot is scored against faults injected on purpose, so every incident
# has a known answer. This script injects one fault, records the ground truth
# for that run, then restores the system to the state it found it in.
#
#   ./k8s/fault-inject.sh --fault <name> --runs <N>
#   ./k8s/fault-inject.sh --none --runs <N>      (no-fault runs, for false alarms)
#
# Faults (each maps to one runbook section, see docs/Runbook.md):
#
#   engine_outage     delete the matching-engine pods          worker-crash-loop
#   batchers_stopped  scale batcherd to 0, then back           queue-backlog
#   ingestion_down    scale ingestd to 0, then back            ingestion-down
#   traffic_burst     load-generate against the REST endpoint  high-latency
#   slow_solver       lower the engine CPU limit, then back    high-latency
#   redis_stall       CLIENT PAUSE on redis-0                  redis-stall
#   --none            no fault                                 (NULL)
#
# Every run writes one row to incidents_truth before the fault is injected.
# The copilot has no grant on that table, and this script never prints its
# rows. Stdout carries only the run index and the fault name.
#
# Ground truth needs TRUTH_DSN, a connection string for the injector's own role.
# It is read from the environment and never hard-coded or printed.
#
# Usage: ./k8s/fault-inject.sh --fault <name> --runs <N>
#        ./k8s/fault-inject.sh --none --runs <N>

set -euo pipefail

NS=ride-matching
REST=http://localhost:30081
WS=ws://localhost:30080/v1/drivers/stream
FAULT_HOLD_SECONDS=${FAULT_HOLD_SECONDS:-90}
SETTLE_SECONDS=${SETTLE_SECONDS:-60}
BURST_RATE=${BURST_RATE:-300}
BURST_SECONDS=${BURST_SECONDS:-60}
BURST_DRIVERS=${BURST_DRIVERS:-200}
SLOW_CPU_LIMIT=${SLOW_CPU_LIMIT:-500m}   # must stay >= the 250m request
REDIS_PAUSE_MS=60000

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
bad()  { printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'EOF'
usage:
  k8s/fault-inject.sh --fault <name> --runs <N>
  k8s/fault-inject.sh --none --runs <N>

faults: engine_outage batchers_stopped ingestion_down traffic_burst slow_solver redis_stall
EOF
  exit 2
}

# ---------------------------------------------------------------------------
# Arguments. Parsed before anything touches the cluster, so a typo costs nothing.
FAULT=""
RUNS=""
NONE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --fault) [[ $# -ge 2 ]] || usage; FAULT="$2"; shift 2 ;;
    --runs)  [[ $# -ge 2 ]] || usage; RUNS="$2"; shift 2 ;;
    --none)  NONE=1; shift ;;
    -h|--help) usage ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage ;;
  esac
done

[[ -n "$RUNS" ]] || usage
[[ "$RUNS" =~ ^[1-9][0-9]*$ ]] || die "--runs must be a positive integer, got '$RUNS'"

if [[ "$NONE" -eq 1 && -n "$FAULT" ]]; then
  die "--none and --fault are mutually exclusive"
fi
if [[ "$NONE" -eq 0 && -z "$FAULT" ]]; then
  usage
fi

# Each fault maps to exactly one target and one expected runbook anchor.
# The case statement is the whitelist: nothing outside it reaches psql or kubectl.
case "$FAULT" in
  engine_outage)    TARGET=matching-engine; EXPECTED=worker-crash-loop ;;
  batchers_stopped) TARGET=batcherd;        EXPECTED=queue-backlog ;;
  ingestion_down)   TARGET=ingestd;         EXPECTED=ingestion-down ;;
  traffic_burst)    TARGET=requestd;        EXPECTED=high-latency ;;
  slow_solver)      TARGET=matching-engine; EXPECTED=high-latency ;;
  redis_stall)      TARGET=redis-0;         EXPECTED=redis-stall ;;
  "")               TARGET=none;            EXPECTED=""; FAULT=none ;;
  *) die "unknown fault '$FAULT' (see --help)" ;;
esac

# Ground truth is mandatory. Failing here means nothing was injected.
[[ -n "${TRUTH_DSN:-}" ]] || die "TRUTH_DSN is not set; the injector cannot record ground truth"

# ---------------------------------------------------------------------------
# Restore state. Every change records its ORIGINAL value first, and the EXIT
# trap restores anything still outstanding, so a failed run still restores.
ORIG_BATCHERD_REPLICAS=""
ORIG_INGESTD_REPLICAS=""
ORIG_ENGINE_CPU=""
PENDING_BATCHERD=0
PENDING_INGESTD=0
PENDING_ENGINE_CPU=0
PENDING_REDIS_PAUSE=0
WORK=""

restore_all() {
  local rc=0
  if [[ "$PENDING_BATCHERD" -eq 1 ]]; then
    if kubectl -n "$NS" scale deployment batcherd --replicas="$ORIG_BATCHERD_REPLICAS" >/dev/null \
       && kubectl -n "$NS" rollout status deployment/batcherd --timeout=120s >/dev/null; then
      PENDING_BATCHERD=0
      ok "restored batcherd to $ORIG_BATCHERD_REPLICAS replicas"
    else
      bad "could not restore batcherd to $ORIG_BATCHERD_REPLICAS replicas"; rc=1
    fi
  fi
  if [[ "$PENDING_INGESTD" -eq 1 ]]; then
    if kubectl -n "$NS" scale deployment ingestd --replicas="$ORIG_INGESTD_REPLICAS" >/dev/null \
       && kubectl -n "$NS" rollout status deployment/ingestd --timeout=120s >/dev/null; then
      PENDING_INGESTD=0
      ok "restored ingestd to $ORIG_INGESTD_REPLICAS replicas"
    else
      bad "could not restore ingestd to $ORIG_INGESTD_REPLICAS replicas"; rc=1
    fi
  fi
  if [[ "$PENDING_ENGINE_CPU" -eq 1 ]]; then
    if kubectl -n "$NS" patch deployment matching-engine --type=strategic \
         -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"engine\",\"resources\":{\"limits\":{\"cpu\":\"$ORIG_ENGINE_CPU\"}}}]}}}}" >/dev/null \
       && kubectl -n "$NS" rollout status deployment/matching-engine --timeout=120s >/dev/null; then
      PENDING_ENGINE_CPU=0
      ok "restored matching-engine CPU limit to $ORIG_ENGINE_CPU"
    else
      bad "could not restore matching-engine CPU limit to $ORIG_ENGINE_CPU"; rc=1
    fi
  fi
  if [[ "$PENDING_REDIS_PAUSE" -eq 1 ]]; then
    # The pause expires on its own after REDIS_PAUSE_MS; UNPAUSE just ends it early.
    if kubectl -n "$NS" exec redis-0 -- redis-cli CLIENT UNPAUSE >/dev/null 2>&1; then
      PENDING_REDIS_PAUSE=0
      ok "redis-0 client pause cleared"
    else
      bad "could not clear the redis-0 client pause"; rc=1
    fi
  fi
  return "$rc"
}

cleanup() {
  local rc=$?
  trap - EXIT
  if ! restore_all; then
    rc=1
  fi
  [[ -n "$WORK" ]] && rm -rf "$WORK"
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Single-quote a value for SQL. Every value here comes from the whitelist above
# or from this script's own clock, but it is quoted anyway so no future edit
# can turn this into an injection point.
sql_quote() { local v=${1//\'/\'\'}; printf "'%s'" "$v"; }

# Write one ground-truth row. Called BEFORE the fault, so an unrecorded fault
# can never happen.
record_truth() {
  local run_id="$1" start="$2"
  local expected_sql="NULL"
  [[ -n "$EXPECTED" ]] && expected_sql=$(sql_quote "$EXPECTED")
  psql "$TRUTH_DSN" -v ON_ERROR_STOP=1 -q -c \
    "INSERT INTO incidents_truth (run_id, fault, target, start_time, expected_runbook) VALUES ($(sql_quote "$run_id"), $(sql_quote "$FAULT"), $(sql_quote "$TARGET"), $(sql_quote "$start")::timestamptz, $expected_sql);" \
    >/dev/null
}

# ---------------------------------------------------------------------------
# Inject one fault. Records the original state of anything it changes.
inject() {
  case "$FAULT" in
    none)
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    engine_outage)
      kubectl -n "$NS" delete pods -l app.kubernetes.io/name=matching-engine --wait=false >/dev/null
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    batchers_stopped)
      ORIG_BATCHERD_REPLICAS=$(kubectl -n "$NS" get deployment batcherd -o jsonpath='{.spec.replicas}')
      [[ -n "$ORIG_BATCHERD_REPLICAS" ]] || die "could not read batcherd replica count"
      PENDING_BATCHERD=1
      kubectl -n "$NS" scale deployment batcherd --replicas=0 >/dev/null
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    ingestion_down)
      ORIG_INGESTD_REPLICAS=$(kubectl -n "$NS" get deployment ingestd -o jsonpath='{.spec.replicas}')
      [[ -n "$ORIG_INGESTD_REPLICAS" ]] || die "could not read ingestd replica count"
      PENDING_INGESTD=1
      kubectl -n "$NS" scale deployment ingestd --replicas=0 >/dev/null
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    slow_solver)
      ORIG_ENGINE_CPU=$(kubectl -n "$NS" get deployment matching-engine \
        -o jsonpath='{.spec.template.spec.containers[?(@.name=="engine")].resources.limits.cpu}')
      [[ -n "$ORIG_ENGINE_CPU" ]] || die "matching-engine has no CPU limit to restore; refusing to touch it"
      PENDING_ENGINE_CPU=1
      kubectl -n "$NS" patch deployment matching-engine --type=strategic \
        -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"engine\",\"resources\":{\"limits\":{\"cpu\":\"$SLOW_CPU_LIMIT\"}}}]}}}}" >/dev/null
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    redis_stall)
      PENDING_REDIS_PAUSE=1
      kubectl -n "$NS" exec redis-0 -- redis-cli CLIENT PAUSE "$REDIS_PAUSE_MS" >/dev/null
      sleep "$FAULT_HOLD_SECONDS"
      ;;
    traffic_burst)
      # The load generator blocks for its own duration (riders / rate), so no
      # separate hold is needed. Its output goes to a file, not stdout.
      command -v go >/dev/null 2>&1 || die "go is required to build cmd/loadtest for traffic_burst"
      WORK=$(mktemp -d)
      (cd "$ROOT/infrastructure" && go build -o "$WORK/loadtest" ./cmd/loadtest)
      local riders=$(( BURST_RATE * BURST_SECONDS ))
      "$WORK/loadtest" --mode=pipeline --ws "$WS" --rest "$REST" \
        --drivers "$BURST_DRIVERS" --riders "$riders" --rate "$BURST_RATE" \
        > "$WORK/loadtest.log" 2>&1
      ;;
  esac
}

# ---------------------------------------------------------------------------
kubectl -n "$NS" get deployment >/dev/null 2>&1 \
  || die "cannot reach the ride-matching namespace; is the stack deployed? (./k8s/deploy.sh)"

log "fault=$FAULT target=$TARGET runs=$RUNS"
for i in $(seq 1 "$RUNS"); do
  RUN_INDEX=$(printf '%03d' "$i")
  RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$RUN_INDEX"
  START=$(date -u +%Y-%m-%dT%H:%M:%SZ)

  record_truth "$RUN_ID" "$START" || die "could not write incidents_truth; no fault injected"
  inject
  restore_all || die "restore failed on run $RUN_INDEX; the EXIT trap will retry"

  printf 'run %s/%s fault=%s ok\n' "$i" "$RUNS" "$FAULT"

  # Let the system settle to baseline before the next run, so runs do not bleed
  # into each other. Skipped after the last run.
  if [[ "$i" -lt "$RUNS" ]]; then
    sleep "$SETTLE_SECONDS"
  fi
done
exit 0
