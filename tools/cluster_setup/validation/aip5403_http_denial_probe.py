"""Probe unauthenticated SLIM LAS routes through an installer port-forward.

Use only against the isolated candidate Service on the test cluster. No real
credentials are read or printed. Positive caller acceptance needs a separate
Splunk-issued test token.
"""

import json
import urllib.error
import urllib.request


ROOT = "http://127.0.0.1:18082/validation/slim-api/v1alpha1/agent/las"
CASES = (
    ("create_missing_bearer", ROOT, {"agent_name": "aip5403probe", "app_version": "validation"}, {}),
    (
        "invoke_missing_bearer",
        ROOT + "/invoke",
        {"payload": json.dumps({"agent_id": "aip5403probe", "session_id": "test", "task_prompt": "test"})},
        {},
    ),
    (
        "create_invalid_bearer",
        ROOT,
        {"agent_name": "aip5403probe", "app_version": "validation"},
        {"Authorization": "Bearer invalid.synthetic.token"},
    ),
)

for label, url, body, additional_headers in CASES:
    headers = {"Content-Type": "application/json", "request_id": label}
    headers.update(additional_headers)
    request = urllib.request.Request(
        url, data=json.dumps(body).encode(), headers=headers, method="POST"
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            status, payload = response.status, response.read().decode()
    except urllib.error.HTTPError as exc:
        status, payload = exc.code, exc.read().decode()
    print(json.dumps({"case": label, "status": status, "body": payload}))
