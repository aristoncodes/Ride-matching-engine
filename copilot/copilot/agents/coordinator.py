"""Coordinator and the metrics, logs and runbook specialists (ticket 2.2).

ADK note: google-adk installs and imports, but it is not used. ModelClient is a text-only
`generate(system, user) -> str` interface (so GuardedModel can cache, retry and break), and ADK's
LlmAgent/Runner needs a native LLM with function calling. A plain-Python loop keeps the same
structure (coordinator + three specialists, one function tool each) and is testable offline.

Tool-call protocol (text, since ModelClient returns a string): a specialist replies with
{"tool_call": {"name": "<tool>", "args": {...}}} to call its tool, or with its final JSON.

Evidence IDs: each specialist numbers its own evidence E1.. so they collide. After the
specialists return, ALL evidence is renumbered E1..En in role order (metrics, logs, runbook)
and the coordinator only ever sees the renumbered IDs, so its claims are already correct.
Claims citing an ID that does not exist are stripped of it (the critic then rejects them).

Tokens: GuardedModel.usage_total() accumulates real usage per investigation.
Trace entries hold step, agent, tool, ms, status only; never prompt or model text.
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from pydantic import ValidationError

from copilot.agents.critic import review
from copilot.agents.model import GuardedModel, ModelUnavailable
from copilot.agents.tools.logs_tools import query_logs
from copilot.agents.tools.metrics_tools import query_metric
from copilot.agents.tools.runbook_tools import search_runbook
from copilot.models import CONFIDENCE_FLOOR, Diagnosis, Evidence, ProposedAction
from copilot.obs import metrics
from copilot.obs.tracing import emit_step, get_tracer

log = logging.getLogger(__name__)

PROMPTS = Path(__file__).parent / "prompts"
MAX_TOOL_CALLS = 4
MAX_RESULT_CHARS = 6000
ROLES = ("metrics", "logs", "runbook")
_ROLE_TOOL = {"metrics": "query_metric", "logs": "query_logs", "runbook": "search_runbook"}

TOOL_PROTOCOL = (
    'To call your tool reply with only {"tool_call": {"name": "<tool>", "args": {...}}}. '
    "Otherwise reply with your final JSON only."
)
TOOL_ARGS = {
    "metrics": 'query_metric args: {"name": <metric name>, "window_minutes": int}',
    "logs": 'query_logs args: {"name": <query name>, "params": {...}}',
    "runbook": 'search_runbook args: {"query": str, "k": int}',
}


@dataclass
class InvestigationResult:
    diagnosis: Diagnosis | None
    evidence: list[Evidence] = field(default_factory=list)
    trace: list[dict] = field(default_factory=list)
    tokens_in: int = 0
    tokens_out: int = 0
    critic_enabled: bool = True
    model: str = ""


def _prompt(name: str) -> str:
    return (PROMPTS / f"{name}.md").read_text(encoding="utf-8")


def _strip_fences(text: str) -> str:
    t = text.strip()
    m = re.match(r"^```[a-zA-Z]*\s*\n?(.*?)\n?```\s*$", t, re.DOTALL)
    return m.group(1).strip() if m else t


def _parse_json(text: str) -> Any:
    try:
        return json.loads(_strip_fences(text))
    except ValueError:
        return None


def _dump(obj: Any) -> str:
    s = json.dumps(obj, default=str)
    return s if len(s) <= MAX_RESULT_CHARS else s[:MAX_RESULT_CHARS] + "...[truncated]"


class _Specialist:
    def __init__(self, role: str, ctx: dict, trace: list[dict]) -> None:
        self.role = role
        self.ctx = ctx
        self.trace = trace
        self.anchors: set[str] = set()
        self.metric_values: dict[str, list[float]] = {}  # "name@15m" -> returned values
        self.log_ids: set[str] = set()
        self.raw_results: list[Any] = []
        self.unavailable = False

    def _step(self, tool: str, t0: float, status: str) -> None:
        ms = int((time.monotonic() - t0) * 1000)
        self.trace.append(
            {
                "step": len(self.trace) + 1,
                "agent": self.role,
                "tool": tool,
                "ms": ms,
                "status": status,
            }
        )
        emit_step(self.role, tool, status, ms)
        if status == "error":
            metrics.tool_error(tool)

    async def _run_tool(self, name: str, args: dict) -> Any:
        c = self.ctx
        if self.role == "metrics" and name == "query_metric":
            window = args.get("window_minutes", 15)
            pts = await query_metric(c["prom_client"], c["prom_base_url"], args.get("name"), window)
            vals = [float(p["value"]) for p in pts if isinstance(p, dict) and "value" in p]
            self.metric_values.setdefault(f"{args.get('name')}@{window}m", []).extend(vals)
            return pts
        if self.role == "logs" and name == "query_logs":
            rows = await query_logs(c["pool_ro"], args.get("name"), args.get("params", {}))
            self.log_ids.update(str(r["id"]) for r in rows if r.get("id") is not None)
            return rows
        if self.role == "runbook" and name == "search_runbook":
            rows = await search_runbook(
                c["pool_ro"], c["embedder"], args.get("query"), args.get("k", 5)
            )
            for r in rows:
                if not r.get("anchor"):
                    continue  # chunks without an anchor cannot be cited
                self.anchors.add(f"{r['source_file']}#{r['anchor']}")
            return rows
        raise ValueError(f"unknown tool {name!r} for {self.role}")

    async def run(self, task: str, model: GuardedModel) -> list[Evidence]:
        t0 = time.monotonic()
        with get_tracer().start_as_current_span(f"{self.role}.run") as span:
            span.set_attribute("agent", self.role)
            span.set_attribute("tool", _ROLE_TOOL[self.role])
            out = await self._run(task, model)
            span.set_attribute("duration_ms", int((time.monotonic() - t0) * 1000))
            span.set_attribute("status", "unavailable" if self.unavailable else "ok")
            return out

    async def _run(self, task: str, model: GuardedModel) -> list[Evidence]:
        system = _prompt(self.role)
        user = f"{task}\n\n{TOOL_PROTOCOL}\n{TOOL_ARGS[self.role]}"
        calls = 0
        final: Any = None
        for _ in range(MAX_TOOL_CALLS + 1):
            t0 = time.monotonic()
            try:
                out = await model.generate(system, user)
            except ModelUnavailable:
                self._step("model", t0, "unavailable")
                self.unavailable = True
                return []
            self._step("model", t0, "ok")
            parsed = _parse_json(out)
            call = parsed.get("tool_call") if isinstance(parsed, dict) else None
            if not isinstance(call, dict):
                final = parsed
                break
            if calls >= MAX_TOOL_CALLS:
                user += "\n\nTool budget exhausted. Reply with your final JSON now."
                continue
            calls += 1
            name = str(call.get("name"))
            args = call.get("args") if isinstance(call.get("args"), dict) else {}
            t0 = time.monotonic()
            try:
                result: Any = await self._run_tool(name, args)
                self._step(name, t0, "ok")
                self.raw_results.append(result)
            except Exception as exc:  # tool errors go back to the model, never raise
                self._step(name, t0, "error")
                result = {"error": type(exc).__name__, "detail": str(exc)[:200]}
            user += f"\n\nTool result for {name}:\n{_dump(result)}"
        return self._evidence(final)

    def _grounded(self, it: dict) -> str | None:
        """Return None if the item is grounded in this run's tool results, else the drop reason."""
        kind, ref, excerpt = it.get("kind"), it.get("ref"), it.get("excerpt")
        if not isinstance(ref, str) or not isinstance(excerpt, str):
            return "bad ref or excerpt"
        if self.role == "metrics":
            if kind != "metric":
                return "wrong kind"
            if ref not in self.metric_values:
                return "metric not queried successfully"
            if not any(_has_number(excerpt, v) for v in self.metric_values[ref]):
                return "excerpt lacks a returned value"
        elif self.role == "logs":
            if kind != "log":
                return "wrong kind"
            if ref not in self.log_ids:
                return "log id not returned"
        else:
            if kind != "doc":
                return "wrong kind"
            if ref not in self.anchors:
                return "anchor not returned"
        return None

    def _evidence(self, final: Any) -> list[Evidence]:
        items: list[Any] = []
        if self.role == "runbook":
            # Binding shape: {"evidence": [...], "best_anchor": str | null}
            ev = final.get("evidence") if isinstance(final, dict) else None
            items = ev if isinstance(ev, list) else []
        elif isinstance(final, list):
            items = final
        elif isinstance(final, dict) and isinstance(final.get("evidence"), list):
            items = final["evidence"]
        out: list[Evidence] = []
        for it in items:
            if not isinstance(it, dict):
                continue
            reason = self._grounded(it)
            if reason is not None:
                log.debug("dropped %s evidence: %s", self.role, reason)
                continue
            it = {**it, "id": f"E{len(out) + 1}"}  # local numbering; renumbered globally on merge
            it["excerpt"] = it["excerpt"][:500]
            try:
                out.append(Evidence.model_validate(it))
            except ValidationError:
                log.debug("dropped %s evidence: invalid", self.role)
        return out


