#!/usr/bin/env python3
"""Unit tests for deployment-managed LAS credentials and egress policy."""

import base64
import json
import subprocess
import unittest

import las_deployment as las


class FakeKubectl:
    def __init__(self):
        self.resources = {}
        self.calls = []
        self.fail_apply_name = None

    def __call__(self, args, input_text=None):
        self.calls.append((args, input_text))
        action = args[0]
        if action == "get":
            kind, name = args[1], args[2]
            namespace = args[args.index("-n") + 1]
            value = self.resources.get((kind, namespace, name))
            if value is None:
                return subprocess.CompletedProcess(args, 1, "", "Error from server (NotFound): not found")
            return subprocess.CompletedProcess(args, 0, json.dumps(value), "")
        if action == "apply":
            value = json.loads(input_text)
            metadata = value["metadata"]
            key = (value["kind"].lower(), metadata["namespace"], metadata["name"])
            if metadata["name"] == self.fail_apply_name:
                return subprocess.CompletedProcess(args, 1, "", "fixture apply failure")
            self.resources[key] = value
            return subprocess.CompletedProcess(args, 0, "applied", "")
        if action == "delete":
            kind, name = args[1], args[2]
            namespace = args[args.index("-n") + 1]
            self.resources.pop((kind.lower(), namespace, name), None)
            return subprocess.CompletedProcess(args, 0, "deleted", "")
        raise AssertionError(f"unexpected kubectl action: {args}")


