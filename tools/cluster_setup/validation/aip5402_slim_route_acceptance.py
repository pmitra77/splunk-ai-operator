"""Exercise the SLIM LAS caller route with the deterministic contract graph.

Run inside the isolated SLIM candidate Pod. The route functions are invoked
directly (HTTP authentication is covered by the AIP-5403 HTTP probe); every
LAS invocation goes through SLIM's ``create_agent`` / ``invoke_agent`` route.
Only fake credential values are used. The script prints check names, never
request bodies, checkpoint contents, or credential values.
"""

import asyncio
import contextlib
import io
import json
import logging
import os
import uuid
from types import SimpleNamespace
from typing import Any

from slim_api.models.aws import CreateAgentPayload, InvokeAgentPayload
from slim_api.routers.las_agent import create_agent, delete_agent, invoke_agent
from slim_api.util.las_client import get_las_client


FIXTURE_GRAPH = "contract_fixture"
REQUEST_ID_PREFIX = "aip5402-"


def jsonable(value: Any) -> Any:
    """Convert SDK model objects into JSON-compatible values for inspection."""
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


def assert_markers_absent(value: Any, markers: list[str], source: str) -> None:
    haystack = "\n".join(strings(value))
    leaked = [index for index, marker in enumerate(markers) if marker in haystack]
    if leaked:
        raise AssertionError(f"fake credential marker(s) found in {source}")


async def checkpoint_history(client: Any, thread_id: str) -> list[Any]:
    """Return every paginated state snapshot for a thread."""
    snapshots = []
    before = None
    seen_cursors = set()
    page_size = 100
    while True:
        page = await client.threads.get_history(thread_id, limit=page_size, before=before)
        if not page:
            return snapshots
        normalized = [jsonable(state) for state in page]
        snapshots.extend(normalized)
        if len(page) < page_size:
            return snapshots
        checkpoint = normalized[-1].get("checkpoint", {})
        cursor = checkpoint.get("checkpoint_id") if isinstance(checkpoint, dict) else None
        if not cursor or cursor in seen_cursors:
            raise AssertionError("could not advance through complete thread checkpoint history")
        seen_cursors.add(cursor)
        before = checkpoint


def parse_chunks(chunks: list[Any]) -> list[Any]:
    lines = []
    for chunk in chunks:
        if isinstance(chunk, bytes):
            chunk = chunk.decode("utf-8", errors="replace")
        for line in str(chunk).splitlines():
            if line.strip():
                lines.append(json.loads(line))
    return lines


async def collect_response(response: Any) -> tuple[int, list[Any]]:
    chunks = []
    async for chunk in response.body_iterator:
        chunks.append(chunk)
    return response.status_code, parse_chunks(chunks)


class MemoryLogHandler(logging.Handler):
    def __init__(self):
        super().__init__()
        self.messages = []

    def emit(self, record):
        self.messages.append(record.getMessage())


async def invoke_once(tenant: str, payload: dict[str, Any], request_id: str):
    request = SimpleNamespace(headers={"request_id": request_id})
    log_handler = MemoryLogHandler()
    root_logger = logging.getLogger()
    root_logger.addHandler(log_handler)
    stdout, stderr = io.StringIO(), io.StringIO()
    try:
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            response = await invoke_agent(
                tenant,
                InvokeAgentPayload(payload=json.dumps(payload)),
                request,
            )
            status, lines = await collect_response(response)
            media_type = response.media_type
    finally:
        root_logger.removeHandler(log_handler)
    captured = {
        "stdout": stdout.getvalue(),
        "stderr": stderr.getvalue(),
        "logs": log_handler.messages,
    }
    return status, lines, captured, media_type


def assert_success(status: int, lines: list[Any], expected_text: str, check: str) -> None:
    serialized = json.dumps(lines, sort_keys=True)
    if status < 200 or status >= 300:
        raise AssertionError(f"{check} returned HTTP {status}")
    if expected_text not in serialized:
        raise AssertionError(f"{check} did not return the expected fixture response")
    if any(isinstance(line, dict) and line.get("success") is False for line in lines):
        raise AssertionError(f"{check} returned a failed terminal event")


