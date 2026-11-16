# Incident Copilot — Plan (Phase 0)

Status: **draft, awaiting approval.** Nothing in `copilot/` is built yet.
Source of truth for scope: `docs/copilot/PRD.md`. Rules: `CLAUDE.md`.

Written for: the project owner, to approve or redirect before any code is written.

## 0. Facts from the repo that change the plan

Found by reading the repo, not the PRD. Each one needs a decision or a fix.

1. **No `pantry-scout` agent exists in this session.** I used the built-in `Explore` agent for the reads. Same job, read-only.
2. **The PRD's dropped-requests metric does not exist.** The runbook's `dropped-requests` symptom is `ride_requests_dropped_total > 0`. The real metrics are `ridematch_*` (15 of them, `infrastructure/internal/metrics/metrics.go:63-151`), and none is a dropped counter. Closest proxy: `ridematch_queue_dead_lettered`. **Decision needed:** map `dropped-requests` to the dead-letter rule, or add a counter in the engine (allowed by CLAUDE.md as an engine-side change).
3. **The alert rules have no severity and no runbook link.** `docs/Observability.md:55-86` has 8 PromQL rules, with comments as names. The file has no links to `Runbook.md`. Ticket 1.2 will add `severity` and `runbook_anchor` labels when it writes the Prometheus rules, so the webhook payload carries the anchor.
4. **Anchor name mismatch.** The runbook anchor is `worker-crash-loop`. The PRD's fault table says `worker-crashloop`. This plan uses the runbook's `worker-crash-loop`. The PRD should be corrected to match.
5. **Redis stall has no runbook section.** The PRD expects "Redis dependency errors", but `docs/Runbook.md` has no Redis section (six incident anchors only). **Decision needed:** add a `redis-stall` section to `docs/Runbook.md` (a doc change, so it needs your OK), or score that fault against the `dependency_errors` alert alone.
6. **Runbook and SLO thresholds disagree.** Runbook `high-latency` fires at end-to-end p99 > 3.5 s. `SLOMatchLatencyP99` is 5 s (`metrics.go:48`). The PRD's detector and eval use the runbook number. **Decision needed:** which one is the paging threshold. Plan assumes the runbook's 3.5 s for diagnosis, the SLO for alerts.
7. **Traffic burst and slow solver both map to `high-latency`.** This matches the PRD risk table. Plan separates them with metrics: batch size (`ridematch_batch_size_riders`) vs solve time (`ridematch_solve_seconds`) per rider.
8. **Admin ports differ per service.** `ingestd` serves `/metrics` on `:6060`, `requestd` on `:6061`, `batcherd` on `:6062` (flag `-admin-addr` in each `main.go`; `ingestd/main.go:53-54`, `requestd/main.go:48`, `batcherd/main.go:53`). CLAUDE.md and PRD say 6060 for all three. Ticket 1.2 scrapes the real ports. **Action:** correct the CLAUDE.md "Facts" line. I have not edited CLAUDE.md, since it is your rules file.
9. **Logger is text, not JSON, in all three services** (`ingestd/main.go:59-60`, `requestd/main.go:54-55`, `batcherd/main.go:59-60`). Matches ticket 1.1. No env var controls the log level.
10. **Chaos script and deploy script are self-contained.** No env vars. `k8s/chaos-test.sh` takes `[num_requests=300] [num_drivers=80]` and exits nonzero on failure. `k8s/deploy.sh` uses kind cluster `ride-matching`, namespace `ride-matching`, and supports `--rebuild` and `--destroy`. Ticket 1.3 copies the chaos script's structure. Ticket 1.2 adds Prometheus to `k8s/base/`, which `deploy.sh` applies with `kubectl apply -f k8s/base/`.
11. **Repo state.** `main` has uncommitted changes and no `copilot/`, `docs/copilot/` (before this step), or `.claude/agents/` directory. CLAUDE.md asks for branch `feat/incident-copilot`. I will create it at the start of Phase 1, with the existing uncommitted files carried over untouched.

## 1. Package layout

