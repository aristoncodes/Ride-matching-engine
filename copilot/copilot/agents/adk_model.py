"""Google ADK model adapter: every ADK model call goes through GuardedModel.

ADK builds an LlmRequest (contents, system instruction, function declarations) and expects an
LlmResponse back. This module converts both ways between that and the OpenAI-style messages
that GuardedModel.chat takes, so timeout, retry, the circuit breaker and the cache all still
apply. ADK's own Gemini client is never used.

Only text and function calls are converted. Other part types (images, files) are dropped, and
the copilot never sends them.
"""

from __future__ import annotations

import json
from collections.abc import AsyncGenerator
from typing import Any

from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types

from copilot.agents.model import ChatResult


class GuardedAdkModel(BaseLlm):
    """ADK model backed by a GuardedModel. `guarded` is any object with an async chat()."""

    guarded: Any

    async def generate_content_async(
        self, llm_request: LlmRequest, stream: bool = False
    ) -> AsyncGenerator[LlmResponse, None]:
        messages = to_messages(llm_request)
        tools = to_tools(llm_request)
        result: ChatResult = await self.guarded.chat(messages, tools or None)
        yield to_response(result)


def _text_of(content: types.Content | str | None) -> str:
    if content is None:
        return ""
    if isinstance(content, str):
        return content
    return "".join(p.text or "" for p in (content.parts or []) if p.text)


def to_messages(llm_request: LlmRequest) -> list[dict]:
    config = llm_request.config
    messages: list[dict] = []
    system = _text_of(config.system_instruction) if config and config.system_instruction else ""
    if system:
        messages.append({"role": "system", "content": system})
    for content in llm_request.contents or []:
        if content.role == "model":
            texts = [p.text for p in content.parts or [] if p.text]
            calls = [p.function_call for p in content.parts or [] if p.function_call]
            msg: dict[str, Any] = {"role": "assistant", "content": "".join(texts) or None}
            if calls:
                msg["tool_calls"] = [
                    {
                        "id": c.id or c.name,
                        "type": "function",
                        "function": {"name": c.name, "arguments": json.dumps(c.args or {})},
                    }
                    for c in calls
                ]
            messages.append(msg)
            continue
        for p in content.parts or []:
            if p.function_response is not None:
                fr = p.function_response
                messages.append(
                    {
                        "role": "tool",
                        "tool_call_id": fr.id or fr.name,
                        "content": json.dumps(fr.response or {}),
                    }
                )
            elif p.text:
                messages.append({"role": "user", "content": p.text})
    return messages


def _lower_types(schema: Any) -> Any:
    """ADK Schema types are upper-case ('OBJECT'); the OpenAI tool format wants 'object'."""
    if isinstance(schema, dict):
        return {
            k: (v.lower() if k == "type" and isinstance(v, str) else _lower_types(v))
            for k, v in schema.items()
        }
    if isinstance(schema, list):
        return [_lower_types(v) for v in schema]
    return schema


def to_tools(llm_request: LlmRequest) -> list[dict]:
    tools: list[dict] = []
    config = llm_request.config
    for tool in (config.tools or []) if config else []:
        for decl in tool.function_declarations or []:
            if decl.parameters_json_schema is not None:
                params = _lower_types(decl.parameters_json_schema)
            elif decl.parameters is not None:
                params = _lower_types(decl.parameters.model_dump(exclude_none=True, mode="json"))
            else:
                params = {"type": "object", "properties": {}}
            tools.append(
                {
                    "type": "function",
                    "function": {
                        "name": decl.name,
                        "description": decl.description or "",
                        "parameters": params,
                    },
                }
            )
    return tools


def to_response(result: ChatResult) -> LlmResponse:
    parts: list[types.Part] = []
    if result.text:
        parts.append(types.Part(text=result.text))
    for tc in result.tool_calls:
        parts.append(
            types.Part(
                function_call=types.FunctionCall(id=tc.id, name=tc.name, args=tc.arguments or {})
            )
        )
    if not parts:
        parts.append(types.Part(text=""))
    return LlmResponse(content=types.Content(role="model", parts=parts), partial=False)
