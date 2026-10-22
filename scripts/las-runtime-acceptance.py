#!/usr/bin/env python3
"""Run sanitized LAS protocol and queue scaling acceptance checks.

Scenario data is read from stdin so product context need not be stored in a
file or command argument. Response bodies and request values are never printed.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any


MAX_BODY = 8 * 1024 * 1024
TERMINAL = {"success", "error", "failed", "cancelled", "timeout"}


class AcceptanceError(RuntimeError):
    pass


class LASClient:
    def __init__(self, base_url: str, timeout: float):
        parsed = urllib.parse.urlsplit(base_url)
        if parsed.scheme not in ("http", "https") or not parsed.hostname:
            raise AcceptanceError("LAS_BASE_URL must be an http(s) URL")
        if parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise AcceptanceError("LAS_BASE_URL cannot contain credentials, query, or fragment")
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.api_key = os.environ.get("LAS_API_KEY", "")

    def request(self, method: str, path: str, payload: Any = None, accept: str = "application/json"):
        data = None if payload is None else json.dumps(payload, separators=(",", ":")).encode()
        headers = {"Accept": accept}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if self.api_key:
            headers["X-Api-Key"] = self.api_key
        request = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                body = response.read(MAX_BODY + 1)
                if len(body) > MAX_BODY:
                    raise AcceptanceError("LAS response exceeded the size limit")
                return response.status, body, response.headers.get_content_type()
        except urllib.error.HTTPError as exc:
            # Deliberately discard the body: it may contain credentials or input.
            raise AcceptanceError(f"LAS returned HTTP {exc.code}") from None
        except (urllib.error.URLError, TimeoutError, OSError):
            raise AcceptanceError("LAS request failed or timed out") from None


def json_body(body: bytes) -> Any:
    try:
        return json.loads(body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise AcceptanceError("LAS returned an invalid JSON response") from None


def strings(value: Any):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for item in value.values():
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)


def path_segment(value: str) -> str:
    return urllib.parse.quote(value, safe="")


def create_thread(client: LASClient) -> str:
    status, body, _ = client.request("POST", "/threads", {})
    result = json_body(body)
    thread_id = result.get("thread_id") if isinstance(result, dict) else None
    if status not in (200, 201) or not isinstance(thread_id, str) or not thread_id:
        raise AcceptanceError("thread creation failed")
    return thread_id


def assert_state(client: LASClient, thread_id: str, expected: list[str], sensitive: list[str]):
    status, body, _ = client.request("GET", f"/threads/{path_segment(thread_id)}/state")
    result = json_body(body)
    if status != 200 or not isinstance(result, dict) or not isinstance(result.get("values"), (dict, list)):
        raise AcceptanceError("thread state response omitted graph values")
    state_values = list(strings(result["values"]))
    for item in expected:
        if not any(item in value for value in state_values):
            raise AcceptanceError("expected value was not persisted under graph state values")
    for secret in sensitive:
        if secret and any(secret in value for value in state_values):
            raise AcceptanceError("sensitive marker appeared in persisted graph state values")


def sse_has_data(body: bytes, expected: list[str], sensitive: list[str]) -> bool:
    if not body:
        return False
    text = body.decode("utf-8", errors="replace")
    payloads = []
    for line in text.splitlines():
        if line.startswith("data:"):
            payloads.append(line[5:].strip())
    joined = "\n".join(payloads)
    if any(secret and secret in joined for secret in sensitive):
        raise AcceptanceError("sensitive marker appeared in SSE output")
    return all(marker in joined for marker in expected)


def wait_for_run(client: LASClient, thread_id: str, run_id: str, timeout: float):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status, body, _ = client.request(
            "GET", f"/threads/{path_segment(thread_id)}/runs/{path_segment(run_id)}"
        )
        run = json_body(body)
        run_status = run.get("status") if isinstance(run, dict) else None
        if run_status in TERMINAL:
            if run_status != "success":
                raise AcceptanceError(f"queued run ended in {run_status}")
            return
        time.sleep(2)
    raise AcceptanceError("queued run did not reach a terminal state before timeout")


def run_protocol_case(client: LASClient, graph_id: str, case: dict[str, Any], timeout: float):
    name = case.get("name")
    run_input = case.get("input")
    expected = case.get("expected_state_values", [])
    sensitive = case.get("sensitive_values", [])
    modes = case.get("run_modes", ["sync", "queued", "stream"])
    if not isinstance(name, str) or not name or not isinstance(run_input, dict):
        raise AcceptanceError("each case needs a name and object input")
    if not isinstance(expected, list) or not all(isinstance(v, str) for v in expected):
        raise AcceptanceError("expected_state_values must be a string list")
    if not expected or not all(expected):
        raise AcceptanceError("expected_state_values must include at least one non-empty persisted marker")
    if not isinstance(sensitive, list) or not all(isinstance(v, str) for v in sensitive):
        raise AcceptanceError("sensitive_values must be a string list")
    if not isinstance(modes, list) or not modes or set(modes) - {"sync", "queued", "stream"}:
        raise AcceptanceError("run_modes may contain sync, queued, and stream")

    checks = []
    for mode in modes:
        marker = f"aip-qualification-{uuid.uuid4().hex}"
        input_copy = dict(run_input)
        # The marker is an independent value to verify persistence, without
        # relying on matching a response that may be emitted outside state.
        input_copy.setdefault("request", marker)
        thread_id = create_thread(client)
        payload = {"assistant_id": graph_id, "input": input_copy}
        thread_path = path_segment(thread_id)
        if mode == "sync":
            status, body, _ = client.request("POST", f"/threads/{thread_path}/runs/wait", payload)
            result = json_body(body)
            if status != 200:
                raise AcceptanceError("synchronous run failed")
            output = "\n".join(strings(result))
            if any(secret and secret in output for secret in sensitive):
                raise AcceptanceError("sensitive marker appeared in synchronous response")
            if expected and not all(item in output for item in expected):
                raise AcceptanceError("synchronous response omitted an expected result")
        elif mode == "queued":
            status, body, _ = client.request("POST", f"/threads/{thread_path}/runs", payload)
            result = json_body(body)
            run_id = result.get("run_id") if isinstance(result, dict) else None
            if status not in (200, 201, 202) or not isinstance(run_id, str) or not run_id:
                raise AcceptanceError("queued run did not return a run ID")
            wait_for_run(client, thread_id, run_id, timeout)
        else:
            status, body, content_type = client.request(
                "POST", f"/threads/{thread_path}/runs/stream", payload, "text/event-stream"
            )
            if status != 200 or content_type != "text/event-stream":
                raise AcceptanceError("run stream did not return an SSE response")
            if not sse_has_data(body, expected, sensitive):
                raise AcceptanceError("SSE stream omitted expected data")

        assert_state(client, thread_id, expected, sensitive)
        checks.append({"case": name, "mode": mode, "thread_id": thread_id, "result": "PASS"})
    return checks


def read_scenario() -> dict[str, Any]:
    try:
        scenario = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise AcceptanceError("scenario stdin must contain valid JSON") from None
    if not isinstance(scenario, dict):
        raise AcceptanceError("scenario must be a JSON object")
    return scenario


def protocol_command(args):
    scenario = read_scenario()
    graph_id = scenario.get("graph_id")
    cases = scenario.get("cases")
    if not isinstance(graph_id, str) or not graph_id or not isinstance(cases, list) or not cases:
        raise AcceptanceError("scenario needs graph_id and at least one case")
    client = LASClient(os.environ.get("LAS_BASE_URL", ""), args.request_timeout)
    health_status, health_body, _ = client.request("GET", "/ok")
    health = json_body(health_body)
    if health_status != 200 or not isinstance(health, dict) or health.get("ok") is not True:
        raise AcceptanceError("Agent Server health check failed")
    all_checks = []
    for case in cases:
        if not isinstance(case, dict):
            raise AcceptanceError("scenario cases must be objects")
        all_checks.extend(run_protocol_case(client, graph_id, case, args.run_timeout))
    print(json.dumps({"result": "PASS", "checks": all_checks}, sort_keys=True))


def state_command(args):
    client = LASClient(os.environ.get("LAS_BASE_URL", ""), args.request_timeout)
    assert_state(client, args.thread_id, [args.expected_value], [])
    print(json.dumps({"result": "PASS", "check": "persisted_graph_state", "thread_id": args.thread_id}, sort_keys=True))


def submit_active_command(args):
    if not 1 <= args.cpu_seconds <= 15 or not 10 <= args.running_timeout <= 180:
        raise AcceptanceError("active run cpu-seconds must be 1..15 and running-timeout 10..180")
    client = LASClient(os.environ.get("LAS_BASE_URL", ""), args.request_timeout)
    marker = f"aip5411-active-{uuid.uuid4().hex}"
    thread_id = create_thread(client)
    payload = {"assistant_id": args.graph_id, "input": {"request": marker, "cpu_seconds": args.cpu_seconds}}
    status, body, _ = client.request("POST", f"/threads/{path_segment(thread_id)}/runs", payload)
    result = json_body(body)
    run_id = result.get("run_id") if isinstance(result, dict) else None
    if status not in (200, 201, 202) or not isinstance(run_id, str) or not run_id:
        raise AcceptanceError("active-run submission did not return a run ID")
    deadline = time.monotonic() + args.running_timeout
    while time.monotonic() < deadline:
        _, body, _ = client.request("GET", f"/threads/{path_segment(thread_id)}/runs/{path_segment(run_id)}")
        run = json_body(body)
        run_status = run.get("status") if isinstance(run, dict) else None
        if run_status == "running":
            print(json.dumps({"result": "PASS", "check": "active_run_submitted", "thread_id": thread_id, "run_id": run_id, "marker": marker}, sort_keys=True))
            return
        if run_status in TERMINAL:
            raise AcceptanceError("active run finished before queue replacement began")
        time.sleep(1)
    raise AcceptanceError("run did not enter running state before timeout")


def verify_run_command(args):
    client = LASClient(os.environ.get("LAS_BASE_URL", ""), args.request_timeout)
    wait_for_run(client, args.thread_id, args.run_id, args.run_timeout)
    assert_state(client, args.thread_id, [args.expected_marker], [])
    print(json.dumps({"result": "PASS", "check": "active_run_survived_queue_replacement", "thread_id": args.thread_id, "run_id": args.run_id}, sort_keys=True))


def kubectl_json(namespace: str, resource: str, name: str):
    import subprocess

    try:
        raw = subprocess.run(
            [os.environ.get("KUBECTL", "kubectl"), "-n", namespace, "get", resource, name, "-o", "json"],
            check=True, capture_output=True, text=True, timeout=20,
        ).stdout
        return json.loads(raw)
    except (OSError, subprocess.SubprocessError, json.JSONDecodeError):
        raise AcceptanceError(f"could not read Kubernetes {resource} {name}") from None


def hpa_observation(namespace: str, hpa_name: str, deployment_name: str):
    hpa = kubectl_json(namespace, "hpa", hpa_name)
    deployment = kubectl_json(namespace, "deployment", deployment_name)
    spec = hpa.get("spec", {})
    status = hpa.get("status", {})
    current_metrics = status.get("currentMetrics", [])
    cpu_metrics = [m for m in current_metrics if m.get("type") == "Resource" and m.get("resource", {}).get("name") == "cpu"]
    return {
        "min": spec.get("minReplicas", 1),
        "max": spec.get("maxReplicas"),
        "desired": status.get("desiredReplicas"),
        "current": status.get("currentReplicas"),
        "cpu_metrics_present": bool(cpu_metrics),
        "deployment_replicas": deployment.get("status", {}).get("readyReplicas", 0),
    }


def deployment_replicas(namespace: str, name: str):
    deployment = kubectl_json(namespace, "deployment", name)
    return {
        "desired": deployment.get("spec", {}).get("replicas", 1),
        "ready": deployment.get("status", {}).get("readyReplicas", 0),
    }


def scale_command(args):
    if os.environ.get("LAS_DISPOSABLE_CLUSTER_CONFIRMED") != "true":
        raise AcceptanceError("set LAS_DISPOSABLE_CLUSTER_CONFIRMED=true only for a disposable cluster")
    if args.workers < 1 or args.workers > 8 or not 1 <= args.cpu_seconds <= 15:
        raise AcceptanceError("workers must be 1..8 and cpu_seconds must be 1..15")
    if args.timeout < 30 or args.timeout > 900 or args.downscale_timeout < 300 or args.downscale_timeout > 1200:
        raise AcceptanceError("timeout must be 30..900 and downscale-timeout 300..1200 seconds")
    client = LASClient(os.environ.get("LAS_BASE_URL", ""), min(args.timeout, 60))
    before = hpa_observation(args.namespace, args.hpa, args.deployment)
    api_before = deployment_replicas(args.namespace, args.api_deployment)
    if api_before["desired"] != 1:
        raise AcceptanceError("LAS API Deployment is not configured for one replica")
    if before["max"] is None or before["max"] < before["min"]:
        raise AcceptanceError("HPA did not provide valid minimum and maximum bounds")
    if not before["cpu_metrics_present"]:
        raise AcceptanceError("HPA has no current CPU metrics")
    if args.workers > before["max"] * 4:
        raise AcceptanceError("workload exceeds the bounded worker-to-HPA ratio")

    def execute(index: int):
        name = f"scale-{index + 1}"
        marker = f"aip5411-{uuid.uuid4().hex}"
        case = {"name": name, "input": {"request": marker, "cpu_seconds": args.cpu_seconds}, "expected_state_values": [marker], "run_modes": ["sync"]}
        return run_protocol_case(client, args.graph_id, case, args.timeout)

    peak = before["current"] or 0
    last = before
    deadline = time.monotonic() + args.timeout
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
        futures = [pool.submit(execute, index) for index in range(args.workers)]
        while time.monotonic() < deadline and not all(future.done() for future in futures):
            last = hpa_observation(args.namespace, args.hpa, args.deployment)
            peak = max(peak, last["current"] or 0, last["desired"] or 0)
            time.sleep(5)
        for future in futures:
            future.result()
    if before["current"] is None or before["current"] >= before["max"]:
        raise AcceptanceError("start below the HPA maximum to qualify scale-up")
    if peak <= before["current"]:
        raise AcceptanceError("queue HPA did not scale above its starting replica count")
    if peak > last["max"]:
        raise AcceptanceError("observed queue replicas exceeded the HPA maximum")

    downscale_deadline = time.monotonic() + args.downscale_timeout
    while time.monotonic() < downscale_deadline:
        last = hpa_observation(args.namespace, args.hpa, args.deployment)
        api_now = deployment_replicas(args.namespace, args.api_deployment)
        if api_now["desired"] != 1:
            raise AcceptanceError("LAS API Deployment replica count changed during queue scaling")
        if (
            last["current"] == last["min"]
            and last["desired"] == last["min"]
            and last["deployment_replicas"] >= last["min"]
        ):
            break
        time.sleep(10)
    if last["current"] != last["min"] or last["desired"] != last["min"]:
        raise AcceptanceError("queue HPA did not return to its minimum before downscale timeout")
    if last["deployment_replicas"] < last["min"]:
        raise AcceptanceError("queue Deployment did not restore its HPA minimum ready workers")
    print(json.dumps({"result": "PASS", "check": "queue_scale_up_cap_downscale_floor_api_fixed", "workers": args.workers, "peak_replicas": peak, "hpa_min": last["min"], "hpa_max": last["max"], "ready_workers": last["deployment_replicas"], "api_replicas": api_now["desired"], "cpu_metrics_present": last["cpu_metrics_present"]}, sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    protocol = subparsers.add_parser("protocol", help="validate synchronous, queued, SSE, and state flows")
    protocol.add_argument("--request-timeout", type=float, default=45)
    protocol.add_argument("--run-timeout", type=float, default=300)
    protocol.set_defaults(func=protocol_command)
    state = subparsers.add_parser("state", help="verify a marker exists under persisted graph values")
    state.add_argument("--thread-id", required=True)
    state.add_argument("--expected-value", required=True)
    state.add_argument("--request-timeout", type=float, default=45)
    state.set_defaults(func=state_command)
    active = subparsers.add_parser("submit-active", help="start a bounded active queue run for replacement testing")
    active.add_argument("--graph-id", default="contract_fixture")
    active.add_argument("--cpu-seconds", type=int, default=15)
    active.add_argument("--running-timeout", type=int, default=45)
    active.add_argument("--request-timeout", type=float, default=45)
    active.set_defaults(func=submit_active_command)
    verify = subparsers.add_parser("verify-run", help="verify an active run completed and persisted its marker")
    verify.add_argument("--thread-id", required=True)
    verify.add_argument("--run-id", required=True)
    verify.add_argument("--expected-marker", required=True)
    verify.add_argument("--run-timeout", type=float, default=300)
    verify.add_argument("--request-timeout", type=float, default=45)
    verify.set_defaults(func=verify_run_command)
    scale = subparsers.add_parser("scale", help="submit bounded queue CPU load and read HPA observations")
    scale.add_argument("--namespace", required=True)
    scale.add_argument("--hpa", required=True)
    scale.add_argument("--deployment", required=True)
    scale.add_argument("--api-deployment", required=True)
    scale.add_argument("--graph-id", default="contract_fixture")
    scale.add_argument("--workers", type=int, default=4)
    scale.add_argument("--cpu-seconds", type=int, default=10)
    scale.add_argument("--timeout", type=int, default=300)
    scale.add_argument("--downscale-timeout", type=int, default=600)
    scale.set_defaults(func=scale_command)
    args = parser.parse_args()
    try:
        args.func(args)
    except AcceptanceError as exc:
        print(json.dumps({"result": "FAIL", "reason": str(exc)}, sort_keys=True), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