```
copilot/
  pyproject.toml            # uv, Python 3.12, ruff, pytest
  Dockerfile
  .env.example              # names only, no values
  compose.yaml              # Postgres+pgvector, Prometheus for local dev
  copilot/
    __init__.py
    config.py               # env-var settings (pydantic-settings)
    models.py               # Diagnosis, Evidence, Claim, ProposedAction (contracts, §2)
    db.py                   # asyncpg/SQLAlchemy engine, schema SQL
    schema.sql              # §2.1
    ingest/logs.py          # DP-1
    ingest/docs.py          # DP-2, DP-3 (P1)
    embed.py                # Embedder protocol + Vertex impl + FakeEmbedder for tests
    detect/zscore.py        # AD-1, AD-3 events
    detect/iforest.py       # AD-2 (P1)
    agents/
      prompts/              # head chef writes these (§5)
      tools/metrics_tools.py    # AG-2
      tools/logs_tools.py       # AG-3
      tools/runbook_tools.py    # AG-4
      coordinator.py            # AG-1
      critic.py                 # AG-5
      model.py                  # 2.7 wrapper: timeout, retry, cache, breaker, fallback
    actions/k8s.py          # AC-1..AC-3
    api/app.py              # FastAPI routes (§2.4)
    obs/                    # OTel tracing, Prometheus metrics (3.2)
    eval/run.py             # 3.1
    eval/RESULTS.md
  tests/
```

Engine-side (infra cooks): `k8s/base/prometheus.yaml`, `k8s/base/copilot.yaml`, `k8s/base/copilot-rbac.yaml`, `k8s/fault-inject.sh`, the three `infrastructure/cmd/*/main.go` logger switches.

## 2. Contracts (head chef owns these)

