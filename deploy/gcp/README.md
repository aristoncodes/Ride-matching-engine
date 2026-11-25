# Deploy the copilot to Google Cloud Run

Status: **ready, not deployed.** Nothing in this folder has been run against a real GCP
project. Cloud Run needs a billing account on the project (even for the free tier), so the
deploy was deferred. Every command below is the exact command to run later.

What deploys: the `copilot/` service (FastAPI, ADK-ready agents, local embeddings). Its
database is Postgres with pgvector, hosted on Neon. The Ride-Matching engine does **not**
deploy here: it stays on the local kind cluster.

## Before you start

| Need | How |
|---|---|
| GCP project with billing | console.cloud.google.com, create a project, enable billing |
| `gcloud` | `brew install --cask google-cloud-sdk`, then `gcloud auth login` |
| Budget alert (do this first) | Console, Billing, Budgets and alerts, a small budget with 50% and 100% emails. It only notifies, it does not cap spend. |
| Neon database | neon.com, new project, copy the **direct** (non-pooled) connection string |
| Gemini API key | aistudio.google.com, Get API key (free tier, no billing) |
| `psql` 15+ | for the one-time role setup (`brew install libpq`) |

Cost guardrails used below: region `us-central1` (the free tier applies in a few US regions),
`--min-instances 0`, `--max-instances 1`. Check the current free allowance on
cloud.google.com/run/pricing before relying on it.

## 1. One-time project setup

```bash
export GCP_PROJECT=<your-project-id>
export REGION=us-central1
gcloud config set project "$GCP_PROJECT"
gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com
```

## 2. Database on Neon

Create a Neon project, then use its **owner** connection string once, from your machine:

```bash
export COPILOT_OWNER_DSN='postgresql://<owner>:<pw>@<host>/<db>?sslmode=require'

# Create the least-privilege roles. Passwords come from the environment, never from the file.
export COPILOT_APP_PASSWORD=<random> COPILOT_RO_PASSWORD=<random> FAULT_INJECTOR_PASSWORD=<random>
psql "$COPILOT_OWNER_DSN" -f copilot/initdb/01-roles.sql

# Apply the schema and grants, then index the docs (needs the local embedding model, ~100 MB).
cd copilot && uv run python -m copilot.bootstrap
```

`bootstrap` prints counts only. It is safe to re-run.

Then build the two runtime DSNs (same host and database, the new roles):

```
COPILOT_DB_DSN = postgresql://copilot_app:<COPILOT_APP_PASSWORD>@<host>/<db>?sslmode=require
COPILOT_RO_DSN = postgresql://copilot_ro:<COPILOT_RO_PASSWORD>@<host>/<db>?sslmode=require
```

To check on first run: Neon's pooled endpoint (pgbouncer) can break asyncpg's prepared
statements, so use the direct endpoint. The `vector` extension and `CREATE ROLE` should work
for the owner role on Neon, but this has not been tried.

## 3. Environment file

```bash
cp deploy/gcp/env.cloudrun.example deploy/gcp/env.cloudrun.yaml   # gitignored
# edit it: fill every REPLACE_ME. Tokens must all be different from each other.
```

Generate the tokens with `python3 -c "import secrets; print(secrets.token_urlsafe(32))"`.

## 4. Deploy

`deploy.sh` prints the command by default and only runs it with `--run`:

```bash
deploy/gcp/deploy.sh          # shows what would run
deploy/gcp/deploy.sh --run    # builds from copilot/Dockerfile and deploys
```

It runs, in effect:

```bash
gcloud run deploy copilot \
  --source copilot --region us-central1 --port 8000 \
  --min-instances 0 --max-instances 1 --cpu 1 --memory 2Gi \
  --allow-unauthenticated \
  --env-vars-file deploy/gcp/env.cloudrun.yaml
```

`--allow-unauthenticated` only opens the URL: every route except `/healthz` still requires the
bearer token. `--memory 2Gi` is a first guess (the embedding model loads at startup); lower it
if the metrics show headroom, raise it if the service is killed for memory.

## 5. Check it

```bash
URL=$(gcloud run services describe copilot --region us-central1 --format 'value(status.url)')
curl -s "$URL/healthz"
curl -s -H "Authorization: Bearer $COPILOT_API_TOKEN" "$URL/incidents"
```

## What will not work yet

- **Live metrics.** The copilot reads Prometheus at `PROMETHEUS_URL`. Cloud Run cannot reach a
  Prometheus on your laptop. The plan is to replay a recorded fault window (see
  `docs/copilot/FREE_TIER_PLAN.md`, section 3). **That replay is not built.** Until it is,
  `/healthz` and the database routes work, but a diagnosis will find no metrics.
- **Kubernetes actions.** Cloud Run has no cluster, so `COPILOT_K8S_ACTIONS=off`. Proposals and
  approvals still work; executing an approved action fails with a fixed message.
- **Logs.** The `logs` table on Neon starts empty. Load a recorded window into it
  (`copilot/copilot/ingest/logs.py`).
- **Model cache.** Without `REDIS_URL`, cache lookups fail open and cost nothing but a log line.

## Roll back or remove

```bash
gcloud run services delete copilot --region us-central1      # stops all Cloud Run cost
```

Neon projects can be deleted from the Neon console.
