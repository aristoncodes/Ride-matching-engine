# Incident Copilot — PRD

Nov 16, 2026 · @Anushka

Incident Copilot is an AI on-call assistant for the Ride Matching Engine. When an alert fires, it reads the system's metrics, logs and runbooks, names the root cause with cited evidence, and proposes a fix that a human must approve. It ships in 3 weeks as a `copilot/` Python service in the same repo, deployed on Google Cloud.

## Overview

**Problem.** The engine already exposes 15 Prometheus metrics, alert rules, a 6-section runbook and 6 ADRs. Diagnosing an incident still means a person reading dashboards, grepping logs and matching symptoms to the right runbook section by hand. With 3 Go services, C++ matcher pods and Redis Streams, the same symptom (rising match latency) can come from several causes.

**Solution.** A multi-agent system that does that triage automatically: an anomaly detector and the existing alert rules trigger it, specialist agents query metrics and logs, a RAG agent retrieves the matching runbook section, and a critic rejects any diagnosis not backed by evidence.

**Why this project.** It closes the two gaps found against the Google Forward Deployed Engineer Intern posting: production AI on a cloud platform, and data pipelines with vector databases and RAG. It builds on an existing, well-instrumented system rather than a toy, and the chaos tests give a test set with known answers.

## Goals and non-goals

### Goals

1. Diagnose injected incidents automatically, naming the correct runbook section and failing component, with every claim citing a metric, log line or doc.
2. Detect problems earlier than the existing static alert rules, using a small anomaly detector on the metrics.
3. Propose remediation actions that run only after explicit human approval.
4. Run on Google Cloud with measured cost, latency and accuracy.
5. Report honest results from a fault-injection test set with known answers.

### Non-goals (for v1)

- Fully automatic remediation without a human in the loop.
- Changes to the C++ matcher or the matching algorithm.
- A dashboard UI; the API, Alertmanager webhook and an optional Slack message are enough.
- Supporting systems other than the Ride Matching Engine.
- Training or fine-tuning a large language model.

## Users and user stories

| User | Story |
|---|---|
| On-call engineer | When an alert fires at night, I get a diagnosis with evidence within a minute, so I start from a likely cause instead of a blank dashboard. |
| On-call engineer | I can ask a follow-up ("show me the dead-letter trend") and get an answer from real data. |
| On-call engineer | Before anything changes in the cluster, I see the proposed action and approve or reject it. |
| Service owner | I see past incidents, what the copilot concluded and whether it was right. |
| Reviewer (interviewer) | I can run one command, inject a fault and watch the copilot diagnose it, with accuracy numbers in the README. |

## Scope: what exists and what to build

Most of the engine-side plumbing already exists in aristoncodes/ride-matching-engine. v1 adds four small engine changes and the new `copilot/` service.

| Area | Already in the repo | To build |
|---|---|---|
| Metrics | 15 Prometheus metrics with SLOs in `internal/metrics/metrics.go`; `/metrics` on admin port 6060 in ingestd, requestd, batcherd | A Prometheus server in the kind cluster scraping the three admin ports |
| Alerts | Alert rules written in `docs/Observability.md` | Load them as Prometheus rules; Alertmanager webhook to the copilot |
| Logs | slog with `request_id` on every line | Switch `NewTextHandler` to `NewJSONHandler` in the three `main.go` files (the doc already says JSON) |
| Knowledge | `docs/Runbook.md` (6 sections with anchors), 6 ADRs, Architecture, SLO_SLA, Rollout_Rollback, weekly learnings | Chunk, embed and index into pgvector |
| Chaos | `k8s/chaos-test.sh` kills all engine pods and audits Redis | Fault injector with 6 fault types that records ground truth |
| Actions | None | Kubernetes actions (restart pod, scale deployment) through a narrowly scoped ServiceAccount |
| Copilot | None | Python service: ingestion, anomaly detection, ADK agents, FastAPI, evaluation |

## Functional requirements

IDs are referenced by the evaluation and milestones. P0 = required for v1, P1 = if time allows.

### Data pipeline

| ID | Requirement | Priority |
|---|---|---|
| DP-1 | Load JSON logs from the three Go services into a Postgres `logs` table (time, service, level, request_id, event, error, fields as JSONB), indexed on time and request_id | P0 |
| DP-2 | Split all docs by heading, keep the source file and anchor id with each chunk, embed with Vertex AI, store in pgvector | P0 |
| DP-3 | Re-index docs incrementally when a file's hash changes | P1 |
| DP-4 | Store every diagnosis, its evidence and the human verdict in an `incidents_seen` table | P0 |

### Anomaly detection

| ID | Requirement | Priority |
|---|---|---|
| AD-1 | Rolling z-score on queue depth, queue pending, dead-lettered, solve time p99, dependency errors and drivers indexed; flag sustained deviations | P0 |
| AD-2 | Isolation Forest over the same metrics as a multivariate detector, compared with AD-1 | P1 |
| AD-3 | A detection, or an Alertmanager alert, opens an incident and calls `/diagnose` | P0 |

