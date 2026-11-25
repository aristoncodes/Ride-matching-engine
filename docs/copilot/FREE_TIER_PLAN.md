# Free-tier plan: close the gaps against the Google FDE posting

Status (2026-11-18): Phase A done. Phase B adapter and Phase C client code done. **Deploy
deferred by decision: no card, so no GCP billing; the repo is made deploy-ready instead**, see
`deploy/gcp/README.md`. Still open: B.5 (coordinator on ADK), the recorded-window replay,
the Neon load, and the Gemini eval (needs an AI Studio key). Companion to PRD.md and PLAN.md.

Goal: a deployed, demoable copilot that matches the Google Forward Deployed Engineer
posting, at ₹0 or close to it. Every cost claim below is marked **verified** (checked on the
vendor page or a current source) or **to check** (from a third-party note, or not checked yet).

## 1. The posting, mapped to the project

| Posting asks for | Where we are | Gap |
|---|---|---|
| Python, data structures and algorithms | Yes. 190 tests pass. | None |
| AI solutions taken from concept to launch | Built and evaluated locally. Not deployed. | **Deploy** |
| Architecting AI systems on a cloud platform (GCP) | Designed for GCP in PRD.md. Nothing is deployed there. | **Deploy to Cloud Run** |
| Pipelines for structured and unstructured data, vector DBs, RAG | Runbook and ADR ingest into pgvector, logs table, retrieval with citations | None (strong) |
| Multi-agent frameworks such as ADK, LangGraph, CrewAI | Custom coordinator and 4 specialist agents. **ADK is installed but not used** (BUILD_LOG deviation). | **Use ADK**, through an adapter that keeps our `GuardedModel` |
| ReAct, self-reflection, hierarchical delegation | ReAct-style tool loop, critic agent (AG-5), coordinator delegates | None |
| LLM-native metrics: tokens/sec, cost per request | Token counts and cost per diagnosis are recorded (`cost.py`, eval `cost_usd`). **Tokens/sec is not implemented**, and the Prometheus export in PRD.md is not built. | **Add tokens/sec and cost/request metrics** |
| Granular tracing | OpenTelemetry spans exist (`obs/tracing.py`). No exporter is configured. | **Configure an exporter** |
| Evaluation and debugging frameworks | Eval harness with fault injection, scoring, and a results table | None (strong). Needs a model that works, see §3 |
| Secure APIs | FastAPI with bearer tokens, webhook token, approvals | None |

Not coded: "leading technical discovery sessions" is a human skill. Put it in the resume and
interview story, not in the repo.

## 2. What is free (check before building)

| Piece | Free option | Status |
|---|---|---|
| Agent framework | Google ADK, open source | Verified: open source, no cost |
| Model | Gemini Flash or Flash-Lite via an AI Studio key, no billing needed | To check: Google's pricing page says per-project limits are shown in AI Studio, not published. Pro's free-tier status conflicts across sources, so use Flash. |
| Hosting | Cloud Run, us-central1, min instances 0, max 1 | To check: sources disagree on the free allowance (180k vs 240k vCPU-seconds, 360k vs 450k GiB-seconds). Read the pricing page before relying on it. Set a budget alert first (PLAN ticket 3.3 already says so). |
| Database | Neon Free, pgvector included, scales to zero | To check: storage cap is 0.5 GB or 1 GB depending on the source. Our schema uses `vector(384)`, so a few thousand chunks fit either way. |
| Redis (model cache) | Upstash free tier, or drop the cache for the demo | To check. `RedisCache` already degrades when Redis is unreachable. |
| Secrets | Cloud Run environment variables for the demo | Secret Manager's free allowance not checked. Not needed for a demo. |
| Tracing | OpenTelemetry to Cloud Trace, or a local collector | To check: Cloud Trace free allowance. The code only needs an exporter. |
| Metrics | Prometheus export from the copilot, scraped locally | Free |

## 3. The problem to solve before the demo: the engine is local

