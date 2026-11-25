#!/usr/bin/env bash
# Deploy the copilot to Cloud Run. Prints the command by default; runs it only with --run.
#   deploy/gcp/deploy.sh          show what would run
#   deploy/gcp/deploy.sh --run    build and deploy
# Needs: gcloud logged in, GCP_PROJECT set, a billing-enabled project, deploy/gcp/env.cloudrun.yaml.
# Never prints the contents of the env file.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
REGION="${REGION:-us-central1}"      # free-tier regions are in the US; check Cloud Run pricing
SERVICE="${SERVICE:-copilot}"
ENV_FILE="${ENV_FILE:-$HERE/env.cloudrun.yaml}"

[ -n "${GCP_PROJECT:-}" ] || { echo "GCP_PROJECT is not set" >&2; exit 2; }
[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE (copy env.cloudrun.example and fill it)" >&2; exit 2; }
if grep -q "REPLACE_ME\|REPLACE_HOST\|REPLACE_DB" "$ENV_FILE"; then
  echo "$ENV_FILE still has REPLACE_ME placeholders" >&2; exit 2
fi

CMD=(gcloud run deploy "$SERVICE"
  --project "$GCP_PROJECT" --region "$REGION"
  --source "$ROOT/copilot" --port 8000
  --min-instances 0 --max-instances 1 --cpu 1 --memory 2Gi
  --allow-unauthenticated
  --env-vars-file "$ENV_FILE")

if [ "${1:-}" = "--run" ]; then
  exec "${CMD[@]}"
fi
echo "dry run (add --run to execute):"
printf ' %q' "${CMD[@]}"; echo
