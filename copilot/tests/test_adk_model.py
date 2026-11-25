"""ADK model adapter (Phase B). Offline: a fake GuardedModel stands in for the real one."""

from __future__ import annotations

import asyncio

import pytest

pytest.importorskip("google.adk")

from google.adk.models.llm_request import LlmRequest  # noqa: E402
from google.genai import types  # noqa: E402

from copilot.agents.adk_model import (  # noqa: E402
    GuardedAdkModel,
    to_messages,
    to_response,
    to_tools,
)
from copilot.agents.model import ChatResult, ToolCall  # noqa: E402


class FakeGuarded:
    def __init__(self, result: ChatResult):
        self.result = result
        self.calls: list[tuple[list[dict], list[dict] | None]] = []

    async def chat(self, messages, tools=None):
        self.calls.append((messages, tools))
        return self.result


def _request(contents, system=None, tools=None) -> LlmRequest:
    config = types.GenerateContentConfig(system_instruction=system, tools=tools)
    return LlmRequest(model="fake", contents=contents, config=config)


def test_system_and_user_text_become_messages():
    req = _request(
        [types.Content(role="user", parts=[types.Part(text="why is the queue growing?")])],
        system="You are an investigator.",
    )
    assert to_messages(req) == [
        {"role": "system", "content": "You are an investigator."},
        {"role": "user", "content": "why is the queue growing?"},
    ]


def test_function_call_and_response_round_trip_to_tool_messages():
    req = _request(
        [
            types.Content(role="user", parts=[types.Part(text="check metrics")]),
            types.Content(
                role="model",
                parts=[
                    types.Part(
                        function_call=types.FunctionCall(
                            id="c1", name="query_metric", args={"metric": "queue"}
                        )
                    )
                ],
            ),
            types.Content(
                role="user",
                parts=[
                    types.Part(
                        function_response=types.FunctionResponse(
                            id="c1", name="query_metric", response={"value": 12}
                        )
                    )
                ],
            ),
        ]
    )
    msgs = to_messages(req)
    assert msgs[1] == {
        "role": "assistant",
        "content": None,
        "tool_calls": [
            {
                "id": "c1",
                "type": "function",
                "function": {"name": "query_metric", "arguments": '{"metric": "queue"}'},
            }
        ],
    }
    assert msgs[2] == {"role": "tool", "tool_call_id": "c1", "content": '{"value": 12}'}


def test_declared_tools_convert_to_openai_format_with_lowercase_types():
    decl = types.FunctionDeclaration(
        name="query_metric",
        description="Read one metric.",
        parameters_json_schema={
            "type": "OBJECT",
            "properties": {"window_s": {"type": "INTEGER"}},
            "required": ["window_s"],
        },
    )
    req = _request([], tools=[types.Tool(function_declarations=[decl])])
    assert to_tools(req) == [
        {
            "type": "function",
            "function": {
                "name": "query_metric",
                "description": "Read one metric.",
                "parameters": {
                    "type": "object",
                    "properties": {"window_s": {"type": "integer"}},
                    "required": ["window_s"],
                },
            },
        }
    ]


def test_tool_calls_come_back_as_function_call_parts():
    call = ToolCall(id="x", name="search_runbook", arguments={"q": "a"})
    resp = to_response(ChatResult(text="", tool_calls=[call]))
    part = resp.content.parts[0]
    assert part.function_call.name == "search_runbook"
    assert part.function_call.args == {"q": "a"}
    assert resp.partial is False


def test_text_answer_comes_back_as_text_part():
    resp = to_response(ChatResult(text="Queue backlog.", tool_calls=[]))
    assert resp.content.parts[0].text == "Queue backlog."


def test_adk_call_goes_through_the_guarded_model():
    fake = FakeGuarded(ChatResult(text="ok", tool_calls=[]))
    model = GuardedAdkModel(model="copilot-local", guarded=fake)
    req = _request(
        [types.Content(role="user", parts=[types.Part(text="hi")])],
        system="sys",
    )

    async def go():
        return [r async for r in model.generate_content_async(req)]

    responses = asyncio.run(go())
    assert len(fake.calls) == 1
    messages, tools = fake.calls[0]
    assert messages[0] == {"role": "system", "content": "sys"}
    assert tools is None  # no declared tools: None, not an empty list
    assert responses[0].content.parts[0].text == "ok"