### 2.1 Postgres schema (`copilot/schema.sql`)

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE logs (
  id          bigserial PRIMARY KEY,
  ts          timestamptz NOT NULL,
  service     text NOT NULL,            -- ingestd | requestd | batcherd
  level       text NOT NULL,
  request_id  text,
  event       text,
  error       text,
  fields      jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX logs_ts_idx ON logs (ts);
CREATE INDEX logs_request_id_idx ON logs (request_id);

CREATE TABLE doc_chunks (
  id           bigserial PRIMARY KEY,
  source_file  text NOT NULL,           -- e.g. docs/Runbook.md
  anchor       text,                    -- e.g. worker-crash-loop; NULL if none
  heading      text NOT NULL,
  content      text NOT NULL,
  content_hash text NOT NULL,           -- file hash, for DP-3
  embedding    vector(768) NOT NULL     -- text-embedding-005 dims (assumption, §6)
);
CREATE INDEX doc_chunks_embedding_idx ON doc_chunks USING hnsw (embedding vector_cosine_ops);

CREATE TABLE incidents_seen (          -- DP-4
  id           uuid PRIMARY KEY,
  opened_at    timestamptz NOT NULL,
  source       text NOT NULL,           -- alert | detector | question
  trigger      jsonb NOT NULL,          -- raw alert or detection event
  diagnosis    jsonb,                   -- Diagnosis, or NULL if fallback only
  fallback     jsonb,                   -- raw evidence + top runbook matches
  status       text NOT NULL,           -- running | diagnosed | needs_human | failed
  verdict      text,                    -- correct | partly | wrong, set by human
  verdict_by   text,
  verdict_at   timestamptz,
  tokens_in    integer NOT NULL DEFAULT 0,
  tokens_out   integer NOT NULL DEFAULT 0,
  cost_usd     numeric(10,6) NOT NULL DEFAULT 0,
  latency_ms   integer
);

CREATE TABLE actions (                 -- AC-3
  id           uuid PRIMARY KEY,
  incident_id  uuid NOT NULL REFERENCES incidents_seen(id),
  kind         text NOT NULL CHECK (kind IN ('restart_pods','scale')),
  deployment   text NOT NULL CHECK (deployment IN ('matching-engine','batcherd')),
  replicas     integer,                 -- scale only; bounded by §2.3 limits
  reason       text NOT NULL,
  status       text NOT NULL CHECK (status IN ('proposed','approved','rejected','executed','failed')),
  proposed_at  timestamptz NOT NULL,
  decided_by   text,
  decided_at   timestamptz,
  executed_at  timestamptz,
  result       text
);

-- Ground truth. Written only by the fault injector; the copilot's DB role has no grant on it.
CREATE TABLE incidents_truth (
  run_id           text NOT NULL,
  fault            text NOT NULL,
  target           text NOT NULL,
  start_time       timestamptz NOT NULL,
  expected_runbook text,                -- anchor, or NULL for no-fault runs
  PRIMARY KEY (run_id, fault)
);
```

### 2.2 Diagnosis model (PRD AG-6, `copilot/models.py`)

```python
from typing import Literal
from pydantic import BaseModel, Field

class Evidence(BaseModel):
    id: str                                   # "E1", "E2", ... referenced by claims
    kind: Literal["metric", "log", "doc"]
    ref: str                                  # PromQL expr, log row id, or "docs/Runbook.md#anchor"
    excerpt: str = Field(max_length=500)

class Claim(BaseModel):
    text: str
    evidence_ids: list[str] = Field(min_length=1)   # empty list = unsupported, critic rejects

class ProposedAction(BaseModel):
    kind: Literal["restart_pods", "scale", "none"]
    deployment: Literal["matching-engine", "batcherd", "none"]
    replicas: int | None = None
    reason: str

class Diagnosis(BaseModel):
    summary: str
    root_cause: str
    component: Literal["matching-engine", "batcherd", "ingestd", "requestd", "redis", "unknown"]
    runbook_anchor: str | None                # must exist in doc_chunks, or None
    confidence: float = Field(ge=0.0, le=1.0)
    needs_human: bool                         # True when confidence < CONFIDENCE_FLOOR (0.6)
    claims: list[Claim]
    evidence: list[Evidence]
    proposed_action: ProposedAction
```

Validation rule, enforced in code: `needs_human = True` if `confidence < 0.6`, and `proposed_action.kind` must be `none` when `needs_human` is true.

### 2.3 Agent tool signatures (AG-2, AG-3, AG-4)

```python
# metrics_tools.py — fixed query names only, no raw PromQL from the model (AG-2)
MetricQuery = Literal[
    "queue_depth", "queue_pending", "dead_lettered", "solve_p99",
    "match_latency_p99", "match_rate", "http_5xx_ratio", "engine_dependency_errors",
    "drivers_indexed", "batch_size_mean", "solve_seconds_per_rider",
]
def query_metric(name: MetricQuery, window_minutes: int = 15) -> list[dict]: ...  # [{ts, value, labels}]

# logs_tools.py — named, parameterised SQL only, read-only role (AG-3)
LogQuery = Literal["errors_by_service", "trace_request", "recent_events_by_service"]
def query_logs(name: LogQuery, params: dict[str, str | int]) -> list[dict]: ...

# runbook_tools.py (AG-4)
def search_runbook(query: str, k: int = 5) -> list[dict]:
    """Returns [{source_file, anchor, heading, content, score}]."""

# critic.py (AG-5)
class CriticVerdict(BaseModel):
    ok: bool
    unsupported_claim_indexes: list[int]
    feedback: str
def review(diagnosis: Diagnosis) -> CriticVerdict: ...
```

Limits for AC-1 (`actions/k8s.py`, head chef decision): `matching-engine` replicas in [1, 4], `batcherd` replicas in [1, 3], one `restart_pods` per deployment per 10 min.

### 2.4 API models (`copilot/api/app.py`)

```python
class DiagnoseRequest(BaseModel):
    source: Literal["alert", "detector", "question"]
    alert: dict | None = None                 # Alertmanager alert object
    question: str | None = None

class DiagnoseResponse(BaseModel):
    incident_id: str
    status: Literal["running", "diagnosed", "needs_human", "failed"]
    diagnosis: Diagnosis | None
    fallback: dict | None                     # raw evidence + top runbook matches (2.7)

class ActionDecision(BaseModel):
    decided_by: str                           # required; from the auth token subject
    note: str | None = None

class ActionResponse(BaseModel):
    action_id: str
    status: Literal["proposed", "approved", "rejected", "executed", "failed"]
    result: str | None
```

Endpoints: `POST /diagnose`, `POST /alerts` (Alertmanager v4 webhook body, mapped to `/diagnose`), `GET /incidents/{id}`, `POST /actions/{id}/approve`, `POST /actions/{id}/reject`. `POST /incidents/{id}/ask` is P1. Auth: bearer token from Secret Manager (or env on kind). Rate limit: 30 req/min per token.

## 3. Phase 1 tickets: signals and data (PRD week 1)

| ID | Cook | Files | Acceptance | Depends on |
|---|---|---|---|---|
| 1.1 | sous-chef-infra | `infrastructure/cmd/{ingestd,requestd,batcherd}/main.go` | `NewJSONHandler` in all three, `request_id` kept, comment says why. `cd infrastructure && make test-go` (or `go test ./...`) and `golangci-lint run` pass | — |
| 1.2 | sous-chef-infra | `k8s/base/prometheus.yaml` (new), `k8s/deploy.sh` | Prometheus scrapes admin ports of the 3 services (`ingestd:6060`, `requestd:6061`, `batcherd:6062`, fact 8); 8 rules from `docs/Observability.md` with `severity` and `runbook_anchor` labels; Alertmanager webhook to `http://copilot.ride-matching:8000/alerts`. `kubectl apply --dry-run=client` passes | — |
| 1.3 | sous-chef-infra | `k8s/fault-inject.sh` (new), `docs/Runbook.md` only if decision 5 is approved | 6 faults; each run inserts into `incidents_truth`, restores system; `--fault X --runs N`, `--none`. `shellcheck k8s/fault-inject.sh` passes; `--none --runs 1` writes a truth row | 1.2 for alerting |
| 1.4 | line-cook | `copilot/pyproject.toml`, `copilot/ruff.toml`, `copilot/Dockerfile`, `copilot/.env.example`, `copilot/compose.yaml` | `cd copilot && uv sync && uv run ruff check . && uv run pytest` runs (0 tests OK). `docker compose config` valid | — |
| 1.5 | sous-chef-python | `copilot/copilot/ingest/logs.py`, `copilot/copilot/db.py`, `copilot/copilot/schema.sql`, tests | Loads JSON log lines from a file into `logs`; idempotent on re-run; test against Postgres from compose | 1.4 |
| 1.6 | sous-chef-python | `copilot/copilot/ingest/docs.py`, `copilot/copilot/embed.py`, tests | Chunks `docs/**/*.md` by heading, keeps `source_file` and `anchor`; embeddings via `Embedder` protocol; `FakeEmbedder` used in tests; `pytest -k docs` passes | 1.4, 1.5 (schema) |
| 1.7 | sous-chef-python | `copilot/copilot/detect/zscore.py`, tests | Rolling z-score over the AD-1 metrics from Prometheus HTTP API; a sustained deviation (N consecutive points beyond threshold) emits an event; tests use a recorded fixture, not a live Prometheus | 1.4 |

**Gate 1:** `reviewer` (Opus) on DP-1, DP-2, AD-1, the fault injector and JSON logging. Blocking issues fixed before Phase 2.

## 4. Phase 2 tickets: agents and API (PRD week 2)

| ID | Cook | Files | Acceptance | Depends on |
|---|---|---|---|---|
| 2.1 | sous-chef-python | `agents/tools/{metrics,logs,runbook}_tools.py` | Each tool matches §2.3; logs tool uses a read-only DB role (`copilot_ro`); tests reject non-listed query names | Phase 1 |
| 2.2 | sous-chef-python | `agents/coordinator.py` | ADK coordinator + metrics, logs, runbook agents; output validates as `Diagnosis` (AG-6); test with fake model returns valid JSON | 2.1, §5 prompts |
| 2.3 | sous-chef-python | `agents/critic.py` | Rejects claims with empty `evidence_ids`; one revision loop max; below floor → `needs_human` | 2.2 |
| 2.4 | sous-chef-python | `api/app.py`, `db.py` writes to `incidents_seen` | Endpoints in §2.4; `pytest` covers auth 401, rate limit 429, and storage | 2.2, 2.3 |
| 2.5 | sous-chef-python | `actions/k8s.py` | Only runs after approve (AC-2); bounds from §2.3; each step logged to `actions` (AC-3); tests use a fake K8s client | 2.4 |
| 2.6 | sous-chef-infra | `k8s/base/copilot.yaml`, `k8s/base/copilot-rbac.yaml` | ServiceAccount; Role limited to `pods` delete and `deployments/scale` patch in `ride-matching`; `kubectl auth can-i` checks show nothing else allowed | — |
| 2.7 | sous-chef-python | `agents/model.py` | Timeout, retry with backoff, Redis cache, circuit breaker; on failure `/diagnose` returns `fallback` instead of 500 | 2.2 |

I (head chef) run one fault end to end after 2.7: inject → alert → diagnosis → approve → action.

**Gate 2:** `reviewer` on AG-1 to AG-6, AC-1 to AC-3, the API. PRD rule: if no full cited diagnosis by end of Phase 2, drop the P1 items.

## 5. Head chef work (not delegated)

- `copilot/agents/prompts/coordinator.md`, `metrics.md`, `logs.md`, `runbook.md`: each agent's system prompt. Each prompt says: cite evidence IDs; never invent a runbook anchor; use only the listed tools.
- `copilot/agents/prompts/critic.md`: rubric. A claim passes if it has ≥1 evidence ID that exists in the evidence list and the excerpt supports it. A claim without that fails. Output is `CriticVerdict`.
- Traffic-burst vs slow-solver rule for the metrics agent: if `batch_size_mean` rose and `solve_seconds_per_rider` did not → traffic burst; if `solve_seconds_per_rider` rose with batch size flat → slow solver.

## 6. Phase 3 tickets: deploy and evaluate (PRD week 3)

| ID | Cook | Work | Acceptance | Depends on |
|---|---|---|---|---|
| 3.1 | sous-chef-python | `copilot/eval/run.py` runs 6 faults × 5 runs + 5 no-fault runs; scores against `incidents_truth`; every PRD metric, with and without critic; writes `copilot/eval/RESULTS.md` | RESULTS.md has all 7 metric rows, measured values | Gate 2 |
| 3.2 | sous-chef-python | OpenTelemetry spans per agent step; Prometheus metrics for tokens/sec, cost, p95 latency, tool errors | Spans and metrics visible locally | 2.7 |
| 3.3 | sous-chef-infra | GCP: Cloud Run, Cloud SQL + pgvector, Secret Manager, `deploy/gcp/README.md` | Budget alert created **first**; README has exact commands. Needs GCP project ID and Vertex region from you | Gate 2 |
| 3.4 | sous-chef-python | P1 only if all P0 pass: Isolation Forest (AD-2), lead-time comparison | Lead time table in RESULTS.md | 3.1 |
| 3.5 | line-cook | `copilot/README.md`, `weekly report/week25.md` entry | README has one-command demo and results copied from RESULTS.md | 3.1 |

**Gate 3:** `reviewer` on the full PRD. Then the final summary (built, measured, not done, 3 resume bullets with measured numbers only).

## 7. Open decisions for you

1. Decision 2: `dropped-requests` mapped to dead-letter, or add an engine counter?
2. Decision 5: may I add a `redis-stall` section to `docs/Runbook.md`?
3. Decision 6: 3.5 s (runbook) or 5 s (SLO) as the diagnosis threshold for high latency?
4. Embedding size: plan assumes Vertex `text-embedding-005` (768 dims). Confirm the model.
5. GCP project ID and Vertex region: needed at Phase 3 only (ticket 3.3), not now.

Nothing in Phase 1 starts until you approve this plan.
