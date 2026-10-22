"""Offline contract tests for the LAS runtime qualification helpers."""

import importlib.util
import pathlib
import unittest
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

    def test_product_result_matches_result_field_not_unrelated_event_text(self):
        with self.assertRaisesRegex(AssertionError, "result field"):
            product.assert_expected_result([{"message": "expected output"}], "expected output", "product case")
        product.assert_expected_result([{"result": "the expected output"}], "expected output", "product case")


if __name__ == "__main__":
    unittest.main()
