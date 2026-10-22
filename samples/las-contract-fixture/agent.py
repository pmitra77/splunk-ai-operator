"""Deterministic Agent Server fixture for the LAS deployment contract."""

from time import process_time
from typing import Any, TypedDict

from langgraph.graph import END, START, StateGraph
from langgraph.runtime import Runtime
from langgraph.types import interrupt


class FixtureState(TypedDict, total=False):
    request: str
    messages: list[dict[str, Any]]
    system_prompt: str
    response: str
    context_keys: list[str]
    context_summary: dict[str, Any]
    pause: bool
    cpu_seconds: float


class FixtureContext(TypedDict, total=False):
    llm_config: dict[str, Any]
    mcp_configs: list[dict[str, Any]]
    kb_configs: list[dict[str, Any]]


def respond(state: FixtureState, runtime: Runtime[FixtureContext]) -> FixtureState:
    """Support direct fixture and SLIM message inputs without exposing context."""
    messages = state.get("messages", [])
    last_message = messages[-1] if messages else {}
    if isinstance(last_message, dict):
        message_text = last_message.get("content", "")
    else:
        message_text = getattr(last_message, "content", "")
    request = state.get("request") or message_text or ""

    # A deterministic marker lets the caller-route probe exercise the resume
    # path without introducing a model or an external tool dependency.
    if state.get("pause") or "AIP5402_INTERRUPT" in request:
        interrupt("Resume the fixture run with fresh context")

    cpu_seconds = state.get("cpu_seconds", 0)
    if isinstance(cpu_seconds, bool) or not isinstance(cpu_seconds, (int, float)):
        raise ValueError("cpu_seconds must be a number")
    if not 0 <= cpu_seconds <= 15:
        raise ValueError("cpu_seconds must be between 0 and 15")
    if cpu_seconds:
        # Burn CPU in the Agent Server worker so HPA tests exercise its metric.
        deadline = process_time() + cpu_seconds
        value = 0
        while process_time() < deadline:
            value = (value * 33 + 17) % 65521

    context = runtime.context or {}
    allowed = ("llm_config", "mcp_configs", "kb_configs")
    response = f"LAS contract fixture received: {request}"
    llm_config = context.get("llm_config") or {}
    mcp_configs = context.get("mcp_configs") or []
    kb_configs = context.get("kb_configs") or []
    return {
        "response": response,
        "messages": messages + [{"role": "assistant", "content": response}],
        "context_keys": [key for key in allowed if key in context],
        # Return only non-sensitive identifiers so tests can distinguish fresh
        # run/resume context without echoing credential-bearing values.
        "context_summary": {
            "llm_model": llm_config.get("model"),
            "mcp_names": [item.get("name") for item in mcp_configs if isinstance(item, dict)],
            "kb_names": [item.get("name") for item in kb_configs if isinstance(item, dict)],
        },
    }


builder = StateGraph(FixtureState, context_schema=FixtureContext)
builder.add_node("respond", respond)
builder.add_edge(START, "respond")
builder.add_edge("respond", END)
graph = builder.compile()
