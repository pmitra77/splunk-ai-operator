"""Offline contract tests for the LAS runtime qualification helpers."""

import asyncio
import importlib.util
import io
import json
import pathlib
import sys
import types
import unittest
from types import SimpleNamespace
from unittest.mock import patch


SCRIPT = pathlib.Path(__file__).parents[2] / "scripts" / "las-runtime-acceptance.py"
SPEC = importlib.util.spec_from_file_location("las_runtime_acceptance", SCRIPT)
las = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(las)

PRODUCT_SCRIPT = pathlib.Path(__file__).parent / "validation" / "aip5410_product_route_acceptance.py"
PRODUCT_SPEC = importlib.util.spec_from_file_location("aip5410_product_route_acceptance", PRODUCT_SCRIPT)
product = importlib.util.module_from_spec(PRODUCT_SPEC)
PRODUCT_SPEC.loader.exec_module(product)


class FakeClient:
    def __init__(self, payload):
        self.payload = payload

    def request(self, method, path):
        return 200, __import__("json").dumps(self.payload).encode(), "application/json"


class LASRuntimeAcceptanceTests(unittest.TestCase):
    def test_persistence_marker_must_be_inside_graph_values(self):
        marker = "run-marker"
        client = FakeClient({"values": {"response": "completed"}, "metadata": {"marker": marker}})
        with self.assertRaisesRegex(las.AcceptanceError, "graph state values"):
            las.assert_state(client, "thread", [marker], [])

    def test_persisted_marker_under_values_passes(self):
        marker = "run-marker"
        client = FakeClient({"values": {"response": f"persisted {marker}"}, "metadata": {}})
        las.assert_state(client, "thread", [marker], [])

    def test_sensitive_markers_are_rejected_from_state_values(self):
        client = FakeClient({"values": {"response": "secret-value"}})
        with self.assertRaisesRegex(las.AcceptanceError, "sensitive marker"):
            las.assert_state(client, "thread", [], ["secret-value"])

    def test_sse_parser_checks_data_lines_only(self):
        self.assertTrue(las.sse_has_data(b"event: values\ndata: {\"response\":\"expected\"}\n\n", ["expected"], []))
        self.assertFalse(las.sse_has_data(b"event: values\ndata: {\"response\":\"other\"}\n\n", ["expected"], []))

    def test_sse_parser_rejects_sensitive_values(self):
        with self.assertRaisesRegex(las.AcceptanceError, "sensitive marker"):
            las.sse_has_data(b"data: secret-value\n\n", [], ["secret-value"])

    def test_client_rejects_url_userinfo_and_query(self):
        for url in ("https://user:pass@example.test", "https://example.test?token=value"):
            with self.subTest(url=url), self.assertRaises(las.AcceptanceError):
                las.LASClient(url, 2)

    def test_hpa_observation_rejects_a_different_scale_target(self):
        hpa = {"spec": {"scaleTargetRef": {"kind": "Deployment", "name": "other-queue"}}}
        with patch.object(las, "kubectl_json", return_value=hpa) as get:
            with self.assertRaisesRegex(las.AcceptanceError, "scale target"):
                las.hpa_observation("ns", "queue-hpa", "expected-queue")
        self.assertEqual(get.call_count, 1)

    def test_hpa_observation_accepts_the_requested_deployment_target(self):
        hpa = {
            "spec": {"scaleTargetRef": {"kind": "Deployment", "name": "queue"}, "minReplicas": 1, "maxReplicas": 3},
            "status": {"currentReplicas": 1, "desiredReplicas": 1, "currentMetrics": [{"type": "Resource", "resource": {"name": "cpu"}}]},
        }
        deployment = {"status": {"readyReplicas": 1}}
        with patch.object(las, "kubectl_json", side_effect=[hpa, deployment]):
            observation = las.hpa_observation("ns", "queue-hpa", "queue")
        self.assertEqual(observation["deployment_replicas"], 1)

    def test_scale_waits_for_a_ready_replica_after_runs_finish(self):
        class CompletedFuture:
            def done(self):
                return True

            def result(self):
                return None

        observations = iter([
            {"current": 2, "desired": 2, "deployment_replicas": 1},
            {"current": 2, "desired": 2, "deployment_replicas": 2},
        ])
        with patch.object(las.time, "monotonic", return_value=0), patch.object(las.time, "sleep"):
            peak_hpa, peak_ready, _ = las.wait_for_ready_scale_up(
                lambda: next(observations), [CompletedFuture()], 1, 1, 30
            )
        self.assertEqual((peak_hpa, peak_ready), (2, 2))

    def test_desired_replicas_without_ready_worker_does_not_pass_scale_up(self):
        class CompletedFuture:
            def done(self):
                return True

            def result(self):
                return None

        with patch.object(las.time, "monotonic", side_effect=[0, 0, 0, 2]), patch.object(las.time, "sleep"):
            with self.assertRaisesRegex(las.AcceptanceError, "ready worker"):
                las.wait_for_ready_scale_up(
                    lambda: {"current": 2, "desired": 2, "deployment_replicas": 1},
                    [CompletedFuture()], 1, 1, 1,
                )

    def test_product_result_requires_nonempty_expected_text(self):
        for expected in (None, "", "   "):
            with self.subTest(expected=expected), self.assertRaisesRegex(AssertionError, "non-empty"):
                product.assert_expected_result([], expected, "product case")

    def test_product_result_requires_completed_caller_response(self):
        for event in (
            {"status": "Thinking", "response": "expected output"},
            {"status": "Completed", "result": "expected output"},
            {"status": "Completed", "message": "expected output"},
        ):
            with self.subTest(event=event), self.assertRaisesRegex(AssertionError, "completed caller response"):
                product.assert_expected_result([event], "expected output", "product case")
        product.assert_expected_result(
            [{"status": "Completed", "response": "the expected output"}], "expected output", "product case"
        )

    def test_product_case_uses_valid_name_mapped_thread_and_retains_redaction_gate(self):
        config = {
            "sensitive_values": ["test-canary"],
            "cases": [{"name": "mock", "calls": [{"task_prompt": "test", "expected_text": "expected output"}]}],
        }

        for leak in (False, True):
            with self.subTest(checkpoint_contains_canary=leak):
                created_names = []
                invoked_payloads = []
                state_ids = []
                history_ids = []
                deleted_ids = []

                async def create_agent(tenant, payload, request):
                    created_names.append(payload.agent_name)
                    return SimpleNamespace(status_code=201, body=b'{"runtime_id":"assistant-id"}')

                async def delete_agent(tenant, assistant_id, request):
                    deleted_ids.append(assistant_id)
                    return SimpleNamespace(status_code=200)

                async def invoke(tenant, payload, request_id):
                    invoked_payloads.append(payload)
                    return 200, [{"status": "Completed", "success": True, "response": "expected output"}], {}

                async def get_state(thread_id):
                    state_ids.append(thread_id)
                    return {"values": {"messages": [{"content": "expected output"}]}}

                async def all_checkpoints(client, thread_id):
                    history_ids.append(thread_id)
                    return [{"metadata": {"llm_config": {"api_key": "test-canary" if leak else "redacted"}}}]

                modules = {}
                for name in (
                    "slim_api", "slim_api.models", "slim_api.models.aws", "slim_api.routers",
                    "slim_api.routers.las_agent", "slim_api.util", "slim_api.util.las_client",
                    "slim_api.util.las_utils",
                ):
                    modules[name] = types.ModuleType(name)
                    parent, _, child = name.rpartition(".")
                    if parent:
                        setattr(modules[parent], child, modules[name])
                modules["slim_api.models.aws"].CreateAgentPayload = lambda **kwargs: SimpleNamespace(**kwargs)
                modules["slim_api.routers.las_agent"].create_agent = create_agent
                modules["slim_api.routers.las_agent"].delete_agent = delete_agent
                modules["slim_api.util.las_client"].get_las_client = lambda: SimpleNamespace(
                    threads=SimpleNamespace(get_state=get_state)
                )
                modules["slim_api.util.las_utils"].get_proper_thread_id = (
                    lambda tenant, session_id: f"mapped:{tenant}:{session_id}"
                )

                output = io.StringIO()
                with patch.dict(sys.modules, modules), patch.object(product, "invoke", invoke), patch.object(
                    product, "all_checkpoints", all_checkpoints
                ), patch("sys.stdin", io.StringIO(json.dumps(config))), patch("sys.stdout", output):
                    if leak:
                        with self.assertRaisesRegex(AssertionError, "checkpoint history"):
                            asyncio.run(product.main())
                    else:
                        asyncio.run(product.main())

                self.assertRegex(created_names[0], r"^aip5410[A-Za-z0-9]+$")
                self.assertEqual(invoked_payloads[0]["agent_id"], "assistant-id")
                expected_thread = f"mapped:product-qualification:{invoked_payloads[0]['session_id']}"
                self.assertEqual(state_ids, [expected_thread])
                self.assertEqual(history_ids, [expected_thread])
                self.assertEqual(deleted_ids, ["assistant-id"])
                if not leak:
                    self.assertEqual(json.loads(output.getvalue())["result"], "PASS")


if __name__ == "__main__":
    unittest.main()
