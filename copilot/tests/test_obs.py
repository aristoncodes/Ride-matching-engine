"""Observability and cost (ticket 3.2). Offline."""

import asyncio

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from test_api import AUTH, BODY, FakePool, diagnosis, env  # noqa: F401
from test_coordinator import FakeModel, diag, run, scripts

from copilot import cost
from copilot.agents.coordinator import InvestigationResult
from copilot.agents.model import GuardedModel, InMemoryCache, ModelUsage, record_usage

EXPORTER = InMemorySpanExporter()
_provider = TracerProvider()
_provider.add_span_processor(SimpleSpanProcessor(EXPORTER))
trace.set_tracer_provider(_provider)


class UsageInner:
    def __init__(self):
        self.n = 0

    async def generate(self, system, user):
        self.n += 1
        record_usage(ModelUsage(100, 10, "openai/gpt-oss-120b"))
        return f"answer-{self.n}"


def _guarded(inner):
    return GuardedModel(inner, cache=InMemoryCache())


def test_tokens_accumulate_and_reset():
    async def go():
        m = _guarded(UsageInner())
        m.reset_usage()
        await m.generate("s", "a")
        await m.generate("s", "b")
        await m.generate("s", "a")  # cache hit: no new usage
        assert m.usage_total() == ModelUsage(200, 20, "openai/gpt-oss-120b")
        m.reset_usage()
        assert m.usage_total().tokens_in == 0
        await m.generate("s", "c")
        assert m.usage_total() == ModelUsage(100, 10, "openai/gpt-oss-120b")

    asyncio.run(go())


def test_tokens_shared_across_gathered_tasks():
    async def go():
        m = _guarded(UsageInner())
        m.reset_usage()
        await asyncio.gather(*(m.generate("s", str(i)) for i in range(3)))
        assert m.usage_total().tokens_in == 300

    asyncio.run(go())


def test_cost_arithmetic(monkeypatch):
    monkeypatch.setitem(cost.PRICES_USD_PER_M, "m-test", (1.0, 4.0))
    assert cost.cost_usd("m-test", 1_000_000, 500_000) == pytest.approx(3.0)
    assert cost.cost_usd("m-test", 0, 0) == 0
    # unknown model falls back to the default row
    fin, fout = cost.FALLBACK_PRICE
    assert cost.cost_usd("nope", 1_000_000, 1_000_000) == pytest.approx(fin + fout)


@pytest.mark.asyncio
async def test_metrics_open_and_incidents_get_tokens_and_cost(env):  # noqa: F811
    _, c, pool, state, _ = env
    r = await c.get("/metrics")  # no bearer token
    assert r.status_code == 200
    assert "copilot_diagnoses_total" in r.text
    state["result"] = InvestigationResult(
        diagnosis(), tokens_in=1_000_000, tokens_out=100_000, model="openai/gpt-oss-120b"
    )
    assert (await c.post("/diagnose", json=BODY, headers=AUTH)).status_code == 200
    args = pool.updates()[0][1]
    fin, fout = cost.PRICES_USD_PER_M["openai/gpt-oss-120b"]
    assert args[4] == 1_000_000 and args[5] == 100_000
    assert args[8] == pytest.approx(fin + 0.1 * fout)
    text = (await c.get("/metrics")).text
    assert 'copilot_model_tokens_total{direction="in"}' in text
    assert "copilot_diagnosis_seconds_bucket" in text
    assert "copilot_model_cost_usd_total" in text


def test_spans_per_step_without_prompt_text():
    EXPORTER.clear()
    m = FakeModel(scripts([diag()]))
    asyncio.run(run(m))
    spans = EXPORTER.get_finished_spans()
    agents = {s.attributes.get("agent") for s in spans}
    assert {"metrics", "logs", "runbook", "coordinator"} <= agents
    assert any(s.name == "metrics.run" for s in spans)
    for s in spans:
        assert set(s.attributes) <= {"agent", "tool", "duration_ms", "status"}
        for v in s.attributes.values():
            assert isinstance(v, (int, str))
            assert "Investigate" not in str(v) and "alert" not in str(v).lower()
    prompts = " ".join(m.prompts)
    assert prompts  # prompts existed, yet none leaked into spans


def test_model_call_records_latency_and_tokens_per_second():
    from copilot.obs import metrics

    before = metrics.REGISTRY.get_sample_value("copilot_model_output_tokens_per_second_count") or 0
    before_calls = metrics.REGISTRY.get_sample_value("copilot_model_call_seconds_count") or 0

    async def go():
        m = _guarded(UsageInner())
        m.reset_usage()
        await m.generate("s", "tps-probe-1")
        await m.generate("s", "tps-probe-1")  # cache hit: no model call, no new sample

    asyncio.run(go())
    after = metrics.REGISTRY.get_sample_value("copilot_model_output_tokens_per_second_count")
    after_calls = metrics.REGISTRY.get_sample_value("copilot_model_call_seconds_count")
    assert after - before == 1
    assert after_calls - before_calls == 1


def test_configure_tracing_is_noop_without_env_and_keeps_existing_provider(monkeypatch):
    from copilot.obs import tracing

    provider = trace.get_tracer_provider()
    monkeypatch.delenv("OTEL_TRACES_EXPORTER", raising=False)
    tracing.configure_tracing()
    assert trace.get_tracer_provider() is provider
    # A provider is already set in this process, so console mode also must not replace it.
    monkeypatch.setenv("OTEL_TRACES_EXPORTER", "console")
    tracing.configure_tracing()
    assert trace.get_tracer_provider() is provider


def test_diagnosis_cost_histogram_and_price_rows_exist():
    from copilot import cost
    from copilot.obs import metrics

    assert cost.cost_usd("copilot-qwen2.5-3b", 1000, 1000) == 0.0
    assert cost.cost_usd("gemini-2.5-flash", 1000, 1000) == 0.0
    metrics.DIAGNOSIS_COST_USD.observe(0.0)
    assert metrics.REGISTRY.get_sample_value("copilot_diagnosis_cost_usd_count") >= 1
