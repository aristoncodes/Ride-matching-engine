import asyncio
import json

import pytest

from copilot.agents import coordinator as co
from copilot.agents import critic as cr
from copilot.agents.model import ModelUnavailable
from copilot.models import Claim, Diagnosis, Evidence
from tests.test_coordinator import Emb, Prom, Rows, diag, scripts


class FakeModel:
    """Coordinator/specialists scripted by role; the critic has its own reply queue."""

    def __init__(self, scripts_, critic=(), critic_unavailable=False):
        self.scripts = {k: list(v) for k, v in scripts_.items()}
        self.critic = list(critic)
        self.critic_unavailable = critic_unavailable
        self.critic_prompts = []
        self.coord_prompts = []

    async def generate(self, system, user):
        if system == cr.critic_prompt():
            self.critic_prompts.append(user)
            if self.critic_unavailable:
                raise ModelUnavailable("down")
            return self.critic.pop(0)
        for role in ("metrics", "logs", "runbook", "coordinator"):
            if system == co._prompt(role):
                if role == "coordinator":
                    self.coord_prompts.append(user)
                return self.scripts[role].pop(0)
        raise AssertionError("unknown system prompt")


def verdict(ok, idx=(), fb=""):
    return json.dumps({"ok": ok, "unsupported_claim_indexes": list(idx), "feedback": fb})


def draft(claims):
    return json.dumps(
        {
            "summary": "s",
            "root_cause": "r",
            "component": "batcherd",
            "runbook_anchor": "docs/Runbook.md#queue-backlog",
            "confidence": 0.9,
            "claims": [{"text": t, "evidence_ids": ids} for t, ids in claims],
            "proposed_action": {"kind": "scale", "deployment": "batcherd", "replicas": 2},
        }
    )


EVID = [
    Evidence(id="E1", kind="metric", ref="m", excerpt="x"),
    Evidence(id="E2", kind="doc", ref="docs/Runbook.md#queue-backlog", excerpt="y"),
]


def d(claims, anchor="docs/Runbook.md#queue-backlog"):
    return Diagnosis(
        summary="s",
        root_cause="r",
        component="batcherd",
        runbook_anchor=anchor,
        confidence=0.9,
        claims=[Claim(text=t, evidence_ids=i) for t, i in claims],
    )


async def run(model, **kw):
    return await co.investigate(
        {"alert": "Q"},
        model=model,
        pool_ro=Rows(),
        prom_client=Prom(),
        prom_base_url="http://p",
        embedder=Emb(),
        **kw,
    )


def test_empty_evidence_rejected_without_model_call():
    m = FakeModel({}, critic=[verdict(True)])
    v = asyncio.run(cr.review(d([("a", [])]), EVID, model=m))
    assert not v.ok and v.unsupported_claim_indexes == [0]
    assert m.critic_prompts == []


def test_unknown_id_rejected_in_code_and_model_asked_only_about_rest():
    m = FakeModel({}, critic=[verdict(True)])
    v = asyncio.run(cr.review(d([("a", ["E9"]), ("b", ["E1"])]), EVID, model=m))
    assert not v.ok and v.unsupported_claim_indexes == [0]
    sent = json.loads(m.critic_prompts[0])
    assert [c["index"] for c in sent["claims"]] == [1]


def test_unknown_anchor_fails():
    m = FakeModel({}, critic=[verdict(True)])
    v = asyncio.run(cr.review(d([("a", ["E1"])], anchor="x#y"), EVID, model=m))
    assert not v.ok and "anchor" in v.feedback


def test_invalid_critic_json_fails_closed():
    m = FakeModel({}, critic=["not json"])
    v = asyncio.run(cr.review(d([("a", ["E1"]), ("b", ["E1"])]), EVID, model=m))
    assert not v.ok and v.unsupported_claim_indexes == [0, 1]
    assert v.feedback == "critic output invalid"


def test_valid_draft_passes_first_review_no_revision():
    m = FakeModel(scripts([diag()]), critic=[verdict(True)])
    r = asyncio.run(run(m))
    assert r.diagnosis.needs_human is False and r.critic_enabled is True
    assert len(m.coord_prompts) == 1 and len(m.critic_prompts) == 1
    assert [t["status"] for t in r.trace if t["agent"] == "critic"] == ["pass"]


def test_fail_once_revised_once_then_passes():
    m = FakeModel(scripts([diag(), diag()]), critic=[verdict(False, [0], "cite E2"), verdict(True)])
    r = asyncio.run(run(m))
    assert r.diagnosis.needs_human is False
    assert len(m.coord_prompts) == 2
    assert "cite E2" in m.coord_prompts[1] and "[0]" in m.coord_prompts[1]
    assert [t["status"] for t in r.trace if t["agent"] == "critic"] == ["fail", "pass"]
    assert all(t["tool"] is None for t in r.trace if t["agent"] == "critic")


def test_fail_twice_needs_human_and_failing_claims_removed():
    two = draft([("keep", ["E1"]), ("bad", ["E2"])])
    m = FakeModel(
        scripts([two, two]),
        critic=[verdict(False, [1], "f1"), verdict(False, [1], "f2")],
    )
    r = asyncio.run(run(m))
    assert len(m.coord_prompts) == 2 and len(m.critic_prompts) == 2
    assert r.diagnosis.needs_human is True
    assert r.diagnosis.proposed_action.kind == "none"
    assert [c.text for c in r.diagnosis.claims] == ["keep"]


def test_critic_disabled_makes_no_critic_calls():
    m = FakeModel(scripts([diag()]))
    r = asyncio.run(run(m, critic=False))
    assert r.critic_enabled is False and m.critic_prompts == []
    assert not any(t["agent"] == "critic" for t in r.trace)
    assert r.diagnosis is not None


def test_critic_model_unavailable_propagates_to_investigate():
    m = FakeModel(scripts([diag()]), critic_unavailable=True)
    r = asyncio.run(run(m))
    assert r.diagnosis is None and len(r.evidence) == 3
    with pytest.raises(ModelUnavailable):
        asyncio.run(cr.review(d([("a", ["E1"])]), EVID, model=m))