class LASDeploymentTests(unittest.TestCase):
    def setUp(self):
        self.namespace = "ai-platform"
        self.platform = "example-ai-platform"
        self.config = {
            "aiPlatform": {
                "features": [
                    {
                        "name": "agentruntime",
                        "provider": "mltk",
                        "las": {
                            "credentials": {
                                "licenseKey": "fixture-license-value",
                                "postgresUri": "postgresql://fixture-user:fixture-password@postgres.internal/db",
                                "redisUri": "rediss://fixture-user:fixture-password@redis.internal/0",
                                "langsmithApiKey": "fixture-api-key",
                            }
                        },
                    }
                ]
            }
        }
        self.runner = FakeKubectl()

    def test_managed_secrets_use_stable_names_and_only_return_references(self):
        references = las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertTrue(references["managed"])
        self.assertEqual(set(references) - {"managed"}, {"licenseSecretRef", "postgresSecretRef", "redisSecretRef"})
        serialized = json.dumps(references)
        for secret_value in self.config["aiPlatform"]["features"][0]["las"]["credentials"].values():
            self.assertNotIn(secret_value, serialized)
        self.assertEqual(3, len([call for call in self.runner.calls if call[0][0] == "apply"]))
        self.assertTrue(all(len(name) <= 63 for name in references.values() if name != "managed"))

    def test_unchanged_reapply_does_not_update_secrets(self):
        las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        applies = len([call for call in self.runner.calls if call[0][0] == "apply"])
        las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertEqual(applies, len([call for call in self.runner.calls if call[0][0] == "apply"]))

    def test_removed_optional_api_key_is_removed_from_owned_auth_secret(self):
        references = las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.config["aiPlatform"]["features"][0]["las"]["credentials"].pop("langsmithApiKey")
        las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        auth = self.runner.resources[("secret", self.namespace, references["licenseSecretRef"])]
        keys = auth["data"]
        self.assertEqual({"langgraph_cloud_license_key"}, set(keys))

    def test_foreign_secret_is_not_adopted_or_overwritten(self):
        name = las.secret_name(self.namespace, self.platform, "agentruntime", "mltk", "auth")
        self.runner.resources[("secret", self.namespace, name)] = {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": name, "namespace": self.namespace, "labels": {}, "annotations": {}},
            "data": {},
        }
        with self.assertRaisesRegex(RuntimeError, "foreign LAS auth Secret"):
            las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertEqual(0, len([call for call in self.runner.calls if call[0][0] == "apply"]))

    def test_later_foreign_secret_is_detected_before_any_secret_is_written(self):
        name = las.secret_name(self.namespace, self.platform, "agentruntime", "mltk", "redis")
        self.runner.resources[("secret", self.namespace, name)] = {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": name, "namespace": self.namespace, "labels": {}, "annotations": {}},
            "data": {},
        }
        with self.assertRaisesRegex(RuntimeError, "foreign LAS redis Secret"):
            las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertEqual(0, len([call for call in self.runner.calls if call[0][0] == "apply"]))

    def test_partial_write_is_retained_and_rerun_converges(self):
        redis_name = las.secret_name(self.namespace, self.platform, "agentruntime", "mltk", "redis")
        self.runner.fail_apply_name = redis_name
        with self.assertRaisesRegex(RuntimeError, "could not apply LAS secret"):
            las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertTrue(any(key[0] == "secret" for key in self.runner.resources))
        self.runner.fail_apply_name = None
        references = las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertTrue(references["managed"])
        self.assertEqual(3, len([key for key in self.runner.resources if key[0] == "secret"]))

    def test_missing_managed_secret_is_recreated(self):
        references = las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        key = ("secret", self.namespace, references["postgresSecretRef"])
        del self.runner.resources[key]
        before = len([call for call in self.runner.calls if call[0][0] == "apply"])
        las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        after = len([call for call in self.runner.calls if call[0][0] == "apply"])
        self.assertEqual(before + 1, after)
        self.assertIn(key, self.runner.resources)

    def test_missing_required_credential_fails_before_any_secret_write(self):
        del self.config["aiPlatform"]["features"][0]["las"]["credentials"]["redisUri"]
        with self.assertRaisesRegex(ValueError, "las.credentials.redisUri is required"):
            las.provision_secrets(self.config, self.namespace, self.platform, self.runner)
        self.assertEqual([], self.runner.calls)

    def test_network_policy_normalizes_ips_and_selects_api_and_queue(self):
        config = {"cluster": {"egress": {"allowedIPs": ["10.20.30.40", "2001:db8::/48", "10.20.30.40/32"]}}}
        config["aiPlatform"] = self.config["aiPlatform"]
        las.reconcile_egress_policy(config, self.namespace, self.platform, self.runner)
        policies = [value for value in self.runner.resources.values() if value["kind"] == "NetworkPolicy"]
        self.assertEqual(1, len(policies))
        policy = policies[0]
        expression = policy["spec"]["podSelector"]["matchExpressions"][0]
        self.assertEqual(
            ["las-example-ai-platform-agentruntime-mltk-langgraph-cloud-api-server", "las-example-ai-platform-agentruntime-mltk-langgraph-cloud-queue"],
            expression["values"],
        )
        ip_blocks = [rule["to"][0]["ipBlock"]["cidr"] for rule in policy["spec"]["egress"] if "ipBlock" in rule["to"][0]]
        self.assertEqual(["10.20.30.40/32", "2001:db8::/48"], ip_blocks)

    def test_invalid_and_catch_all_egress_entries_are_rejected(self):
        self.config["cluster"] = {"egress": {"allowedIPs": ["not-an-ip"]}}
        with self.assertRaisesRegex(ValueError, "allowedIPs\[0\]"):
            las.reconcile_egress_policy(self.config, self.namespace, self.platform, self.runner)
        self.config["cluster"]["egress"]["allowedIPs"] = ["0.0.0.0/0"]
        with self.assertRaisesRegex(ValueError, "catch-all"):
            las.reconcile_egress_policy(self.config, self.namespace, self.platform, self.runner)
        self.assertEqual([], self.runner.calls)

    def test_allowlist_update_reconciles_owned_policy(self):
        config = {"cluster": {"egress": {"allowedIPs": ["10.0.0.1"]}}}
        config["aiPlatform"] = self.config["aiPlatform"]
        las.reconcile_egress_policy(config, self.namespace, self.platform, self.runner)
        config["cluster"]["egress"]["allowedIPs"] = ["10.0.0.2"]
        before = len([call for call in self.runner.calls if call[0][0] == "apply"])
        las.reconcile_egress_policy(config, self.namespace, self.platform, self.runner)
        after = len([call for call in self.runner.calls if call[0][0] == "apply"])
        self.assertEqual(before + 1, after)
        policy = next(value for value in self.runner.resources.values() if value["kind"] == "NetworkPolicy")
        ip_blocks = [rule["to"][0]["ipBlock"]["cidr"] for rule in policy["spec"]["egress"] if "ipBlock" in rule["to"][0]]
        self.assertEqual(["10.0.0.2/32"], ip_blocks)

    def test_omitted_allowlist_removes_only_the_owned_policy(self):
        configured = dict(self.config)
        configured["cluster"] = {"egress": {"allowedIPs": ["10.0.0.1"]}}
        las.reconcile_egress_policy(configured, self.namespace, self.platform, self.runner)
        las.reconcile_egress_policy(self.config, self.namespace, self.platform, self.runner)
        self.assertFalse(any(value["kind"] == "NetworkPolicy" for value in self.runner.resources.values()))

    def test_registry_secret_manifest_has_durable_docker_auth_without_apply_annotation(self):
        manifest = las.registry_secret_manifest(
            self.namespace, "ecr-registry-secret", "123456789012.dkr.ecr.us-west-2.amazonaws.com", "AWS", "fixture-token"
        )
        self.assertEqual("kubernetes.io/dockerconfigjson", manifest["type"])
        docker_config = json.loads(base64.b64decode(manifest["data"][".dockerconfigjson"]))
        self.assertEqual("fixture-token", docker_config["auths"]["123456789012.dkr.ecr.us-west-2.amazonaws.com"]["password"])
        self.assertNotIn("last-applied-configuration", manifest["metadata"].get("annotations", {}))
        self.assertNotIn("fixture-token", json.dumps({k: v for k, v in manifest.items() if k != "data"}))


if __name__ == "__main__":
    unittest.main()
