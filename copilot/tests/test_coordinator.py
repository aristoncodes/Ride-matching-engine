import asyncio
import json
import logging

import pytest

from copilot.agents import coordinator as co
from copilot.agents.model import ChatResult, ModelUnavailable, ToolCall
from copilot.models import CONFIDENCE_FLOOR

SECRET = "SECRET-LOG-PAYLOAD-xyz"


class FakeModel:
    """Scripted by which system prompt is calling; each role has a queue of replies."""

    def __init__(self, scripts, coord_unavailable=False):
        self.scripts = {k: list(v) for k, v in scripts.items()}
        self.coord_unavailable = coord_unavailable
        self.prompts = []

    async def generate(self, system, user):
        self.prompts.append(user)
        for role in ("metrics", "logs", "runbook", "coordinator"):
            if system == co._prompt(role):
                if role == "coordinator" and self.coord_unavailable:
                    raise ModelUnavailable("down")
                return self.scripts[role].pop(0)
        raise AssertionError("unknown system prompt")

    async def chat(self, messages, tools=None):
        """Native-tools turn: a scripted {"tool_call": ...} JSON entry becomes a tool_calls."""
        self.requests = getattr(self, "requests", [])
        self.requests.append((json.loads(json.dumps(messages)), tools))
        out = await self.generate(messages[0]["content"], messages[1]["content"])
        try:
            obj = json.loads(out)
        except ValueError:
            return ChatResult(out, [])
        tc = obj.get("tool_call") if isinstance(obj, dict) else None
        if isinstance(tc, dict):
            n = len(self.requests)
            return ChatResult("", [ToolCall(f"call_{n}", tc["name"], tc["args"])])
        return ChatResult(out, [])


class Rows:
    async def fetch(self, sql, *args):
        if "doc_chunks" in sql:
            return [
                {
                    "source_file": "docs/Runbook.md",
                    "anchor": "queue-backlog",
                    "heading": "h",
                    "content": SECRET,
                    "score": 0.9,
                }
            ]
        return [
            {
                "id": 42,
                "ts": "t",
                "service": "batcherd",
                "level": "ERROR",
                "request_id": "r",
                "event": "e",
                "error": SECRET,
            }
        ]


class Emb:
    async def embed(self, texts):
        return [[0.1, 0.2] for _ in texts]


class Resp:
    def raise_for_status(self):
        pass

    def json(self):
        return {"status": "success", "data": {"result": [{"metric": {}, "values": [[1.0, "5"]]}]}}


class Prom:
    async def get(self, url, params=None):
        return Resp()


def ev(kind, ref, excerpt="x"):
    return json.dumps([{"id": "E1", "kind": kind, "ref": ref, "excerpt": excerpt}])


def rb(ref, best=None):
    item = {"id": "E1", "kind": "doc", "ref": ref, "excerpt": "x"}
    return json.dumps({"evidence": [item], "best_anchor": best or ref})


def call(name, args):
    return json.dumps({"tool_call": {"name": name, "args": args}})


def scripts(diag):
    return {
        "metrics": [
            call("query_metric", {"name": "queue_depth", "window_minutes": 15}),
            ev("metric", "queue_depth@15m", "queue_depth=5 at ts 1.0"),
        ],
        "logs": [
            call("query_logs", {"name": "errors_by_service", "params": {"minutes": 10}}),
            ev("log", "42"),
        ],
        "runbook": [
            call("search_runbook", {"query": "queue rising"}),
            rb("docs/Runbook.md#queue-backlog"),
        ],
        "coordinator": diag,
    }


def diag(conf=0.9, anchor="docs/Runbook.md#queue-backlog", action="scale"):
    return json.dumps(
        {
            "summary": "s",
            "root_cause": "r",
            "component": "batcherd",
            "runbook_anchor": anchor,
            "confidence": conf,
            "needs_human": False,
            "claims": [{"text": "a", "evidence_ids": ["E1", "E2", "E3", "E9"]}],
            "evidence": [],
            "proposed_action": {
                "kind": action,
                "deployment": "batcherd",
                "replicas": 2,
                "reason": "x",
            },
        }
    )