### Agents (Google ADK)

| ID | Requirement | Priority |
|---|---|---|
| AG-1 | Coordinator plans the investigation, hands off to specialists, and merges their findings into one diagnosis | P0 |
| AG-2 | Metrics agent runs PromQL over a time window through a fixed set of query tools | P0 |
| AG-3 | Logs agent runs read-only, parameterised SQL over the logs table (errors by service, trace by request_id) | P0 |
| AG-4 | Runbook agent retrieves the top chunks from pgvector and returns them with citations | P0 |
| AG-5 | Critic rejects a diagnosis whose claims lack cited evidence and sends it back once for revision | P0 |
| AG-6 | Output schema: summary, root cause, component, runbook anchor, confidence, evidence list, proposed action | P0 |

### Actions

| ID | Requirement | Priority |
|---|---|---|
| AC-1 | Allowed actions only: restart matching-engine pods, scale batcherd or matching-engine within set limits | P0 |
| AC-2 | No action runs without `POST /actions/{id}/approve` from a human | P0 |
| AC-3 | Every proposed, approved, rejected and executed action is logged with who and when | P0 |

### API (FastAPI)

| Endpoint | Purpose | Priority |
|---|---|---|
| `POST /diagnose` | Start a diagnosis from an alert payload or a question | P0 |
| `POST /alerts` | Alertmanager webhook receiver | P0 |
| `GET /incidents/{id}` | Diagnosis, evidence, trace and status | P0 |
| `POST /actions/{id}/approve`, `/reject` | Human decision on a proposed action | P0 |
| `POST /incidents/{id}/ask` | Follow-up question in the same context | P1 |

## System architecture

The copilot never touches the engine's code: the engine only exports logs and metrics, and receives restart or scale calls through the Kubernetes API after a human approves.

Components: Ride Matching Engine (Go services ingestd/requestd/batcherd, C++ matcher pods, Redis request stream and driver GEO) → Logs table (JSON slog to Postgres), Prometheus (metrics and alert rules), Knowledge base (runbook and ADRs to pgvector), Kubernetes API (restart and scale only). Incident Copilot (Python on Cloud Run): anomaly detector (z-score, Isolation Forest), FastAPI (alerts in, human approves), coordinator agent (plans, delegates, merges), with metrics agent (PromQL tools), logs agent (read-only SQL), runbook agent (pgvector and citations) and critic agent (evidence check).

### One incident, end to end

1. A fault hits the engine; queue depth or dead-lettered count starts to move.
2. The anomaly detector or an Alertmanager rule fires and calls `POST /alerts`.
3. The coordinator sends the metrics, logs and runbook agents to investigate in parallel.
4. The critic checks that every claim cites evidence; unsupported drafts go back once.
5. The API returns the diagnosis and a proposed action; nothing runs until a human approves.
6. Every step, token count and cost is traced and stored with the incident.

## Evaluation

The copilot is scored against faults injected on purpose, so every incident has a known answer. The injector writes `{fault, target, start_time, expected_runbook}` to an `incidents_truth` table that the copilot can never read.

### Fault types (each maps to a runbook section)

| Fault injected | How | Expected diagnosis |
|---|---|---|
| Engine outage | Delete all matching-engine pods (existing chaos test) | Runbook#worker-crashloop |
| Batchers stopped | Scale batcherd to 0 | Runbook#queue-backlog |
| Ingestion down | Stop ingestd or the mock drivers | Runbook#ingestion-down |
| Traffic burst | Spike requests with `cmd/loadtest` | Runbook#high-latency |
| Slow solver | Lower the engine's CPU limit | Runbook#high-latency |
| Redis stall | `CLIENT PAUSE` on Redis | Redis dependency errors |

Target size: 6 fault types × 5–8 runs = 30–50 incidents, plus 5 no-fault runs to measure false alarms.

### Metrics and v1 targets

Targets are starting goals to revisit after the first run, not results.

| Metric | Definition | Target |
|---|---|---|
| Root-cause accuracy | Correct runbook anchor and component | ≥ 80% |
| Retrieval recall@5 | Correct runbook chunk in the top 5 | ≥ 90% |
| Unsupported-claim rate | Diagnoses with a claim not backed by cited evidence | ≤ 5% |
| False-alarm rate | No-fault runs that open an incident | ≤ 1 in 5 |
| Time to diagnosis | Incident opened to diagnosis returned, p95 | ≤ 60 s |
| Detection lead time | Anomaly detector fires vs the static alert rule | Report measured value |
| Cost per diagnosis | Model tokens × price | Report measured value |

Also report results with and without the critic, to show what the self-reflection step adds.

## Non-functional requirements

