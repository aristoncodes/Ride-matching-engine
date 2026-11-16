"""Critic (ticket 2.3, PRD AG-5): checks every claim of a draft diagnosis against the evidence.

Cheap structural checks run in code first (empty or unknown evidence IDs fail without asking the
model). The model is asked only about the remaining claims and is told their original indexes.
Unparseable critic output fails closed. ModelUnavailable propagates to the caller.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

from pydantic import ValidationError

from copilot.agents.model import GuardedModel
from copilot.models import CriticVerdict, Diagnosis, Evidence

PROMPT_PATH = Path(__file__).parent / "prompts" / "critic.md"


def critic_prompt() -> str:
    return PROMPT_PATH.read_text(encoding="utf-8")


def _strip_fences(text: str) -> str:
    t = text.strip()
    m = re.match(r"^```[a-zA-Z]*\s*\n?(.*?)\n?```\s*$", t, re.DOTALL)
    return m.group(1).strip() if m else t


async def review(
    diagnosis: Diagnosis, evidence: list[Evidence], *, model: GuardedModel
) -> CriticVerdict:
    n = len(diagnosis.claims)
    if n == 0:
        return CriticVerdict(ok=False, feedback="The diagnosis is empty: it has no claims.")

    known = {e.id for e in evidence}
    failed: set[int] = set()
    for i, c in enumerate(diagnosis.claims):
        if not c.evidence_ids or any(eid not in known for eid in c.evidence_ids):
            failed.add(i)
    notes: list[str] = []
    if failed:
        notes.append(
            f"Claims {sorted(failed)} cite no evidence or an unknown evidence ID; "
            "cite existing evidence IDs or gather more evidence."
        )

    anchor_ok = True
    anchor = diagnosis.runbook_anchor
    if anchor is not None and anchor not in {e.ref for e in evidence if e.kind == "doc"}:
        anchor_ok = False
        notes.append(
            f"Runbook anchor {anchor!r} is not a source_file#anchor in the doc evidence; "
            "use a returned runbook anchor or set it to null."
        )

    remaining = [i for i in range(n) if i not in failed]
    model_ok = True
    if remaining:
        payload = {
            "claims": [
                {
                    "index": i,
                    "text": diagnosis.claims[i].text,
                    "evidence_ids": diagnosis.claims[i].evidence_ids,
                }
                for i in remaining
            ],
            "evidence": [
                {"id": e.id, "kind": e.kind, "ref": e.ref, "excerpt": e.excerpt} for e in evidence
            ],
            "runbook_anchor": anchor,
            "proposed_action": diagnosis.proposed_action.model_dump(),
        }
        out = await model.generate(critic_prompt(), json.dumps(payload))
        try:
            v = CriticVerdict.model_validate_json(_strip_fences(out))
        except ValidationError:
            return CriticVerdict(
                ok=False,
                unsupported_claim_indexes=list(range(n)),
                feedback="critic output invalid",
            )
        model_ok = v.ok
        failed |= {i for i in v.unsupported_claim_indexes if i in remaining}
        if v.feedback:
            notes.append(v.feedback)

    ok = model_ok and anchor_ok and not failed
    return CriticVerdict(ok=ok, unsupported_claim_indexes=sorted(failed), feedback=" ".join(notes))
