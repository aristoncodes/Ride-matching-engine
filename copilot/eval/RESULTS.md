# Evaluation results

Status: **not run**. Every value below is a placeholder until `eval/run.py` has run against a
live cluster and model. Targets are v1 goals, not results (PRD, Evaluation).

Generate the table with `uv run python eval/run.py --score eval/results/<on>.jsonl eval/results/<off>.jsonl`
and paste it here. Fewer than 30 valid incidents is labelled "small sample".

| Metric | Target | Critic on | Critic off |
|---|---|---|---|
| Root-cause accuracy | >= 80% | not measured | not measured |
| Retrieval recall@5 | >= 90% | not measured | not measured |
| Unsupported-claim rate | <= 5% | not measured | not measured |
| False-alarm rate | <= 1 in 5 | not measured | not measured |
| Time to diagnosis (p95) | <= 60 s | not measured | not measured |
| Detection lead time | report measured value | not measured | not measured |
| Cost per diagnosis (mean) | report measured value | not measured | not measured |
| Valid incidents | | not measured | not measured |
| Excluded [injection_failed] | | not measured | not measured |

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
