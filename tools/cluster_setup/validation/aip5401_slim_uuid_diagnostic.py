"""Isolate SLIM invoke behavior after creating a valid LAS UUID assistant.

Run inside the isolated candidate Pod. This bypasses SLIM create and HTTP
authentication and is a diagnostic only, not an end-to-end acceptance test.
"""

import asyncio
import json
import uuid
from types import SimpleNamespace

from slim_api.models.aws import InvokeAgentPayload
from slim_api.routers.las_agent import invoke_agent
from slim_api.util.las_client import get_las_client


async def main():
    assistant_id = str(uuid.uuid4())
    marker = uuid.uuid4().hex[:12]
    request = SimpleNamespace(headers={"request_id": f"aip5401-uuid-{marker}"})
    client = get_las_client()
    created = False
    try:
        assistant = await client.assistants.create(
            graph_id="echo", assistant_id=assistant_id, name=f"aip5401-{marker}"
        )
        created = True
        print(
            json.dumps(
                {
                    "step": "direct_las_create",
                    "assistant_id": assistant["assistant_id"],
                }
            )
        )
        payload = {
            "agent_id": assistant_id,
            "session_id": f"aip5401-{marker}",
            "task_prompt": f"synthetic-{marker}",
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
        print(json.dumps({"step": "slim_invoke", "status": response.status_code, "lines": lines}))
    finally:
        if created:
            await client.assistants.delete(assistant_id, delete_threads=True)
            print(json.dumps({"step": "direct_las_delete", "assistant_id": assistant_id}))


asyncio.run(main())
