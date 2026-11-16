"""Offline tests for eval/score.py (pure functions, synthetic rows)."""

from __future__ import annotations

import importlib.util
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "eval_score", Path(__file__).resolve().parent.parent / "eval" / "score.py"
)
sc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sc)

T0 = "2026-01-01T00:00:00+00:00"
T1 = "2026-01-01T00:02:00+00:00"
TS = "2026-01-01T00:00:10+00:00"


def diag(anchor="queue-backlog", component="batcherd", claims=None, evidence=("E1",)):
    return {
        "runbook_anchor": anchor,
        "component": component,
        "claims": claims if claims is not None else [{"text": "x", "evidence_ids": ["E1"]}],
        "evidence": [{"id": i} for i in evidence],
    }


def res(fault="batchers_stopped", d=None, **kw):
    r = {"fault": fault, "critic": "on", "window_start": T0, "window_end": T1,
         "diagnosis": d, "incident_opened": d is not None, "retrieval_top5": [],
         "latency_s": 10.0, "cost_usd": 0.01}  # fmt: skip
    r.update(kw)
    return r


def truth(fault="batchers_stopped", expected="queue-backlog", target="batcherd"):
    return {"run_id": "r", "fault": fault, "target": target, "start_time": TS,
            "expected_runbook": expected}  # fmt: skip


def valid(pairs):
    return [(r, t) for r, t in pairs]


def test_accuracy_correct_and_incorrect():
    ok = (res(d=diag()), truth())
    bad_anchor = (res(d=diag(anchor="high-latency")), truth())
    bad_comp = (res(d=diag(component="requestd")), truth())
    acc, n = sc.root_cause_accuracy([ok, bad_anchor, bad_comp, ok])
    assert (acc, n) == (0.5, 4)


def test_accuracy_accepts_prefixed_anchor_and_no_diagnosis_is_wrong():
    ok = (res(d=diag(anchor="Runbook#queue-backlog")), truth(expected="Runbook#queue-backlog"))
    none = (res(d=None), truth())
    assert sc.root_cause_accuracy([ok, none])[0] == 0.5


def test_expected_component_map():
    assert sc.EXPECTED_COMPONENT["redis_stall"] == "redis"
    assert sc.EXPECTED_COMPONENT["traffic_burst"] == "matching-engine"


def test_recall_at_5_cutoff():
    t = truth()
    hit5 = (res(retrieval_top5=["a", "b", "c", "d", "queue-backlog"]), t)
    miss6 = (res(retrieval_top5=["a", "b", "c", "d", "e", "queue-backlog"]), t)
    assert sc.recall_at_5([hit5, miss6]) == (0.5, 2)


def test_unsupported_claims():
    empty = diag(claims=[{"text": "x", "evidence_ids": []}])
    unknown = diag(claims=[{"text": "x", "evidence_ids": ["E9"]}])
    good = diag()
    pairs = [(res(d=d), truth()) for d in (empty, unknown, good, good)]
    assert sc.unsupported_claim_rate(pairs) == (0.5, 4)


def test_false_alarm_rate():
    nf = truth(fault="none", expected=None, target="none")
    pairs = [
        (res(fault="none", d=diag()), nf),
        (res(fault="none", d=None), nf),
        (res(d=diag()), truth()),
    ]
    assert sc.false_alarms(pairs) == (1, 2)


def test_p95_small_list():
    assert sc.percentile([1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 95) == 10
    assert sc.percentile([5.0], 95) == 5.0
    assert sc.percentile([], 95) is None


def test_injection_failed_excluded_and_counted():
    good_t = truth()
    bad_t = truth(expected=None, target="batcherd [injection_failed]")
    r = [res(d=diag())]
    r2 = [res(d=diag()), res(d=diag())]
    m = sc.score(r2, [good_t, bad_t])
    assert m["valid_incidents"] == 1
    assert m["excluded_injection_failed"] == 1
    assert sc.score(r, [good_t])["excluded_injection_failed"] == 0


def test_small_sample_label_and_lead_time_not_measured():
    m = sc.score([res(d=diag())], [truth()])
    assert m["small_sample"] is True
    assert m["lead_time_s"] is None
    table = sc.render_table({"on": m})
    assert "small sample" in table and "not measured" in table
    assert sc.small_sample(30) is False


def test_lead_time_measured_when_timestamps_present():
    r = res(d=diag(), detector_fired_at=T0, alert_fired_at=TS)
    assert sc.score([r], [truth()])["lead_time_s"] == 10.0


def test_both_critic_columns():
    rows = [res(d=diag()), res(d=diag(), critic="off")]
    by = sc.score_by_critic(rows, [truth(), truth()])
    assert set(by) == {"on", "off"}
    t = sc.render_table(by)
    assert "Critic on" in t and "Critic off" in t
