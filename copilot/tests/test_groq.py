"""GroqClient offline tests: httpx.MockTransport, no network."""

from __future__ import annotations

import asyncio
import json

import httpx
import pytest

from copilot.agents import model as model_mod
from copilot.agents.groq import GroqClient
from copilot.agents.model import ModelUsage

FAKE_KEY = "unit-test-placeholder"

CANNED = {
    "choices": [{"message": {"role": "assistant", "content": "hello"}}],
    "usage": {"prompt_tokens": 12, "completion_tokens": 3},
}


def test_parses_response_and_records_usage(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    monkeypatch.delenv("GROQ_MODEL", raising=False)
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers["authorization"]
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json=CANNED)

    client = GroqClient(transport=httpx.MockTransport(handler))
    sink: list[ModelUsage] = []
    model_mod._sink_var.set(sink)
    out = asyncio.run(client.generate("sys", "usr"))

    assert out == "hello"
    assert seen["auth"] == f"Bearer {FAKE_KEY}"
    assert seen["body"]["model"] == "openai/gpt-oss-120b"
    assert seen["body"]["temperature"] == 0
    assert [m["role"] for m in seen["body"]["messages"]] == ["system", "user"]
    assert sink == [ModelUsage(12, 3, "openai/gpt-oss-120b")]
    assert client.last_usage == sink[0]


@pytest.mark.parametrize("status", [429, 503])
def test_rate_limit_and_server_errors_raise(monkeypatch: pytest.MonkeyPatch, status: int) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    client = GroqClient(transport=httpx.MockTransport(lambda r: httpx.Response(status)))
    with pytest.raises(httpx.HTTPStatusError) as ei:
        asyncio.run(client.generate("s", "u"))
    assert FAKE_KEY not in str(ei.value)


def test_missing_key_error_has_no_key_text(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    client = GroqClient(transport=httpx.MockTransport(lambda r: httpx.Response(200, json=CANNED)))
    with pytest.raises(RuntimeError) as ei:
        asyncio.run(client.generate("s", "u"))
    msg = str(ei.value)
    assert "GROQ_API_KEY" in msg
    assert "gsk_" not in msg and "Bearer" not in msg