def _has_number(excerpt: str, value: float) -> bool:
    forms = {repr(value), f"{value:g}", f"{value:.6g}"}
    if value == int(value):
        forms.add(str(int(value)))
    return any(re.search(r"(?<![\d.])" + re.escape(f) + r"(?![\d])", excerpt) for f in forms)


def _merge(per_role: dict[str, list[Evidence]]) -> list[Evidence]:
    merged: list[Evidence] = []
    for role in ROLES:
        for ev in per_role.get(role, []):
            merged.append(ev.model_copy(update={"id": f"E{len(merged) + 1}"}))
    return merged


def _finalize(d: Diagnosis, evidence: list[Evidence], anchors: set[str]) -> Diagnosis:
    valid = {e.id for e in evidence}
    claims = [
        c.model_copy(update={"evidence_ids": [i for i in c.evidence_ids if i in valid]})
        for c in d.claims
    ]
    update: dict[str, Any] = {"claims": claims, "evidence": evidence}
    if d.runbook_anchor is not None and d.runbook_anchor not in anchors:
        update["runbook_anchor"] = None
    if d.confidence < CONFIDENCE_FLOOR:
        update["needs_human"] = True
    if update.get("needs_human", d.needs_human):
        update["proposed_action"] = ProposedAction(kind="none", deployment="none")
    return d.model_copy(update=update)