async def main():
    if os.environ.get("LAS_GRAPH_ID") != FIXTURE_GRAPH:
        raise RuntimeError(f"candidate must select LAS_GRAPH_ID={FIXTURE_GRAPH}")
    if not os.environ.get("AGENT_SERVER_URL"):
        raise RuntimeError("candidate must set AGENT_SERVER_URL to the stable LAS Service")

    marker = uuid.uuid4().hex
    tenant = "validation"
    assistant_name = f"aip5402-{marker[:12]}"
    request = SimpleNamespace(headers={"request_id": f"{REQUEST_ID_PREFIX}{marker[:12]}"})
    assistant_id = None
    fake_credentials = [
        f"aip5402-llm-secret-{marker}",
        f"aip5402-mcp-secret-{marker}",
        f"aip5402-kb-secret-{marker}",
    ]
    checks = []
    client = get_las_client()
    try:
        created = await create_agent(
            tenant,
            CreateAgentPayload(agent_name=assistant_name, app_version="aip5402-test"),
            request,
        )
        if created.status_code != 201:
            raise AssertionError(f"fixture assistant creation returned HTTP {created.status_code}")
        created_body = json.loads(created.body)
        assistant_id = created_body.get("runtime_id")
        if not assistant_id:
            raise AssertionError("fixture assistant creation omitted runtime_id")
        checks.append("caller_route_assistant_create")

        run_session = str(uuid.uuid4())
        run_prompt = f"AIP5402_RUN_{marker}"
        run_context = {
            "llm": {"model": f"aip5402-model-{marker}", "api_key": fake_credentials[0]},
            "mcps": [
                {
                    "name": f"aip5402-run-mcp-{marker}",
                    "headers": {"Authorization": f"Bearer {fake_credentials[1]}"},
                }
            ],
            "knowledge_bases": [
                {"name": f"aip5402-run-kb-{marker}", "token": fake_credentials[2]}
            ],
        }
        run_payload = {
            "agent_id": assistant_id,
            "session_id": run_session,
            "task_prompt": run_prompt,
            "system_prompt": "Use the deterministic contract fixture.",
            "agent_timeout": 30,
            **run_context,
        }
        status, lines, captured, media_type = await invoke_once(
            tenant, run_payload, f"{REQUEST_ID_PREFIX}{marker[:12]}-run"
        )
        assert_success(status, lines, run_prompt, "caller run")
        if media_type != "application/x-ndjson":
            raise AssertionError(f"caller run returned unexpected stream media type: {media_type}")
        assert_markers_absent(lines, fake_credentials, "caller run result")
        assert_markers_absent(captured, fake_credentials, "caller run logs")
        run_state = jsonable(await client.threads.get_state(run_session))
        run_values = run_state.get("values", {}) if isinstance(run_state, dict) else {}
        expected_context = {"llm_config", "mcp_configs", "kb_configs"}
        if not expected_context.issubset(set(run_values.get("context_keys", []))):
            raise AssertionError("caller run did not forward all fixture context fields")
        run_summary = run_values.get("context_summary", {})
        if run_summary.get("llm_model") != run_context["llm"]["model"]:
            raise AssertionError("caller run did not forward the invocation's model context")
        if run_summary.get("mcp_names") != [run_context["mcps"][0]["name"]]:
            raise AssertionError("caller run did not forward the invocation's MCP context")
        if run_summary.get("kb_names") != [run_context["knowledge_bases"][0]["name"]]:
            raise AssertionError("caller run did not forward the invocation's KB context")
        run_history = await checkpoint_history(client, run_session)
        assert_markers_absent(run_history, fake_credentials, "caller run checkpoint history")
        checks.extend(("caller_run_context_forwarding", "run_result_and_checkpoint_redaction"))

        resume_session = str(uuid.uuid4())
        resume_prompt = f"AIP5402_INTERRUPT_{marker}"
        first_resume_payload = {
            "agent_id": assistant_id,
            "session_id": resume_session,
            "task_prompt": resume_prompt,
            "system_prompt": "Pause once, then complete after the caller resumes.",
            "agent_timeout": 30,
            **run_context,
        }
        status, interrupt_lines, captured, _ = await invoke_once(
            tenant, first_resume_payload, f"{REQUEST_ID_PREFIX}{marker[:12]}-interrupt"
        )
        if status < 200 or status >= 300:
            raise AssertionError(f"caller interrupt request returned HTTP {status}")
        if not any("interrupt" in json.dumps(line).lower() for line in interrupt_lines):
            raise AssertionError("caller route did not expose the fixture interrupt")
        assert_markers_absent(interrupt_lines, fake_credentials, "interrupt response")
        assert_markers_absent(captured, fake_credentials, "interrupt logs")

        resume_credentials = [
            f"aip5402-resume-llm-secret-{marker}",
            f"aip5402-resume-mcp-secret-{marker}",
            f"aip5402-resume-kb-secret-{marker}",
        ]
        resume_payload = {
            "agent_id": assistant_id,
            "session_id": resume_session,
            "task_prompt": resume_prompt,
            "hitl_response": f"aip5402-resume-{marker}",
            "system_prompt": "Complete the deterministic fixture.",
            "agent_timeout": 30,
            "llm": {"model": f"aip5402-resume-model-{marker}", "api_key": resume_credentials[0]},
            "mcps": [
                {
                    "name": f"aip5402-resume-mcp-{marker}",
                    "headers": {"Authorization": f"Bearer {resume_credentials[1]}"},
                }
            ],
            "knowledge_bases": [
                {"name": f"aip5402-resume-kb-{marker}", "token": resume_credentials[2]}
            ],
        }
        status, resume_lines, captured, media_type = await invoke_once(
            tenant, resume_payload, f"{REQUEST_ID_PREFIX}{marker[:12]}-resume"
        )
        assert_success(status, resume_lines, resume_prompt, "caller resume")
        if media_type != "application/x-ndjson":
            raise AssertionError(f"caller resume returned unexpected stream media type: {media_type}")
        assert_markers_absent(resume_lines, fake_credentials + resume_credentials, "resume result")
        assert_markers_absent(captured, fake_credentials + resume_credentials, "resume logs")
        resume_state = jsonable(await client.threads.get_state(resume_session))
        resume_values = resume_state.get("values", {}) if isinstance(resume_state, dict) else {}
        if not expected_context.issubset(set(resume_values.get("context_keys", []))):
            raise AssertionError("caller resume did not forward fresh fixture context")
        resume_summary = resume_values.get("context_summary", {})
        if resume_summary.get("llm_model") != resume_payload["llm"]["model"]:
            raise AssertionError("caller resume did not replace context with fresh invocation values")
        if resume_summary.get("mcp_names") != [resume_payload["mcps"][0]["name"]]:
            raise AssertionError("caller resume did not replace MCP context with fresh invocation values")
        if resume_summary.get("kb_names") != [resume_payload["knowledge_bases"][0]["name"]]:
            raise AssertionError("caller resume did not replace KB context with fresh invocation values")
        resume_history = await checkpoint_history(client, resume_session)
        assert_markers_absent(resume_history, fake_credentials + resume_credentials, "resume checkpoint history")
        checks.extend(("caller_resume_context_forwarding", "resume_result_and_checkpoint_redaction"))

        # A malformed body must fail safely and must not create a run.
        try:
            bad_response = await invoke_agent(
                tenant,
                InvokeAgentPayload(payload="{"),
                SimpleNamespace(headers={"request_id": f"{REQUEST_ID_PREFIX}{marker[:12]}-invalid"}),
            )
            bad_status, bad_lines = await collect_response(bad_response)
            invalid_rejected = bad_status >= 400 or any(
                isinstance(line, dict) and line.get("success") is False for line in bad_lines
            )
        except (ValueError, json.JSONDecodeError):
            invalid_rejected = True
        if not invalid_rejected:
            raise AssertionError("caller route accepted malformed JSON")
        checks.append("malformed_payload_rejected")
    finally:
        if assistant_id:
            deleted = await delete_agent(tenant, assistant_id, request)
            if deleted.status_code < 200 or deleted.status_code >= 300:
                raise AssertionError(f"fixture assistant cleanup returned HTTP {deleted.status_code}")
            checks.append("caller_route_cleanup")
    print(json.dumps({"result": "PASS", "checks": checks}, sort_keys=True))


asyncio.run(main())