async def run(model):
    return await co.investigate(
        {"alert": "QueueDepth"},
        model=model,
        pool_ro=Rows(),
        prom_client=Prom(),
        prom_base_url="http://p",
        embedder=Emb(),
        critic=False,
    )


def test_happy_path_ids_unique_and_claims_valid():
    r = asyncio.run(run(FakeModel(scripts(["```json\n" + diag() + "\n```"]))))
    assert r.diagnosis is not None
    assert [e.id for e in r.evidence] == ["E1", "E2", "E3"]
    ids = {e.id for e in r.evidence}
    for c in r.diagnosis.claims:
        assert c.evidence_ids and set(c.evidence_ids) <= ids
    assert r.diagnosis.runbook_anchor == "docs/Runbook.md#queue-backlog"


def test_unknown_anchor_dropped():
    r = asyncio.run(run(FakeModel(scripts([diag(anchor="docs/Runbook.md#invented")]))))
    assert r.diagnosis.runbook_anchor is None


def test_low_confidence_needs_human_no_action():
    r = asyncio.run(run(FakeModel(scripts([diag(conf=CONFIDENCE_FLOOR - 0.1)]))))
    assert r.diagnosis.needs_human is True
    assert r.diagnosis.proposed_action.kind == "none"


def test_invalid_json_twice_returns_none():
    r = asyncio.run(run(FakeModel(scripts(["not json", "{}"]))))
    assert r.diagnosis is None
    assert len(r.evidence) == 3


def test_retry_succeeds_second_time():
    r = asyncio.run(run(FakeModel(scripts(["nope", diag()]))))
    assert r.diagnosis is not None


def test_model_unavailable_returns_evidence():
    r = asyncio.run(run(FakeModel(scripts([]), coord_unavailable=True)))
    assert r.diagnosis is None
    assert len(r.evidence) == 3


def test_trace_has_no_prompt_text(caplog):
    caplog.set_level(logging.DEBUG)
    r = asyncio.run(run(FakeModel(scripts([diag()]))))
    assert r.trace
    for t in r.trace:
        assert set(t) == {"step", "agent", "tool", "ms", "status"}
    blob = json.dumps(r.trace) + caplog.text
    assert SECRET not in blob and "Investigate" not in blob


def test_tool_error_does_not_raise():
    s = scripts([diag()])
    s["metrics"][0] = call("query_metric", {"name": "bogus"})
    r = asyncio.run(run(FakeModel(s)))
    assert any(t["status"] == "error" for t in r.trace)


@pytest.mark.parametrize("text", ['```json\n{"a":1}\n```', '{"a":1}'])
def test_strip_fences(text):
    assert json.loads(co._strip_fences(text)) == {"a": 1}


def _evidence_kinds(r):
    return [e.kind for e in r.evidence]


def test_ungrounded_evidence_dropped():
    s = scripts([diag()])
    s["metrics"][1] = ev("metric", "queue_depth@15m", "queue_depth=999")  # value not returned
    s["logs"][1] = ev("log", "row-invented")
    s["runbook"][1] = rb("docs/Runbook.md#invented")
    r = asyncio.run(run(FakeModel(s)))
    assert r.evidence == []


def test_unqueried_metric_and_no_tool_call_give_no_evidence():
    s = scripts([diag()])
    s["metrics"] = [ev("metric", "queue_depth@15m", "5")]  # never called the tool
    s["logs"][0] = call("query_logs", {"name": "bogus", "params": {}})  # tool errors
    r = asyncio.run(run(FakeModel(s)))
    assert _evidence_kinds(r) == ["doc"]


