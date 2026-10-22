"""Offline contract tests for the LAS runtime qualification helpers."""

import importlib.util
import pathlib
import unittest


SCRIPT = pathlib.Path(__file__).parents[2] / "scripts" / "las-runtime-acceptance.py"
SPEC = importlib.util.spec_from_file_location("las_runtime_acceptance", SCRIPT)
las = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(las)


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


if __name__ == "__main__":
    unittest.main()