Cloud Run cannot reach `localhost` on your laptop. The copilot needs the engine's Prometheus
metrics and logs, and the kind cluster runs only on your machine. There are three options:

1. **Recorded data for the public demo (recommended).** Export a window of metrics and logs
   from the kind run into Neon and a static Prometheus snapshot. Cloud Run investigates the
   recording. The live kind cluster stays a local demo. This is free, safe, and repeatable.
2. **Expose the cluster through a tunnel** (cloudflared or ngrok). Free, but it opens the
   cluster's metrics to the internet and needs auth in front of it. Not recommended.
3. **Move the engine to GKE or a VM.** Costs money and is out of scope for ₹0.

Option 1 also gives the demo a fixed, known set of faults to show.

## 4. Model choice for the demo and the eval

- The local `qwen2.5:3b` run is slow and weak. Early results: about half the runs returned no
  diagnosis, and each investigation takes about 50 seconds.
- Gemini Flash through AI Studio is the better demo model and matches the posting's Google
  stack. Its free limits are per project and not published, so the eval may hit a rate limit
  like the Groq run did. Run the eval in small batches, and check the limits in AI Studio first.
- Keep the local model as the zero-cost fallback for development.

## 5. Plan

Phases are sized for the PRD's remaining weeks. Each step ends with a check.

**Phase A: add the missing metrics (about 1 day, no cloud needed)**
1. Add tokens/sec and cost-per-request to the copilot's Prometheus metrics (`obs/metrics.py`).
   Check: a test asserts the counters increase after a call with a fake model client.
2. Add a Gemini Flash price row (free) and a local-model row to `cost.py`. Check: the price
   table covers every model name in `.env`.
3. Configure a trace exporter. Check: one investigation produces a trace with one span per agent.

**Phase B: use ADK (about 2 days)**
4. Install `google-adk`. Write an adapter that implements ADK's model interface and calls
   `GuardedModel`, so timeouts, retries, the breaker and the cache still apply. **To check:**
   the exact ADK extension point for a custom model in the installed version.
5. Rebuild the coordinator as an ADK agent with the existing specialists as sub-agents.
   Check: the eval still runs end to end, and the diagnosis rate does not drop with the
   local model.
6. Update BUILD_LOG.md to record that the ADK deviation is resolved.

**Phase C: Gemini and Neon (about 1 day)**
7. Create an AI Studio key and point `GROQ_MODEL`-style config at Gemini Flash.
8. Create a Neon project, enable pgvector, apply `schema.sql`, and load the runbook.
   Check: `retrieval_top5` returns the same anchors as the local database for the same query.

**Phase D: demo data and deploy (about 2 days)**
9. Export a recorded fault window (metrics and logs) from kind into Neon and a static
   Prometheus snapshot (option 1 in §3).
10. Deploy the copilot to Cloud Run in us-central1 with min 0 and max 1. Set the budget alert
    first. Check: `GET /incidents` returns a diagnosis for the recorded window.

**Phase E: evaluate and write up (about 2 days)**
11. Run the eval on Gemini Flash in batches that fit the AI Studio limits. Report accuracy,
    latency, tokens/sec and cost per diagnosis from the measured run, next to the local-model
    run.
12. Write the README and resume bullets. Map each bullet to a posting line from §1.

## 6. Decisions for you

1. **Demo data (§3):** recorded window (recommended) or tunnel?
2. **Demo model:** Gemini Flash (recommended, Google stack) or keep the local model?
3. **ADK:** adopt it (Phase B, recommended, it's in the posting and the PRD) or keep the plain
   loop and say why in the README?
4. **Order:** start with Phase A (no cloud, no spend), or go straight to the deploy?

## Sources

- Gemini API free tier and rate-limit page: https://ai.google.dev/gemini-api/docs/rate-limits
- Gemini API pricing: https://ai.google.dev/gemini-api/docs/pricing
- Cloud Run pricing: https://cloud.google.com/run/pricing
- Neon pricing: https://neon.com/pricing
- Neon pgvector: https://neon.com/docs/extensions/pgvector
