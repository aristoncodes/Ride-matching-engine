"""Pure scoring functions for the evaluation harness (PRD Evaluation section).

No I/O. A result row is a dict written by eval/run.py; a truth row is a dict read from
incidents_truth by the scorer. Rows:

  result: {run_id, fault, critic ("on"|"off"), window_start, window_end (ISO UTC),
           diagnosis (Diagnosis dict or None), incident_opened (bool),
           retrieval_top5 (list of anchors), latency_s, tokens_in, tokens_out, cost_usd,
           detector_fired_at / alert_fired_at (optional ISO timestamps)}
  truth:  {run_id, fault, target, start_time, expected_runbook}
"""

from __future__ import annotations

import math
from datetime import datetime, timedelta
from typing import Any

EXPECTED_COMPONENT = {
    "engine_outage": "matching-engine",
    "batchers_stopped": "batcherd",
    "ingestion_down": "ingestd",
    "traffic_burst": "matching-engine",
    "slow_solver": "matching-engine",
    "redis_stall": "redis",
}
NO_FAULT = "none"
INJECTION_FAILED = "[injection_failed]"
SMALL_SAMPLE_MIN = 30
NOT_MEASURED = "not measured"
TARGETS = {
    "accuracy": ">= 80%",
    "recall_at_5": ">= 90%",
    "unsupported_claim_rate": "<= 5%",
    "false_alarm_rate": "<= 1 in 5",
    "p95_latency_s": "<= 60 s",
    "lead_time_s": "report measured value",
    "mean_cost_usd": "report measured value",
}
MATCH_SLOP = timedelta(seconds=30)


def anchor_of(ref: str | None) -> str | None:
    """'docs/Runbook.md#queue-backlog' or 'Runbook#queue-backlog' -> 'queue-backlog'."""
    if not ref:
        return None
    return ref.rsplit("#", 1)[-1].strip() or None


def injection_failed(truth: dict) -> bool:
    return INJECTION_FAILED in (truth.get("target") or "")


def _ts(v: Any) -> datetime:
    return v if isinstance(v, datetime) else datetime.fromisoformat(str(v).replace("Z", "+00:00"))


def match_truth(results: list[dict], truths: list[dict]) -> list[tuple[dict, dict | None]]:
    """Pair each result with the truth row of the same fault whose start_time falls in the
    result's injector window (each truth row is used once). Unmatched results pair with None."""
    free = list(truths)
    out: list[tuple[dict, dict | None]] = []
    for r in sorted(results, key=lambda x: str(x.get("window_start"))):
        lo, hi = _ts(r["window_start"]) - MATCH_SLOP, _ts(r["window_end"]) + MATCH_SLOP
        found = next(
            (t for t in free if t["fault"] == r["fault"] and lo <= _ts(t["start_time"]) <= hi),
            None,
        )
        if found is not None:
            free.remove(found)
        out.append((r, found))
    return out


def split_valid(pairs: list[tuple[dict, dict | None]]) -> tuple[list[tuple[dict, dict]], int, int]:
    """(valid pairs, excluded as injection_failed, unmatched with no truth row)."""
    valid, failed, unmatched = [], 0, 0
    for r, t in pairs:
        if t is None:
            unmatched += 1
        elif injection_failed(t):
            failed += 1
        else:
            valid.append((r, t))
    return valid, failed, unmatched


def is_correct(result: dict, truth: dict) -> bool:
    """Correct runbook anchor AND the fault's expected component."""
    d = result.get("diagnosis")
    if not d:
        return False
    want = anchor_of(truth.get("expected_runbook"))
    comp = EXPECTED_COMPONENT.get(truth.get("fault"))
    return (
        want is not None
        and anchor_of(d.get("runbook_anchor")) == want
        and d.get("component") == comp
    )


def _fault_pairs(valid: list[tuple[dict, dict]]) -> list[tuple[dict, dict]]:
    return [(r, t) for r, t in valid if t["fault"] != NO_FAULT]