async def investigate(
    alert_or_question: dict,
    *,
    model: GuardedModel,
    pool_ro: Any,
    prom_client: Any,
    prom_base_url: str,
    embedder: Any,
    max_rounds: int = 3,
    critic: bool = True,
) -> InvestigationResult:
    """Run the specialists in parallel, merge evidence, ask the coordinator for a Diagnosis.

    max_rounds caps coordinator attempts (the first plus retries with the parse error), with
    the spec's single retry as the default behaviour: attempts = min(max_rounds, 2).
    """
    ctx = {
        "pool_ro": pool_ro,
        "prom_client": prom_client,
        "prom_base_url": prom_base_url,
        "embedder": embedder,
    }
    reset = getattr(model, "reset_usage", None)
    if reset is not None:
        reset()
    trace: list[dict] = []
    task = "Investigate this alert or question:\n" + json.dumps(alert_or_question, default=str)
    specialists = {r: _Specialist(r, ctx, trace) for r in ROLES}
    per_role: dict[str, list[Evidence]] = {}  # shared; filled as each specialist finishes

    async def _specialist(role: str) -> None:
        per_role[role] = await specialists[role].run(task, model)

    async def _work() -> InvestigationResult:
        await asyncio.gather(*(_specialist(r) for r in ROLES))
        return await _coordinate(
            task, per_role, specialists, model, trace, max_rounds=max_rounds, critic=critic
        )

    try:
        result = await _work()
    except asyncio.CancelledError:
        # Overall deadline hit: hand back what the specialists had finished.
        result = InvestigationResult(None, _merge(per_role), trace, critic_enabled=critic)
    usage_total = getattr(model, "usage_total", None)
    if usage_total is not None:
        u = usage_total()
        result.tokens_in, result.tokens_out, result.model = u.tokens_in, u.tokens_out, u.model
    return result


