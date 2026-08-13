#!/usr/bin/env bash
# Bring up a local Kubernetes cluster running the whole stack.
#
#   ./k8s/deploy.sh              create cluster (if absent), build, load, apply
#   ./k8s/deploy.sh --rebuild    force a rebuild of all images first
#   ./k8s/deploy.sh --destroy    delete the cluster
#
# Everything here is idempotent, because a deploy script that only works on a
# clean machine is a script you cannot use to recover.

set -euo pipefail

CLUSTER=ride-matching
NS=ride-matching
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGES=(ride-matching/engine:dev ride-matching/ingestd:dev ride-matching/requestd:dev ride-matching/batcherd:dev)

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }
need docker; need kind; need kubectl
docker info >/dev/null 2>&1 || die "the Docker daemon is not running (try: open -a Docker)"

if [[ "${1:-}" == "--destroy" ]]; then
  log "deleting cluster $CLUSTER"
  kind delete cluster --name "$CLUSTER"
  exit 0
fi

# ---- 1. Cluster --------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "cluster $CLUSTER already exists"
else
  log "creating cluster $CLUSTER (control-plane + 2 workers)"
  kind create cluster --config "$ROOT/k8s/kind-cluster.yaml" --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# ---- 2. Images ---------------------------------------------------------------
if [[ "${1:-}" == "--rebuild" ]] || ! docker image inspect "${IMAGES[0]}" >/dev/null 2>&1; then
  log "building images"
  (cd "$ROOT" && docker compose build)
fi

# kind nodes have their own image store; a locally-built image is invisible to
# them until it is loaded. This is why every manifest sets
# imagePullPolicy: Never — without the load AND the policy, kubelet goes looking
# on Docker Hub for an image that was never pushed anywhere.
log "loading images into the cluster"
for img in "${IMAGES[@]}"; do
  kind load docker-image "$img" --name "$CLUSTER" >/dev/null
  printf '    %s\n' "$img"
done

# ---- 3. metrics-server (for the HPA) -----------------------------------------
if ! kubectl get deployment metrics-server -n kube-system >/dev/null 2>&1; then
  log "installing metrics-server (required by the HPA)"
  kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.7.2/components.yaml

  # kind's kubelet serves a self-signed certificate, which metrics-server
  # rejects by default. Without this patch it CrashLoopBackOffs and every HPA
  # reports <unknown> forever. Acceptable for a local cluster only — in a real
  # one you fix the certificates instead of skipping verification.
  kubectl patch deployment metrics-server -n kube-system --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
fi

# ---- 4. Apply ----------------------------------------------------------------
# The namespace goes FIRST, explicitly. `kubectl apply -f <dir>` processes files
# in ALPHABETICAL order, so go-services.yaml, hpa.yaml and matching-engine.yaml
# are all attempted before namespace.yaml and fail with "namespaces not found".
# (redis.yaml happened to work, since r > n — which is the kind of accident that
# makes this bug look intermittent.)
log "applying manifests"
kubectl apply -f "$ROOT/k8s/base/namespace.yaml"
kubectl apply -f "$ROOT/k8s/base/"

# ---- 5. Wait -----------------------------------------------------------------
# Redis first and on its own: everything else depends on it, and a Go service
# that starts before Redis is ready fails its first writes. This is the
# `depends_on: service_healthy` idea from Week 14, expressed as ordering in the
# deploy rather than as a field — Kubernetes has no built-in equivalent.
log "waiting for redis"
kubectl -n "$NS" rollout status statefulset/redis --timeout=120s

log "waiting for the matching engine"
kubectl -n "$NS" rollout status deployment/matching-engine --timeout=180s

log "waiting for the Go services"
for d in ingestd requestd batcherd; do
  kubectl -n "$NS" rollout status "deployment/$d" --timeout=120s
done

echo
log "cluster is up"
kubectl -n "$NS" get pods -o wide
echo
cat <<EOF
Endpoints (forwarded to the host by kind):
  ingestd   ws://localhost:30080/v1/drivers/stream
  requestd  http://localhost:30081/v1/ride-requests
  batcherd  http://localhost:30082/stats

Try:
  curl -s localhost:30081/healthz
  curl -s localhost:30082/stats
  ./k8s/chaos-test.sh          # the Week 16 checkpoint
EOF