def test_runbook_prompt_shape_gives_doc_evidence_old_shape_none():
    r = asyncio.run(run(FakeModel(scripts([diag()]))))
    assert _evidence_kinds(r) == ["metric", "log", "doc"]
    s = scripts([diag()])
    s["runbook"][1] = ev("doc", "docs/Runbook.md#queue-backlog")  # old list shape
    r = asyncio.run(run(FakeModel(s)))
    assert "doc" not in _evidence_kinds(r)


def test_null_anchor_chunk_not_allowed():
    class NullAnchorRows(Rows):
        async def fetch(self, sql, *args):
            if "doc_chunks" in sql:
                return [
                    {
                        "source_file": "docs/A.md",
                        "anchor": None,
                        "heading": "h",
                        "content": "c",
                        "score": 0.5,
                    }
                ]
            return await super().fetch(sql, *args)

    s = scripts([diag(anchor="docs/A.md#None")])
    s["runbook"][1] = rb("docs/A.md#None")

    async def go():
        return await co.investigate(
            {"alert": "x"},
            model=FakeModel(s),
            pool_ro=NullAnchorRows(),
            prom_client=Prom(),
            prom_base_url="http://p",
            embedder=Emb(),
            critic=False,
        )

    r = asyncio.run(go())
    assert "doc" not in _evidence_kinds(r)
    assert r.diagnosis.runbook_anchor is None


def test_needs_human_forces_no_action_even_with_high_confidence():
    d = json.loads(diag(conf=0.95))
    d["needs_human"] = True
    r = asyncio.run(run(FakeModel(scripts([json.dumps(d)]))))
    assert r.diagnosis.needs_human is True
    assert r.diagnosis.proposed_action.kind == "none"


def test_partial_evidence_on_timeout():
    class SlowLogsModel(FakeModel):
        async def chat(self, messages, tools=None):
            if messages[0]["content"] == co._prompt("logs"):
                await asyncio.sleep(5)
            return await super().chat(messages, tools)

    async def go():
        task = asyncio.ensure_future(run(SlowLogsModel(scripts([diag()]))))
        await asyncio.sleep(0.3)
        task.cancel()
        return await task

    r = asyncio.run(go())
    assert r.diagnosis is None
    assert _evidence_kinds(r) == ["metric", "doc"]
    assert [e.id for e in r.evidence] == ["E1", "E2"]


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), float("-inf")])
def test_has_number_non_finite_is_false_and_never_raises(bad):
    assert co._has_number("queue_depth=nan", bad) is False
    assert co._has_number("value=inf", bad) is False


def test_non_finite_metric_values_do_not_ground_evidence():
    role = co.RoleRunner.__new__(co.RoleRunner) if hasattr(co, "RoleRunner") else None
    if role is None:
        pytest.skip("RoleRunner not exposed")
    role.role = "metrics"
    role.metric_values = {"solve_p99@15m": [float("nan"), float("inf")]}
    item = {"kind": "metric", "ref": "solve_p99@15m", "excerpt": "p99=nan then inf"}
    assert role._grounded(item) == "excerpt lacks a returned value"


def test_parse_qwen_tool_call_tag_with_prose():
    text = (
        "Let me check the queue.\n<tool_call>\n"
        '{"tool": "query_metric", "args": {"metric": "queue_depth", "window_minutes": 15}}\n'
        "</tool_call>\nThanks."
    )
    assert co._parse_json(text) == {
        "tool_call": {
            "name": "query_metric",
            "args": {"metric": "queue_depth", "window_minutes": 15},
        }
    }


def test_parse_think_block_then_final_evidence():
    text = '<think>hmm {"x": 1}</think>\n{"evidence": [{"kind": "metric", "ref": "r"}]}'
    assert co._parse_json(text) == {"evidence": [{"kind": "metric", "ref": "r"}]}


def test_parse_old_shape_unchanged():
    obj = {"tool_call": {"name": "query_logs", "args": {"a": 1}}}
    assert co._parse_json(json.dumps(obj)) == obj


