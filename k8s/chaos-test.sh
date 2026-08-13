#!/usr/bin/env bash
# WEEK 16 CHECKPOINT — the chaos test.
#
#   "Kill the C++ pod under active load and demonstrate ZERO dropped ride
#    requests."
#
# This is the test the whole project has been building toward. Every earlier
# decision exists to make it pass:
#
#   ADR-0002  the engine is a separate process, so its death is an error value
#             rather than a crashed Go service
#   ADR-0006  ride requests live in a durable Redis Stream, not in memory
#   Week 12   the batcher acks a request only AFTER it is matched, so a batch
#             in flight when the engine dies is never acked
#   Week 10   un-acked messages are redelivered (XPENDING/XCLAIM)
#   Week 16   Kubernetes reschedules the dead pod automatically
#
# The accounting is taken from REDIS, not from a service's own counters,
# because /stats is served by whichever pod the NodePort load-balances to and
# each batcher only knows its own totals. Redis is the single authority on what
# was published and what remains outstanding.
#
# Usage: ./k8s/chaos-test.sh [num_requests] [num_drivers]

set -euo pipefail

NS=ride-matching
REQUESTS=${1:-300}
DRIVERS=${2:-80}
REST=http://localhost:30081
WS=ws://localhost:30080/v1/drivers/stream
STREAM=requests:stream:default
DEAD=requests:dead:default
GROUP=batchers

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
bad()  { printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

kubectl -n "$NS" get deployment matching-engine >/dev/null 2>&1 \
  || die "stack not deployed — run ./k8s/deploy.sh first"

redis() { kubectl -n "$NS" exec redis-0 -- redis-cli "$@" 2>/dev/null | tr -d '\r'; }

stream_len()  { redis XLEN "$STREAM" || echo 0; }

# Distinct ride requests, counted by their idempotency key.
#
# XLEN is NOT the number of requests: an unmatched rider is REPUBLISHED for a
# later window, which appends another entry carrying the same request_id. The
# first version of this test compared XLEN against the accepted count and
# reported 269 vs 200 as "requests lost at the front door", when in fact 69
# riders had simply needed a second window. Counting distinct ids is what the
# zero-loss claim actually requires.
distinct_requests() {
  redis XRANGE "$STREAM" - + 2>/dev/null | grep -oE 'req_[0-9a-f]+' | sort -u | wc -l | tr -d ' '
}
dead_len()    { redis XLEN "$DEAD" || echo 0; }
pending_len() { redis XPENDING "$STREAM" "$GROUP" 2>/dev/null | head -1 || echo 0; }
group_lag()   { redis XINFO GROUPS "$STREAM" 2>/dev/null | grep -A1 -w lag | tail -1 || echo 0; }

# ---------------------------------------------------------------------------
# Start from a clean queue so the accounting is unambiguous. Leftovers from a
# previous run would sit pending forever and look like requests this run lost.
#
# The batchers are restarted afterwards because deleting the stream also deletes
# the consumer group; without a restart their XREADGROUP calls fail NOGROUP
# until something recreates it, and the group is created at process startup.
log "0. resetting the queue for a clean baseline"
redis DEL "$STREAM" "$DEAD" >/dev/null || true
kubectl -n "$NS" rollout restart deployment/batcherd >/dev/null
kubectl -n "$NS" rollout status deployment/batcherd --timeout=120s >/dev/null
BASE_DEAD=$(dead_len)
printf '    stream=%s dead-letter=%s (clean)\n' "$(stream_len)" "$BASE_DEAD"

# ---------------------------------------------------------------------------
log "1. starting $DRIVERS drivers"
(cd "$ROOT/infrastructure" && go build -o /tmp/chaos-mockdrivers ./cmd/mockdrivers)
/tmp/chaos-mockdrivers --url "$WS" --drivers "$DRIVERS" --interval 1s --duration 180s \
  > /tmp/chaos-drivers.log 2>&1 &
DRIVERS_PID=$!
trap 'kill $DRIVERS_PID 2>/dev/null || true' EXIT
sleep 8
INDEXED=$(redis ZCARD "drivers:geo:default")
printf '    %s drivers indexed in Redis\n' "$INDEXED"
[[ "$INDEXED" -gt 0 ]] || die "no drivers reached Redis; is ingestd healthy?"

# ---------------------------------------------------------------------------
# Submit continuously in the background so requests are genuinely IN FLIGHT
# when the engine dies. Killing a pod on an idle system proves nothing.
log "2. submitting $REQUESTS ride requests (in the background)"
(
  accepted=0
  for i in $(seq 1 "$REQUESTS"); do
    lat=$(awk -v i="$i" 'BEGIN{printf "%.6f", 12.9716 + (i%40)*0.0008}')
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$REST/v1/ride-requests" \
      -H 'Content-Type: application/json' \
      -d "{\"rider_id\":\"R-chaos-$i\",\"pickup\":{\"lat\":$lat,\"lng\":77.5946}}" || echo 000)
    [[ "$code" == "202" ]] && accepted=$((accepted+1))
    sleep 0.05
  done
  echo "$accepted" > /tmp/chaos-accepted
) &
SUBMIT_PID=$!

# ---------------------------------------------------------------------------
# THE CHAOS. Delete every engine pod at once, so there is a real outage window
# rather than a Service quietly routing around one surviving replica.
sleep 5
log "3. CHAOS — deleting ALL matching-engine pods mid-flight"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=matching-engine \
  -o custom-columns=NAME:.metadata.name --no-headers | sed 's/^/    killing /'
kubectl -n "$NS" delete pods -l app.kubernetes.io/name=matching-engine --wait=false >/dev/null
KILL_AT=$(date +%s)

# Watch the recovery rather than assuming it.
log "4. watching Kubernetes reschedule"
for i in $(seq 1 60); do
  READY=$(kubectl -n "$NS" get deployment matching-engine \
    -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
  READY=${READY:-0}
  [[ "$READY" -ge 1 ]] && break
  sleep 2
done
RECOVERED_AT=$(date +%s)
printf '    engine ready again after %ss (readyReplicas=%s)\n' "$((RECOVERED_AT-KILL_AT))" "$READY"

wait $SUBMIT_PID
ACCEPTED=$(cat /tmp/chaos-accepted 2>/dev/null || echo 0)
log "5. submission finished: $ACCEPTED / $REQUESTS accepted (HTTP 202)"

# ---------------------------------------------------------------------------
# Drain. Requests unacked when the engine died are redelivered, but only after
# the batcher's reclaim interval — recovery is not instantaneous and the test
# must not pretend otherwise.
log "6. draining (up to 150s for reclaim + rematch)"
for i in $(seq 1 50); do
  PEND=$(pending_len); LAG=$(group_lag)
  PEND=${PEND:-0}; LAG=${LAG:-0}
  printf '\r    pending=%-6s undelivered=%-6s' "$PEND" "$LAG"
  [[ "$PEND" == "0" && "$LAG" == "0" ]] && break
  sleep 3
done
echo

# ---------------------------------------------------------------------------
log "7. accounting (from Redis, the only authority)"
DISTINCT=$(distinct_requests)
ENTRIES=$(stream_len)
REPUBLISHES=$(( ENTRIES - DISTINCT ))
FINAL_PENDING=$(pending_len);  FINAL_PENDING=${FINAL_PENDING:-0}
FINAL_LAG=$(group_lag);        FINAL_LAG=${FINAL_LAG:-0}
FINAL_DEAD=$(( $(dead_len) - BASE_DEAD ))

cat <<EOF

    submitted & accepted (HTTP 202)  : $ACCEPTED
    DISTINCT requests in the stream  : $DISTINCT
    stream entries (incl. retries)   : $ENTRIES
    republished for a later window   : $REPUBLISHES
    still pending (claimed, unacked) : $FINAL_PENDING
    still undelivered (lag)          : $FINAL_LAG
    dead-lettered                    : $FINAL_DEAD

EOF

FAILED=0

# The core claim: every request the API accepted reached the durable stream.
# Anything lost here was lost BEFORE durability, which no amount of
# redelivery can fix.
if [[ "$DISTINCT" -eq "$ACCEPTED" ]]; then
  ok "every accepted request reached the durable queue ($ACCEPTED = $DISTINCT distinct)"
else
  bad "distinct requests ($DISTINCT) != accepted ($ACCEPTED) — lost at the front door"
  FAILED=1
fi

# Nothing was discarded as poison. An engine crash is RETRYABLE (Week 6), so
# dead-lettering any of it would mean the taxonomy misclassified a transient
# failure as permanent.
if [[ "$FINAL_DEAD" -eq 0 ]]; then
  ok "nothing dead-lettered — an engine crash was correctly treated as retryable"
else
  bad "$FINAL_DEAD requests dead-lettered; a crash must not be permanent failure"
  FAILED=1
fi

# Everything drained, which means everything was matched and acked.
if [[ "$FINAL_PENDING" -eq 0 && "$FINAL_LAG" -eq 0 ]]; then
  ok "queue fully drained — every request was resolved, none left in limbo"
else
  bad "queue did not drain (pending=$FINAL_PENDING lag=$FINAL_LAG)"
  FAILED=1
fi

# The headline claim. Everything drained, nothing was dead-lettered, and the
# distinct count matches what the API accepted — so all $ACCEPTED requests were
# matched, including those in flight when the engine was destroyed.
if [[ "$DISTINCT" -eq "$ACCEPTED" && "$FINAL_PENDING" -eq 0 \
      && "$FINAL_LAG" -eq 0 && "$FINAL_DEAD" -eq 0 && "$ACCEPTED" -gt 0 ]]; then
  ok "ZERO DROPPED REQUESTS across a full engine outage"
  [[ "$REPUBLISHES" -gt 0 ]] && \
    printf '       (%s needed more than one window — retried, not lost)\n' "$REPUBLISHES"
else
  bad "the zero-loss claim does not hold"
  FAILED=1
fi

echo
kubectl -n "$NS" get pods -l app.kubernetes.io/name=matching-engine
echo
if [[ "$FAILED" -eq 0 ]]; then
  printf '\033[1;32m==> CHECKPOINT MET: pod deleted mid-traffic, automatic recovery, no lost requests\033[0m\n'
else
  printf '\033[1;31m==> CHECKPOINT FAILED\033[0m\n'
fi
exit "$FAILED"
