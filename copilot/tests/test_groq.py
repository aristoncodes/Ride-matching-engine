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
    assert seen["body"]["model"] == "qwen/qwen3.8-27b"
    assert seen["body"]["temperature"] == 0
    assert [m["role"] for m in seen["body"]["messages"]] == ["system", "user"]
    assert sink == [ModelUsage(12, 3, "qwen/qwen3.8-27b")]
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


TOOLS = [
    {"type": "function", "function": {"name": "query_metric", "parameters": {"type": "object"}}}
]


def _tool_resp(args: str) -> dict:
    return {
        "choices": [
            {
                "message": {
                    "role": "assistant",
                    "content": None,
                    "tool_calls": [
                        {
                            "id": "call_1",
                            "type": "function",
                            "function": {"name": "query_metric", "arguments": args},
                        }
                    ],
                }
            }
        ],
        "usage": {"prompt_tokens": 5, "completion_tokens": 2},
    }


def test_chat_parses_tool_calls_and_sends_tools(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json=_tool_resp('{"name": "queue_depth"}'))

    client = GroqClient(transport=httpx.MockTransport(handler))
    res = asyncio.run(client.chat([{"role": "user", "content": "q"}], TOOLS))
    assert seen["body"]["tools"] == TOOLS and seen["body"]["tool_choice"] == "auto"
    assert res.text == ""
    assert [(t.id, t.name, t.arguments) for t in res.tool_calls] == [
        ("call_1", "query_metric", {"name": "queue_depth"})
    ]


def test_chat_without_tools_omits_tool_fields(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["body"] = json.loads(request.content)
        return httpx.Response(200, json=CANNED)

    asyncio.run(GroqClient(transport=httpx.MockTransport(handler)).chat([]))
    assert "tools" not in seen["body"] and "tool_choice" not in seen["body"]


@pytest.mark.parametrize("bad", ["{not json", "[1, 2]"])
def test_chat_bad_arguments_do_not_raise(monkeypatch: pytest.MonkeyPatch, bad: str) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    client = GroqClient(
        transport=httpx.MockTransport(lambda r: httpx.Response(200, json=_tool_resp(bad)))
    )
    res = asyncio.run(client.chat([], TOOLS))
    assert res.tool_calls[0].arguments == {} and res.tool_calls[0].error


LOCAL = "http://localhost:11434/v1"


def test_groq_url_sends_bearer(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GROQ_API_KEY", FAKE_KEY)
    monkeypatch.delenv("LLM_BASE_URL", raising=False)
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("authorization")
        return httpx.Response(200, json=CANNED)

    asyncio.run(GroqClient(transport=httpx.MockTransport(handler)).chat([]))
    assert seen["url"] == "https://api.groq.com/openai/v1/chat/completions"
    assert seen["auth"] == f"Bearer {FAKE_KEY}"


def test_local_base_url_no_auth_no_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    monkeypatch.setenv("LLM_BASE_URL", LOCAL + "/")  # trailing slash must not double the path
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["headers"] = dict(request.headers)
        return httpx.Response(200, json=CANNED)

    out = asyncio.run(GroqClient(transport=httpx.MockTransport(handler)).generate("s", "u"))
    assert out == "hello"
    assert seen["url"] == LOCAL + "/chat/completions"
    assert "authorization" not in seen["headers"]


def test_local_server_500_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    monkeypatch.setenv("LLM_BASE_URL", LOCAL)
    client = GroqClient(transport=httpx.MockTransport(lambda r: httpx.Response(500)))
    with pytest.raises(httpx.HTTPStatusError):
        asyncio.run(client.generate("s", "u"))


def test_local_base_url_parses_tool_calls(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    monkeypatch.setenv("LLM_BASE_URL", LOCAL)
    client = GroqClient(
        transport=httpx.MockTransport(
            lambda r: httpx.Response(200, json=_tool_resp('{"name": "queue_depth"}'))
        )
    )
    res = asyncio.run(client.chat([], TOOLS))
    assert [(t.name, t.arguments) for t in res.tool_calls] == [
        ("query_metric", {"name": "queue_depth"})
    ]


GEMINI = "https://generativelanguage.googleapis.com/v1beta/openai"


def test_hosted_provider_uses_llm_api_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LLM_BASE_URL", GEMINI)
    monkeypatch.setenv("LLM_API_KEY", FAKE_KEY)
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    seen: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["auth"] = request.headers.get("authorization")
        return httpx.Response(200, json=CANNED)

    asyncio.run(GroqClient(transport=httpx.MockTransport(handler)).chat([]))
    assert seen["url"] == GEMINI + "/chat/completions"
    assert seen["auth"] == f"Bearer {FAKE_KEY}"


def test_hosted_provider_without_any_key_fails_closed(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("LLM_BASE_URL", GEMINI)
    monkeypatch.delenv("LLM_API_KEY", raising=False)
    monkeypatch.delenv("GROQ_API_KEY", raising=False)
    client = GroqClient(transport=httpx.MockTransport(lambda r: httpx.Response(200, json=CANNED)))
    with pytest.raises(RuntimeError) as ei:
        asyncio.run(client.chat([]))
    assert "LLM_API_KEY" in str(ei.value)
    assert "Bearer" not in str(ei.value)