def root_cause_accuracy(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    fp = _fault_pairs(valid)
    if not fp:
        return None, 0
    return sum(is_correct(r, t) for r, t in fp) / len(fp), len(fp)


def recall_at_5(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    fp = [(r, t) for r, t in _fault_pairs(valid) if anchor_of(t.get("expected_runbook"))]
    if not fp:
        return None, 0
    hit = sum(
        anchor_of(t["expected_runbook"])
        in {anchor_of(a) for a in (r.get("retrieval_top5") or [])[:5]}
        for r, t in fp
    )
    return hit / len(fp), len(fp)


def has_unsupported_claim(diagnosis: dict) -> bool:
    known = {e.get("id") for e in diagnosis.get("evidence") or []}
    for c in diagnosis.get("claims") or []:
        ids = c.get("evidence_ids") or []
        if not ids or any(i not in known for i in ids):
            return True
    return False


def unsupported_claim_rate(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    ds = [r["diagnosis"] for r, _ in valid if r.get("diagnosis")]
    if not ds:
        return None, 0
    return sum(has_unsupported_claim(d) for d in ds) / len(ds), len(ds)


def false_alarms(valid: list[tuple[dict, dict]]) -> tuple[int | None, int]:
    """(no-fault runs that opened an incident, no-fault runs)."""
    nf = [r for r, t in valid if t["fault"] == NO_FAULT]
    if not nf:
        return None, 0
    return sum(bool(r.get("incident_opened")) for r in nf), len(nf)


def percentile(values: list[float], p: float) -> float | None:
    """Nearest-rank percentile."""
    if not values:
        return None
    s = sorted(values)
    return s[max(1, math.ceil(p / 100 * len(s))) - 1]


def p95_latency(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    xs = [float(r["latency_s"]) for r, _ in valid if r.get("latency_s") is not None]
    return percentile(xs, 95), len(xs)


def mean_cost(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    xs = [float(r["cost_usd"]) for r, _ in valid if r.get("cost_usd") is not None]
    return (sum(xs) / len(xs) if xs else None), len(xs)


def lead_time(valid: list[tuple[dict, dict]]) -> tuple[float | None, int]:
    """Mean seconds the detector fired before the static alert; None if no timestamps."""
    xs = [
        (_ts(r["alert_fired_at"]) - _ts(r["detector_fired_at"])).total_seconds()
        for r, _ in valid
        if r.get("alert_fired_at") and r.get("detector_fired_at")
    ]
    return (sum(xs) / len(xs) if xs else None), len(xs)


def small_sample(n_valid: int) -> bool:
    return n_valid < SMALL_SAMPLE_MIN


def score(results: list[dict], truths: list[dict]) -> dict:
    """All metrics for one critic setting's results."""
    valid, failed, unmatched = split_valid(match_truth(results, truths))
    acc, acc_n = root_cause_accuracy(valid)
    rec, rec_n = recall_at_5(valid)
    uns, uns_n = unsupported_claim_rate(valid)
    fa, fa_n = false_alarms(valid)
    p95, p95_n = p95_latency(valid)
    lead, lead_n = lead_time(valid)
    cost, cost_n = mean_cost(valid)
    return {
        "valid_incidents": len(valid),
        "excluded_injection_failed": failed,
        "unmatched_no_truth": unmatched,
        "small_sample": small_sample(len(valid)),
        "accuracy": acc,
        "accuracy_n": acc_n,
        "recall_at_5": rec,
        "recall_at_5_n": rec_n,
        "unsupported_claim_rate": uns,
        "unsupported_claim_rate_n": uns_n,
        "false_alarms": fa,
        "false_alarm_runs": fa_n,
        "p95_latency_s": p95,
        "p95_latency_s_n": p95_n,
        "lead_time_s": lead,
        "lead_time_s_n": lead_n,
        "mean_cost_usd": cost,
        "mean_cost_usd_n": cost_n,
    }


def score_by_critic(results: list[dict], truths: list[dict]) -> dict[str, dict]:
    """{'on': metrics, 'off': metrics} for each critic setting present in the results."""
    out = {}
    for mode in ("on", "off"):
        rows = [r for r in results if r.get("critic") == mode]
        if rows:
            out[mode] = score(rows, truths)
    return out


def _pct(v: float | None) -> str:
    return NOT_MEASURED if v is None else f"{v * 100:.0f}%"


def _cell(m: dict, key: str) -> str:
    v = m[key]
    flag = (
        " (small sample)"
        if m["small_sample"]
        and key in ("accuracy", "recall_at_5", "unsupported_claim_rate", "false_alarms")
        else ""
    )
    if v is None:
        return NOT_MEASURED
    if key in ("accuracy", "recall_at_5", "unsupported_claim_rate"):
        return _pct(v) + flag
    if key == "false_alarms":
        return f"{v} of {m['false_alarm_runs']}{flag}"
    if key == "p95_latency_s":
        return f"{v:.1f} s"
    if key == "lead_time_s":
        return f"{v:.1f} s"
    return f"${v:.4f}"


ROWS = [
    ("Root-cause accuracy", "accuracy"),
    ("Retrieval recall@5", "recall_at_5"),
    ("Unsupported-claim rate", "unsupported_claim_rate"),
    ("False-alarm rate", "false_alarms"),
    ("Time to diagnosis (p95)", "p95_latency_s"),
    ("Detection lead time", "lead_time_s"),
    ("Cost per diagnosis (mean)", "mean_cost_usd"),
]


def render_table(by_critic: dict[str, dict]) -> str:
    """Markdown metric table with a column per critic setting present."""
    modes = [m for m in ("on", "off") if m in by_critic]
    head = "| Metric | Target | " + " | ".join(f"Critic {m}" for m in modes) + " |"
    sep = "|---|---|" + "---|" * len(modes)
    lines = [head, sep]
    for label, key in ROWS:
        tkey = "false_alarm_rate" if key == "false_alarms" else key
        cells = " | ".join(_cell(by_critic[m], key) for m in modes)
        lines.append(f"| {label} | {TARGETS[tkey]} | {cells} |")
    for label, key in (
        ("Valid incidents", "valid_incidents"),
        ("Excluded [injection_failed]", "excluded_injection_failed"),
        ("Unmatched (no truth row)", "unmatched_no_truth"),
    ):
        lines.append(f"| {label} | | " + " | ".join(str(by_critic[m][key]) for m in modes) + " |")
    return "\n".join(lines)
