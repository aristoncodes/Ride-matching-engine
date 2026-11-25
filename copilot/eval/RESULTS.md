# Evaluation results

Status: **run once, local model baseline** (2026-11-18). 35 runs (5 per fault x 6 faults, plus 5
no-fault), critic on, model `qwen2.5:3b` on a local Ollama server (8k context), results file
`eval/results/20261118T080317Z.jsonl`. The critic-off column was not run. Targets are v1 goals,
not results (PRD, Evaluation).

Settings that differ from the defaults: per-call model timeout 90 s (`MODEL_CALL_TIMEOUT_S`) and
per-investigation cap 180 s (`EVAL_INVESTIGATE_TIMEOUT_S`); the PRD target is 60 s. Late in the run
the Redis model cache was unreachable (it fails open), so some calls ran uncached.

Generate the table with `uv run python eval/run.py --score eval/results/<on>.jsonl eval/results/<off>.jsonl`
and paste it here. Fewer than 30 valid incidents is labelled "small sample".

| Metric | Target | Critic on | Critic off |
|---|---|---|---|
| Root-cause accuracy | >= 80% | 0% (30 valid) | not run |
| Retrieval recall@5 | >= 90% | 0% | not run |
| Unsupported-claim rate | <= 5% | 0% (vacuous: no diagnosis made any claim) | not run |
| False-alarm rate | <= 1 in 5 | 0 of 5 (see note) | not run |
| Time to diagnosis (p95) | <= 60 s | 69.5 s (mean 51 s, median 48 s, max 101 s) | not run |
| Detection lead time | report measured value | not measured | not run |
| Cost per diagnosis (mean) | report measured value | $0.00 (local model) | not run |
| Valid incidents | | 30 | not run |
| Excluded [injection_failed] | | 0 | not run |

## What the numbers mean

- 25 of 35 runs returned a diagnosis, and all 25 are identical: component `matching-engine`, runbook
  `docs/Observability.md#alerting-rules`, `needs_human: true`. The 3B model does not tell the faults
  apart, so accuracy is 0% (the component is right by chance for three faults; the runbook anchor is
  never right).
- Recall@5 is 0% partly because the search query comes from that generic summary. Retrieval has not
  been tested with a better query.
- False alarms are 0 of 5 only because the model flags every diagnosis `needs_human`, which counts as
  not opening an incident. 4 of the 5 no-fault runs still returned a diagnosis.
- The harness, injection, scoring and cluster restore all worked: 0 injection failures, 0 run errors.
  This is a measurement of the 3B model, not of the copilot design. A stronger model is the next run.

## Definitions

- Accuracy: runbook anchor equals the truth anchor and component equals the fault's expected component (engine_outage: matching-engine, batchers_stopped: batcherd, ingestion_down: ingestd, traffic_burst and slow_solver: matching-engine, redis_stall: redis). No diagnosis counts as wrong.
- Recall@5: expected anchor is among the top 5 runbook chunks for a query built from the diagnosis summary and root cause (the trigger text if there is no diagnosis).
- Unsupported claim: any claim with empty or unresolvable evidence_ids, over runs that returned a diagnosis.
- False alarm: a no-fault run whose diagnosis is not needs_human and names a runbook anchor.
- Lead time: only reported when result rows carry detector_fired_at and alert_fired_at.
- Runs whose truth row is marked [injection_failed] are excluded and counted.

## Caveats

- The investigation trigger is a neutral question; it never names the fault.
- The model response cache (Redis) is part of the guarded model; flush it between critic settings for clean latency and cost.