async def _coordinate(
    task: str,
    per_role: dict[str, list[Evidence]],
    specialists: dict[str, _Specialist],
    model: GuardedModel,
    trace: list[dict],
    *,
    max_rounds: int,
    critic: bool,
) -> InvestigationResult:
    evidence = _merge(per_role)
    anchors = set().union(*(s.anchors for s in specialists.values()))

    if any(s.unavailable for s in specialists.values()):
        return InvestigationResult(None, evidence, trace, critic_enabled=critic)

    system = _prompt("coordinator")
    user = (
        f"{task}\n\nEvidence returned by the specialists (use only these IDs):\n"
        + json.dumps([e.model_dump() for e in evidence])
        + "\n\nRunbook anchors returned: "
        + json.dumps(sorted(anchors))
    )
    diagnosis: Diagnosis | None = None
    for _attempt in range(max(1, min(max_rounds, 2))):
        t0 = time.monotonic()
        try:
            out = await model.generate(system, user)
        except ModelUnavailable:
            trace.append(_coord_step(trace, t0, "unavailable"))
            return InvestigationResult(None, evidence, trace, critic_enabled=critic)
        try:
            diagnosis = Diagnosis.model_validate_json(_strip_fences(out))
        except ValidationError as exc:
            trace.append(_coord_step(trace, t0, "invalid"))
            user += (
                "\n\nYour previous reply failed validation. Return only corrected JSON.\n"
                f"Error: {str(exc)[:800]}"
            )
            continue
        trace.append(_coord_step(trace, t0, "ok"))
        break
    if diagnosis is not None:
        diagnosis = _finalize(diagnosis, evidence, anchors)
        if critic:
            try:
                diagnosis = await _criticize(
                    diagnosis, evidence, anchors, model, system, user, trace
                )
            except ModelUnavailable:
                return InvestigationResult(None, evidence, trace, critic_enabled=critic)
    return InvestigationResult(diagnosis, evidence, trace, critic_enabled=critic)


def _critic_step(trace: list[dict], t0: float, status: str) -> None:
    ms = int((time.monotonic() - t0) * 1000)
    trace.append(
        {"step": len(trace) + 1, "agent": "critic", "tool": None, "ms": ms, "status": status}
    )
    emit_step("critic", None, status, ms)


async def _review(d: Diagnosis, evidence: list[Evidence], model: GuardedModel, trace: list[dict]):
    t0 = time.monotonic()
    try:
        verdict = await review(d, evidence, model=model)
    except ModelUnavailable:
        _critic_step(trace, t0, "unavailable")
        raise
    _critic_step(trace, t0, "pass" if verdict.ok else "fail")
    return verdict


def _escalate(d: Diagnosis, failing: list[int]) -> Diagnosis:
    drop = set(failing)
    return d.model_copy(
        update={
            "claims": [c for i, c in enumerate(d.claims) if i not in drop],
            "needs_human": True,
            "proposed_action": ProposedAction(kind="none", deployment="none"),
        }
    )


async def _criticize(
    draft: Diagnosis,
    evidence: list[Evidence],
    anchors: set[str],
    model: GuardedModel,
    system: str,
    user: str,
    trace: list[dict],
) -> Diagnosis:
    verdict = await _review(draft, evidence, model, trace)
    if verdict.ok:
        return draft
    # AG-5: send it back once.
    revision = (
        user
        + "\n\nYour previous draft:\n"
        + draft.model_dump_json()
        + "\n\nThe critic rejected it. Feedback: "
        + verdict.feedback
        + "\nFailing claim indexes: "
        + json.dumps(verdict.unsupported_claim_indexes)
        + "\nFix only those claims or gather more evidence. Return the full corrected JSON only."
    )
    t0 = time.monotonic()
    try:
        out = await model.generate(system, revision)
    except ModelUnavailable:
        trace.append(_coord_step(trace, t0, "unavailable"))
        raise
    try:
        revised = Diagnosis.model_validate_json(_strip_fences(out))
    except ValidationError:
        trace.append(_coord_step(trace, t0, "invalid"))
        return _escalate(draft, verdict.unsupported_claim_indexes)
    trace.append(_coord_step(trace, t0, "ok"))
    revised = _finalize(revised, evidence, anchors)
    second = await _review(revised, evidence, model, trace)
    if second.ok:
        return revised
    return _escalate(revised, second.unsupported_claim_indexes)


def _coord_step(trace: list[dict], t0: float, status: str) -> dict:
    ms = int((time.monotonic() - t0) * 1000)
    emit_step("coordinator", "model", status, ms)
    return {
        "step": len(trace) + 1,
        "agent": "coordinator",
        "tool": "model",
        "ms": ms,
        "status": status,
    }
