"""Config-driven SLIM caller-route checks for approved product graph cases.

Run inside the SLIM Pod and pipe a trusted case JSON document to stdin. Input
may contain credentials and is never logged, printed, or written by this tool.
The caller must supply approved product graph expectations; this harness does
not invent product fixtures or expected answers.
"""

import asyncio
import contextlib
import io
import json
import logging
import sys
import uuid
from types import SimpleNamespace
from typing import Any

def jsonable(value: Any) -> Any:
    if hasattr(value, "model_dump"):
        return value.model_dump(mode="json")
    if hasattr(value, "dict"):
        return value.dict()
    if isinstance(value, dict):
        return {key: jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [jsonable(item) for item in value]
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    return vars(value) if hasattr(value, "__dict__") else str(value)


def strings(value: Any):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield from strings(key)
            yield from strings(item)
    elif isinstance(value, (list, tuple)):
        for item in value:
            yield from strings(item)


def assert_absent(value: Any, secrets: list[str], source: str):
    serialized = "\n".join(strings(value))
    if any(secret and secret in serialized for secret in secrets):
        raise AssertionError(f"sensitive marker appeared in {source}")


async def all_checkpoints(client: Any, thread_id: str) -> list[Any]:
    snapshots = []
    before = None
    seen = set()
    while True:
        page = await client.threads.get_history(thread_id, limit=100, before=before)
        if not page:
            return snapshots
        normalized = [jsonable(item) for item in page]
        snapshots.extend(normalized)
        if len(page) < 100:
            return snapshots
        checkpoint = normalized[-1].get("checkpoint", {})
        cursor = checkpoint.get("checkpoint_id") if isinstance(checkpoint, dict) else None
        if not cursor or cursor in seen:
            raise AssertionError("checkpoint history pagination did not advance")
        seen.add(cursor)
        before = checkpoint


async def collect(response: Any):
    chunks = []
    async for chunk in response.body_iterator:
        chunks.append(chunk.decode("utf-8", errors="replace") if isinstance(chunk, bytes) else str(chunk))
    events = []
    for chunk in chunks:
        for line in chunk.splitlines():
            if line.strip():
                events.append(json.loads(line))
    return response.status_code, events


class Capture(logging.Handler):
    def __init__(self):
        super().__init__()
        self.records = []

    def emit(self, record):
        self.records.append(record.getMessage())


async def invoke(tenant: str, payload: dict[str, Any], request_id: str):
    from slim_api.models.aws import InvokeAgentPayload
    from slim_api.routers.las_agent import invoke_agent

    handler = Capture()
    root = logging.getLogger()
    root.addHandler(handler)
    stdout, stderr = io.StringIO(), io.StringIO()
    try:
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            response = await invoke_agent(
                tenant, InvokeAgentPayload(payload=json.dumps(payload)),
                SimpleNamespace(headers={"request_id": request_id}),
            )
            status, events = await collect(response)
    finally:
        root.removeHandler(handler)
    return status, events, {"stdout": stdout.getvalue(), "stderr": stderr.getvalue(), "logs": handler.records}


def assert_expected_result(events: list[Any], expected: Any, check: str) -> None:
    if not isinstance(expected, str) or not expected.strip():
        raise AssertionError(f"{check} needs a non-empty expected result")
    result_values = [
        event["result"]
        for event in events
        if isinstance(event, dict) and isinstance(event.get("result"), str)
    ]
    if expected not in "\n".join(result_values):
        raise AssertionError(f"{check} missed its expected result in the caller result field")


async def main():
    from slim_api.models.aws import CreateAgentPayload
    from slim_api.routers.las_agent import create_agent, delete_agent
    from slim_api.util.las_client import get_las_client

    config = json.load(sys.stdin)
    cases = config.get("cases") if isinstance(config, dict) else None
    if not isinstance(cases, list) or not cases:
        raise ValueError("stdin JSON must provide non-empty cases")
    secrets = config.get("sensitive_values", [])
    if not isinstance(secrets, list) or not all(isinstance(item, str) for item in secrets):
        raise ValueError("sensitive_values must be a list of strings")
    tenant = "product-qualification"
    token = uuid.uuid4().hex[:12]
    request = SimpleNamespace(headers={"request_id": f"aip5410-{token}-create"})
    created = await create_agent(
        tenant,
        CreateAgentPayload(agent_name=f"aip5410-{token}", app_version="aip5410-acceptance"),
        request,
    )
    if created.status_code != 201:
        raise AssertionError(f"assistant create returned HTTP {created.status_code}")
    body = json.loads(created.body)
    assistant_id = body.get("runtime_id")
    if not assistant_id:
        raise AssertionError("assistant create omitted runtime_id")
    client = get_las_client()
    check_names = []
    try:
        for case_index, case in enumerate(cases):
            if not isinstance(case, dict) or not isinstance(case.get("name"), str):
                raise ValueError("each product case needs a name")
            calls = case.get("calls")
            if not isinstance(calls, list) or not calls:
                raise ValueError("each product case needs one or more calls")
            session_id = str(uuid.uuid4())
            for call_index, call in enumerate(calls):
                if not isinstance(call, dict) or not isinstance(call.get("task_prompt"), str):
                    raise ValueError("each call needs task_prompt")
                expected = call.get("expected_text")
                if not isinstance(expected, str) or not expected.strip():
                    raise ValueError("each call needs a non-empty expected_text")
                payload = {
                    "agent_id": assistant_id,
                    "session_id": session_id,
                    "task_prompt": call["task_prompt"],
                    "system_prompt": call.get("system_prompt", ""),
                    "agent_timeout": int(call.get("agent_timeout", 120)),
                    **{key: call[key] for key in ("llm", "mcps", "knowledge_bases", "hitl_response") if key in call},
                }
                status, events, captured = await invoke(
                    tenant, payload, f"aip5410-{token}-{case_index}-{call_index}"
                )
                if status < 200 or status >= 300 or any(
                    isinstance(event, dict) and event.get("success") is False for event in events
                ):
                    raise AssertionError(f"case {case['name']} call {call_index} failed (HTTP {status})")
                assert_expected_result(events, expected, f"case {case['name']} call {call_index}")
                assert_absent(events, secrets, "caller response")
                assert_absent(captured, secrets, "caller logs")
                check_names.append(f"{case['name']}:call-{call_index + 1}")

            state = jsonable(await client.threads.get_state(session_id))
            values = state.get("values", {}) if isinstance(state, dict) else {}
            state_text = "\n".join(strings(values))
            if not all(call.get("expected_text") in state_text for call in calls):
                raise AssertionError(f"case {case['name']} expected outputs were not in graph state values")
            assert_absent(values, secrets, "graph state values")
            history = await all_checkpoints(client, session_id)
            assert_absent(history, secrets, "checkpoint history")
            check_names.extend((f"{case['name']}:state-values", f"{case['name']}:checkpoint-redaction"))
    finally:
        deleted = await delete_agent(tenant, assistant_id, request)
        if deleted.status_code < 200 or deleted.status_code >= 300:
            raise AssertionError("qualification assistant cleanup failed")
    print(json.dumps({"result": "PASS", "checks": check_names}, sort_keys=True))


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as exc:
        # Do not emit exceptions that may contain request bodies or credentials.
        message = str(exc)
        safe = message if "credential" not in message.lower() and "token" not in message.lower() else "sensitive failure details withheld"
        print(json.dumps({"result": "FAIL", "reason": safe}, sort_keys=True), file=sys.stderr)
        raise SystemExit(1)