def test_parse_arguments_normalised_to_args():
    text = 'ok {"tool_call": {"name": "t", "arguments": {"q": 1}}}'
    assert co._parse_json(text) == {"tool_call": {"name": "t", "args": {"q": 1}}}


def test_parse_non_json_returns_none():
    assert co._parse_json("no json here") is None
    assert co._parse_json("broken { not json") is None


def test_qwen_style_tool_call_end_to_end():
    s = scripts(json.dumps(json.loads(diag())))
    qwen = (
        "<think>plan</think>I'll query.\n<tool_call>\n"
        '{"tool": "query_metric", "args": {"name": "queue_depth", "window_minutes": 15}}\n'
        "</tool_call>"
    )
    s["metrics"][0] = qwen
    s["metrics"].insert(0, qwen)  # first turn is chat (no tool_calls, text tag) -> text fallback
    r = asyncio.run(run(FakeModel(s)))
    assert "metric" in _evidence_kinds(r)


def test_native_loop_sends_tool_message_back_with_matching_id():
    m = FakeModel(scripts([diag()]))
    r = asyncio.run(run(m))
    assert _evidence_kinds(r) == ["metric", "log", "doc"]
    reqs = [q for q in m.requests if q[0][0]["content"] == co._prompt("metrics")]
    assert len(reqs) == 2
    assert reqs[0][1] == co.TOOL_SCHEMAS["metrics"]
    msgs = reqs[1][0]
    asst = next(x for x in msgs if x["role"] == "assistant")
    tool = next(x for x in msgs if x["role"] == "tool")
    assert tool["tool_call_id"] == asst["tool_calls"][0]["id"]
    assert json.loads(tool["content"])[0]["value"] == 5.0


def test_tool_schemas_have_enums_and_coordinator_gets_no_tools():
    fn = co.TOOL_SCHEMAS["metrics"][0]["function"]
    assert "queue_depth" in fn["parameters"]["properties"]["name"]["enum"]
    m = FakeModel(scripts([diag()]))
    asyncio.run(run(m))
    coord = [q for q in m.requests if q[0][0]["content"] == co._prompt("coordinator")]
    assert coord == []  # coordinator uses generate(), not chat with tools


def test_bad_arguments_go_back_as_error_not_raise():
    class BadArgs(FakeModel):
        async def chat(self, messages, tools=None):
            if messages[0]["content"] == co._prompt("metrics") and len(messages) == 2:
                return ChatResult(
                    "", [ToolCall("c1", "query_metric", {}, "arguments were not valid JSON")]
                )
            return await super().chat(messages, tools)

    s = scripts([diag()])
    s["metrics"] = [ev("metric", "queue_depth@15m", "queue_depth=5")]
    r = asyncio.run(run(BadArgs(s)))
    assert "metric" not in _evidence_kinds(r)


@pytest.mark.parametrize(
    "raw,kind,dep",
    [
        ("scale batcherd", "scale", "batcherd"),
        ("  Scale   BATCHERD ", "scale", "batcherd"),
        ("none", "none", "none"),
        ("", "none", "none"),
        ("restart_pods matching-engine", "restart_pods", "matching-engine"),
        ("restart batcherd", "restart_pods", "batcherd"),
        ("reboot the world", "none", "none"),
        ("scale redis", "none", "none"),
        (None, "none", "none"),
    ],
)
def test_normalise_action_strings(raw, kind, dep):
    out = co._normalise_action(raw)
    assert out["kind"] == kind and out["deployment"] == dep
    assert out["replicas"] is None and out["reason"] == ""


def test_normalise_action_dict_unchanged():
    d = {"kind": "scale", "deployment": "batcherd", "replicas": 3, "reason": "x"}
    assert co._normalise_action(d) is d


def test_string_proposed_action_yields_valid_diagnosis():
    data = json.loads(diag())
    data["proposed_action"] = "scale batcherd"
    d = co._parse_diagnosis(json.dumps(data))
    assert d.proposed_action.kind == "scale"
    assert d.proposed_action.deployment == "batcherd"