- **Security.** The copilot's Kubernetes ServiceAccount can only delete pods and patch deployment scale in the `ride-matching` namespace. Agent SQL is read-only and parameterised. API keys live in Secret Manager. The API requires an auth token and is rate limited. Logs already exclude API keys, per `docs/Observability.md`.
- **Reliability.** Model calls sit behind a timeout, retry with backoff, a Redis cache and a circuit breaker. If the model is unavailable, `/diagnose` returns the raw evidence and the top runbook matches instead of failing.
- **Safety.** The copilot never acts without approval and states its confidence. Low confidence returns "needs human" instead of a guess.
- **Observability.** Every agent step is traced (OpenTelemetry to Cloud Trace). The copilot exports its own Prometheus metrics: tokens/sec, cost per request, p95 latency, tool errors.
- **Cost.** Set a GCP budget alert before the first deploy. Cache embeddings and repeated queries.

## Tech stack and deployment

| Layer | Choice | Why |
|---|---|---|
| Language | Python 3.12 | Required by the FDE posting; best agent and ML libraries |
| Agents | Google ADK | Google's own agent framework, a direct match to the posting |
| Models | Gemini on Vertex AI; Vertex AI text embeddings | Managed, on GCP |
| Vector DB | Postgres + pgvector (Cloud SQL) | One database for logs, vectors and incidents |
| Metrics | Prometheus + Alertmanager | Already the engine's metrics format |
| ML | scikit-learn (Isolation Forest), NumPy (z-score) | Small, explainable models |
| API | FastAPI | Typed request and response models |
| Actions | Kubernetes Python client | Talks to the existing kind or GKE cluster |
| Tracing | OpenTelemetry to Cloud Trace | Per-step agent traces |

**Deployment in two stages**

1. **Local:** everything on the existing kind cluster plus a local Postgres and Prometheus, for fast iteration at no cost.
2. **GCP:** copilot on Cloud Run, database on Cloud SQL, models on Vertex AI, secrets in Secret Manager. The engine runs on GKE Autopilot or a small Compute Engine VM. The connections (logs, metrics, HTTP, Kubernetes API) stay the same, so the design does not change.

## Milestones

v1 ships in three weeks, gated on a first cited diagnosis.

- **Week 1 · Signals and data (Nov 17 – Nov 23):** JSON logs + Prometheus in kind; fault injector, 6 faults; docs → pgvector (DP-2); logs → Postgres (DP-1); z-score detector (AD-1).
- **Week 2 · Agents and API (Nov 24 – Nov 30):** ADK coordinator + 3 agents; critic agent (AG-5); FastAPI + Alertmanager hook; approved k8s actions (AC-1 to AC-3); first end-to-end diagnosis.
- **Week 3 · Deploy, evaluate (Dec 1 – Dec 7):** deploy to Cloud Run and Cloud SQL; run 30–50 fault incidents; tracing, cost and latency metrics; Isolation Forest if time (P1); README and resume bullets.

**Gates:** first cited diagnosis (end to end, end of week 2); all P0 pass with numbers in the README (end of week 3).

If week 2 ends without one full cited diagnosis, week 3 drops the P1 items and the cloud deploy waits until it works locally.

## Success, risks and open questions

**v1 is done when** a reviewer can run one command, inject any of the 6 faults, receive a cited diagnosis within 60 seconds, approve the proposed action, and read the measured accuracy, cost and latency in the README.

| Risk | Mitigation |
|---|---|
| Two faults look alike (traffic burst and slow solver both raise latency) | Give the metrics agent queries that separate them: batch size vs solve time per rider |
| The model invents causes | Critic plus required citations; "needs human" on low confidence |
| GCP cost overrun | Budget alert on day one; develop locally on kind |
| Too few incidents for a meaningful score | Automate the injector so runs cost minutes, not hours |
| Scope creep (UI, Slack, auto-fix) | Non-goals list; P1 items only after all P0 items pass |

**Open questions**

- GKE Autopilot or a Compute Engine VM for the engine on GCP?
- Is a Slack notification worth adding in week 3, or left for later?

## Mapping to the Google FDE requirements

| Requirement | Where it is covered |
|---|---|
| Python programming | Whole copilot service |
| Production-oriented AI | Cloud Run deploy, approvals, circuit breaker, fallbacks |
| Data pipelines | DP-1 to DP-4 |
| AI/ML models | Gemini and embeddings; own anomaly detector (AD-1, AD-2) |
| RAG architecture | AG-4 with citations |
| Vector databases | pgvector on Cloud SQL |
| Multi-agent systems | AG-1 to AG-5 (delegation, ReAct tools, self-reflection) |
| Google Cloud | Cloud Run, Cloud SQL, Vertex AI, Secret Manager, Cloud Trace |
| APIs | FastAPI endpoints and Alertmanager webhook |
| Monitoring and evaluation | Fault-injection test set, tracing, LLM metrics |
