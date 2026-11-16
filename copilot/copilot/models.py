"""Contracts shared by the agents, the critic, the API and the action runner.

These are the output schema from PRD AG-6 and the action limits from AC-1.
Changing a field here changes what the API returns, so change it deliberately.
"""

from typing import Literal

from pydantic import BaseModel, Field

# Below this confidence the copilot does not propose an action and says
# "needs human" instead of guessing. PRD Safety requirement.
CONFIDENCE_FLOOR = 0.6

Component = Literal["matching-engine", "batcherd", "ingestd", "requestd", "redis", "unknown"]


class Evidence(BaseModel):
    id: str = Field(pattern=r"^E\d+$")
    kind: Literal["metric", "log", "doc"]
    ref: str = Field(description="PromQL window, log row ID, or 'source_file#anchor'")
    excerpt: str = Field(max_length=500)


class Claim(BaseModel):
    text: str
    # An empty list is an unsupported claim; the critic rejects it.
    evidence_ids: list[str] = Field(default_factory=list)


class ProposedAction(BaseModel):
    kind: Literal["restart_pods", "scale", "none"]
    deployment: Literal["matching-engine", "batcherd", "none"]
    replicas: int | None = None
    reason: str = ""


class Diagnosis(BaseModel):
    summary: str
    root_cause: str
    component: Component
    runbook_anchor: str | None = None
    confidence: float = Field(ge=0.0, le=1.0)
    needs_human: bool = False
    claims: list[Claim] = Field(default_factory=list)
    evidence: list[Evidence] = Field(default_factory=list)
    proposed_action: ProposedAction = ProposedAction(kind="none", deployment="none")


class CriticVerdict(BaseModel):
    ok: bool
    unsupported_claim_indexes: list[int] = Field(default_factory=list)
    feedback: str = ""


# Action limits (AC-1). Scale bounds are inclusive.
SCALE_LIMITS: dict[str, tuple[int, int]] = {
    "matching-engine": (1, 4),
    "batcherd": (1, 3),
}
