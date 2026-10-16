"""Synthetic SLIM to LAS check inside an isolated candidate Pod.

This intentionally calls the SLIM route functions in-process, so it does not
exercise HTTP authentication. Use the real AITK HTTP route separately for the
production caller access acceptance criterion.
"""

import asyncio
import json
import uuid
from types import SimpleNamespace

from slim_api.models.aws import CreateAgentPayload, InvokeAgentPayload
from slim_api.routers.las_agent import create_agent, delete_agent, invoke_agent


async def main():
    marker = uuid.uuid4().hex[:12]
    name = f"aip5401probe{marker}"
    assistant_id = None
    session_id = f"aip5401probe{marker}"
    request = SimpleNamespace(headers={"request_id": f"aip5401-{marker}"})
    created = False
    try:
        response = await create_agent(
            "validation",
            CreateAgentPayload(agent_name=name, app_version="validation"),
            request,
        )
        create_body = json.loads(response.body)
        print(json.dumps({"step": "create", "status": response.status_code, "body": create_body}))
        if response.status_code != 201:
            return
        assistant_id = create_body.get("runtime_id")
        if not assistant_id:
            raise RuntimeError("SLIM create response did not include runtime_id")
        created = True
        payload = {
            "agent_id": assistant_id,
            "session_id": session_id,
            "task_prompt": f"synthetic-{marker}",
            "system_prompt": "Echo the user's message.",
            "llm": {},
            "mcps": [],
            "knowledge_bases": [],
            "agent_timeout": 30,
        }
        response = await invoke_agent(
            "validation", InvokeAgentPayload(payload=json.dumps(payload)), request
        )
        lines = []
        async for chunk in response.body_iterator:
            lines.extend(json.loads(line) for line in chunk.splitlines() if line)
        print(json.dumps({"step": "invoke", "status": response.status_code, "lines": lines}))
    finally:
        if created:
            response = await delete_agent("validation", assistant_id, request)
            print(
                json.dumps(
                    {
                        "step": "delete",
                        "status": response.status_code,
                        "body": json.loads(response.body),
                    }
                )
            )


asyncio.run(main())
