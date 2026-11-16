"""The copilot's own Prometheus metrics (ticket 3.2). Labels are bounded; no request data."""

from __future__ import annotations

from prometheus_client import CollectorRegistry, Counter, Histogram

REGISTRY = CollectorRegistry()

MODEL_TOKENS = Counter(
    "copilot_model_tokens_total", "Model tokens used", ["direction"], registry=REGISTRY
)
MODEL_COST = Counter(
    "copilot_model_cost_usd_total", "Estimated model spend in USD", registry=REGISTRY
)
DIAGNOSIS_SECONDS = Histogram(
    "copilot_diagnosis_seconds",
    "Wall time of one diagnosis",
    buckets=(1, 2, 5, 10, 20, 30, 45, 60, 90),
    registry=REGISTRY,
)
TOOL_ERRORS = Counter(
    "copilot_tool_errors_total", "Agent tool errors", ["tool"], registry=REGISTRY
)
DIAGNOSES = Counter(
    "copilot_diagnoses_total", "Diagnoses finished", ["status"], registry=REGISTRY
)
for _d in ("in", "out"):
    MODEL_TOKENS.labels(_d)
for _s in ("diagnosed", "needs_human", "failed"):
    DIAGNOSES.labels(_s)

KNOWN_TOOLS = {"query_metric", "query_logs", "search_runbook"}


def tool_error(tool: str) -> None:
    # The tool name comes from model output: bound the label set.
    TOOL_ERRORS.labels(tool if tool in KNOWN_TOOLS else "unknown").inc()
