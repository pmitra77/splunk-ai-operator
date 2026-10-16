#!/usr/bin/env python3
"""Deployment-only LAS credential and egress helpers.

Credential payloads are read from the deployment file, passed to kubectl over
stdin, and never printed or placed in command-line arguments. Secret writes use
server-side apply so credentials are not copied into last-applied annotations.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import ipaddress
import json
import os
import re
import shlex
import subprocess
import sys
from typing import Any, Callable

MANAGED_BY = "splunk-ai-operator-deployment"
DEPLOYMENT_ID_LABEL = "ai.splunk.com/deployment-id"
CREDENTIAL_ROLE_LABEL = "ai.splunk.com/credential-role"
IDENTITY_ANNOTATION = "ai.splunk.com/deployment-identity"
FIELD_MANAGER = MANAGED_BY

Runner = Callable[..., subprocess.CompletedProcess[str]]


def canonical_identity(parts: list[str]) -> str:
    return json.dumps(parts, ensure_ascii=False, separators=(",", ":"))


def deployment_id(namespace: str, platform: str, feature: str, provider: str) -> str:
    identity = canonical_identity([namespace, platform, feature, provider])
    return hashlib.sha256(identity.encode()).hexdigest()[:16]


def _dns_part(value: str) -> str:
    normalized = re.sub(r"[^a-z0-9-]+", "-", value.lower()).strip("-")
    return normalized or "platform"


def secret_name(namespace: str, platform: str, feature: str, provider: str, role: str) -> str:
    identity = canonical_identity([namespace, platform, feature, provider, role])
    suffix = hashlib.sha256(identity.encode()).hexdigest()[:8]
    readable = _dns_part(f"las-{platform}-{provider}-{role}")
    readable = readable[: 63 - len(suffix) - 1].rstrip("-")
    return f"{readable}-{suffix}"


def _find_las_feature(config: dict[str, Any]) -> dict[str, Any] | None:
    features = config.get("aiPlatform", {}).get("features", [])
    if not isinstance(features, list):
        raise ValueError("aiPlatform.features must be a list")
    matches = [item for item in features if isinstance(item, dict) and item.get("name") == "agentruntime"]
    if len(matches) > 1:
        raise ValueError("only one agentruntime feature is supported by this deployment")
    return matches[0] if matches else None


def _validate_credentials(feature: dict[str, Any]) -> dict[str, str] | None:
    las_config = feature.get("las", {})
    if las_config is None:
        las_config = {}
    if not isinstance(las_config, dict):
        raise ValueError("aiPlatform.features[].las must be a mapping")
    credentials = las_config.get("credentials")
    if credentials is None:
        return None
    if not isinstance(credentials, dict):
        raise ValueError("aiPlatform.features[].las.credentials must be a mapping")
    mapping = {
        "licenseKey": "langgraph_cloud_license_key",
        "postgresUri": "postgres_connection_url",
        "redisUri": "redis_connection_url",
    }
    result: dict[str, str] = {}
    for source, target in mapping.items():
        value = credentials.get(source)
        if not isinstance(value, str) or not value:
            raise ValueError(f"LAS credential field las.credentials.{source} is required")
        result[target] = value
    api_key = credentials.get("langsmithApiKey")
    if api_key is not None:
        if not isinstance(api_key, str):
            raise ValueError("LAS credential field las.credentials.langsmithApiKey must be a string")
        if api_key:
            result["api_key"] = api_key
    unknown = set(credentials) - {"licenseKey", "postgresUri", "redisUri", "langsmithApiKey"}
    if unknown:
        raise ValueError("LAS credentials contain an unsupported field")
    return result


def _secret_manifest(
    namespace: str,
    platform: str,
    feature: str,
    provider: str,
    role: str,
    data: dict[str, str],
) -> dict[str, Any]:
    dep_id = deployment_id(namespace, platform, feature, provider)
    identity = canonical_identity([namespace, platform, feature, provider])
    return {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": secret_name(namespace, platform, feature, provider, role),
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/managed-by": MANAGED_BY,
                DEPLOYMENT_ID_LABEL: dep_id,
                CREDENTIAL_ROLE_LABEL: role,
            },
            "annotations": {IDENTITY_ANNOTATION: identity},
        },
        "type": "Opaque",
        "data": {key: base64.b64encode(value.encode()).decode() for key, value in data.items()},
    }


def _command_prefix() -> list[str]:
    return shlex.split(os.environ.get("KUBECTL", "kubectl"))


def _kubectl(args: list[str], input_text: str | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        _command_prefix() + args,
        input=input_text,
        text=True,
        capture_output=True,
        check=False,
    )


def _is_not_found(result: subprocess.CompletedProcess[str]) -> bool:
    message = f"{result.stdout}\n{result.stderr}".lower()
    return "notfound" in message or "not found" in message


def _read_object(kind: str, name: str, namespace: str, runner: Runner) -> dict[str, Any] | None:
    result = runner(["get", kind, name, "-n", namespace, "-o", "json"])
    if result.returncode:
        if _is_not_found(result):
            return None
        raise RuntimeError(f"could not inspect LAS {kind} {name}")
    try:
        value = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"could not decode LAS {kind} {name} response") from exc
    if not isinstance(value, dict):
        raise RuntimeError(f"invalid LAS {kind} {name} response")
    return value


def _managed_by_identity(obj: dict[str, Any], dep_id: str, identity: str, role: str | None = None) -> bool:
    metadata = obj.get("metadata", {})
    labels = metadata.get("labels", {})
    annotations = metadata.get("annotations", {})
    return (
        labels.get("app.kubernetes.io/managed-by") == MANAGED_BY
        and labels.get(DEPLOYMENT_ID_LABEL) == dep_id
        and annotations.get(IDENTITY_ANNOTATION) == identity
        and (role is None or labels.get(CREDENTIAL_ROLE_LABEL) == role)
    )


def _apply_object(kind: str, name: str, namespace: str, manifest: dict[str, Any], runner: Runner) -> None:
    result = runner(
        ["apply", "--server-side", f"--field-manager={FIELD_MANAGER}", "-f", "-"],
        json.dumps(manifest, separators=(",", ":")),
    )
    if result.returncode:
        raise RuntimeError(f"could not apply LAS {kind} {name}")


def provision_secrets(
    config: dict[str, Any], namespace: str, platform: str, runner: Runner = _kubectl
) -> dict[str, Any]:
    feature = _find_las_feature(config)
    if feature is None:
        return {"managed": False}
    values = _validate_credentials(feature)
    if values is None:
        return {"managed": False}
    feature_name = "agentruntime"
    provider = feature.get("provider")
    if not isinstance(provider, str) or not provider:
        raise ValueError("aiPlatform.features[].provider is required for managed LAS credentials")
    # Validate all deployment-only inputs before the first Kubernetes write.
    _normalize_allowed_ips(config)

    role_data = {
        "auth": {key: value for key, value in values.items() if key in {"langgraph_cloud_license_key", "api_key"}},
        "postgres": {"postgres_connection_url": values["postgres_connection_url"]},
        "redis": {"redis_connection_url": values["redis_connection_url"]},
    }
    dep_id = deployment_id(namespace, platform, feature_name, provider)
    identity = canonical_identity([namespace, platform, feature_name, provider])
    names: dict[str, str] = {}
    existing_by_role: dict[str, dict[str, Any] | None] = {}

    # Preflight the complete set before the first write. This avoids a foreign
    # or malformed later Secret being discovered after earlier credentials
    # have already been rotated.
    for role, data in role_data.items():
        manifest = _secret_manifest(namespace, platform, feature_name, provider, role, data)
        name = manifest["metadata"]["name"]
        existing = _read_object("secret", name, namespace, runner)
        if existing is not None:
            if not _managed_by_identity(existing, dep_id, identity, role):
                raise RuntimeError(f"refusing to overwrite foreign LAS {role} Secret")
            existing_data = existing.get("data", {})
            allowed_keys = {"langgraph_cloud_license_key", "api_key"} if role == "auth" else set(data)
            if existing.get("type") != "Opaque" or existing.get("immutable") is True or not isinstance(existing_data, dict) or set(existing_data) - allowed_keys:
                raise RuntimeError(f"managed LAS {role} Secret has an unexpected schema")
        existing_by_role[role] = existing

    for role, data in role_data.items():
        manifest = _secret_manifest(namespace, platform, feature_name, provider, role, data)
        name = manifest["metadata"]["name"]
        existing = existing_by_role[role]
        if existing is not None:
            existing_data = existing.get("data", {})
            desired_data = {
                key: base64.b64encode(value.encode()).decode() for key, value in data.items()
            }
            if existing_data == desired_data:
                names[role] = name
                continue
        _apply_object("secret", name, namespace, manifest, runner)
        applied = _read_object("secret", name, namespace, runner)
        expected_data = {key: base64.b64encode(value.encode()).decode() for key, value in data.items()}
        if applied is None or not _managed_by_identity(applied, dep_id, identity, role) or applied.get("type") != "Opaque" or applied.get("data", {}) != expected_data:
            raise RuntimeError(f"could not verify managed LAS {role} Secret")
        names[role] = name

    return {
        "managed": True,
        "licenseSecretRef": names["auth"],
        "postgresSecretRef": names["postgres"],
        "redisSecretRef": names["redis"],
    }


def _normalize_allowed_ips(config: dict[str, Any]) -> list[str] | None:
    if "cluster" not in config:
        return None
    cluster = config["cluster"]
    if not isinstance(cluster, dict):
        raise ValueError("cluster must be a mapping")
    if "egress" not in cluster:
        return None
    egress = cluster["egress"]
    if not isinstance(egress, dict):
        raise ValueError("cluster.egress must be a mapping")
    if "allowedIPs" not in egress:
        return None
    raw = egress["allowedIPs"]
    if not isinstance(raw, list):
        raise ValueError("cluster.egress.allowedIPs must be a list")
    networks: set[ipaddress.IPv4Network | ipaddress.IPv6Network] = set()
    for index, value in enumerate(raw):
        if not isinstance(value, str) or not value.strip():
            raise ValueError(f"cluster.egress.allowedIPs[{index}] must be an IP address or CIDR")
        try:
            if "/" not in value:
                address = ipaddress.ip_address(value)
                network = ipaddress.ip_network(f"{address}/{address.max_prefixlen}")
            else:
                network = ipaddress.ip_network(value, strict=False)
        except ValueError as exc:
            raise ValueError(f"cluster.egress.allowedIPs[{index}] must be an IP address or CIDR") from exc
        if network.prefixlen == 0:
            raise ValueError(f"cluster.egress.allowedIPs[{index}] cannot be a catch-all network")
        networks.add(network)
    return [str(item) for item in sorted(networks, key=lambda n: (n.version, int(n.network_address), n.prefixlen))]


def _fnv32a(value: str) -> int:
    result = 2166136261
    for byte in value.encode():
        result ^= byte
        result = (result * 16777619) & 0xFFFFFFFF
    return result


def _ai_service_name(platform: str, feature: str, provider: str) -> str:
    name = f"{platform}-{feature}-{provider}"
    if len(name) <= 63:
        return name
    digest = f"{_fnv32a(name):08x}"
    return f"{name[: 63 - len(digest) - 1].rstrip('-')}-{digest}"


def _release_name(service: str) -> str:
    name = f"las-{service}"
    if len(name) <= 34:
        return name
    digest = hashlib.sha256(service.encode()).hexdigest()[:8]
    return f"{name[:25]}-{digest}"


def network_policy_manifest(
    namespace: str,
    platform: str,
    feature: str,
    provider: str,
    allowed_ips: list[str],
) -> dict[str, Any]:
    dep_id = deployment_id(namespace, platform, feature, provider)
    identity = canonical_identity([namespace, platform, feature, provider])
    release = _release_name(_ai_service_name(platform, feature, provider))
    fullname = f"{release}-langgraph-cloud"
    egress: list[dict[str, Any]] = [
        {"to": [{"namespaceSelector": {}, "podSelector": {}}]},
        {
            "to": [
                {
                    "namespaceSelector": {
                        "matchLabels": {"kubernetes.io/metadata.name": "kube-system"}
                    },
                    "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}},
                }
            ],
            "ports": [
                {"protocol": "UDP", "port": 53},
                {"protocol": "TCP", "port": 53},
            ],
        },
    ]
    egress.extend({"to": [{"ipBlock": {"cidr": cidr}}]} for cidr in allowed_ips)
    return {
        "apiVersion": "networking.k8s.io/v1",
        "kind": "NetworkPolicy",
        "metadata": {
            "name": f"las-egress-{dep_id}",
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/managed-by": MANAGED_BY,
                DEPLOYMENT_ID_LABEL: dep_id,
            },
            "annotations": {IDENTITY_ANNOTATION: identity},
        },
        "spec": {
            "podSelector": {
                "matchLabels": {"app.kubernetes.io/instance": release},
                "matchExpressions": [
                    {
                        "key": "app.kubernetes.io/component",
                        "operator": "In",
                        "values": [
                            f"{fullname}-api-server",
                            f"{fullname}-queue",
                        ],
                    }
                ],
            },
            "policyTypes": ["Egress"],
            "egress": egress,
        },
    }


def _comparable_object(obj: dict[str, Any]) -> dict[str, Any]:
    metadata = obj.get("metadata", {})
    return {
        "apiVersion": obj.get("apiVersion"),
        "kind": obj.get("kind"),
        "metadata": {
            "name": metadata.get("name"),
            "namespace": metadata.get("namespace"),
            "labels": metadata.get("labels", {}),
            "annotations": metadata.get("annotations", {}),
        },
        "spec": obj.get("spec", {}),
    }


def reconcile_egress_policy(
    config: dict[str, Any], namespace: str, platform: str, runner: Runner = _kubectl
) -> None:
    feature = _find_las_feature(config)
    if feature is None:
        return
    provider = feature.get("provider")
    if not isinstance(provider, str) or not provider:
        raise ValueError("aiPlatform.features[].provider is required for LAS egress policy")
    allowed_ips = _normalize_allowed_ips(config)
    desired = None if allowed_ips is None else network_policy_manifest(
        namespace, platform, "agentruntime", provider, allowed_ips
    )
    name = f"las-egress-{deployment_id(namespace, platform, 'agentruntime', provider)}"
    existing = _read_object("networkpolicy", name, namespace, runner)
    if existing is None:
        if desired is not None:
            _apply_object("networkpolicy", name, namespace, desired, runner)
        return

    dep_id = deployment_id(namespace, platform, "agentruntime", provider)
    identity = canonical_identity([namespace, platform, "agentruntime", provider])
    if not _managed_by_identity(existing, dep_id, identity):
        raise RuntimeError("refusing to replace foreign LAS egress NetworkPolicy")
    if desired is None:
        result = runner(["delete", "networkpolicy", name, "-n", namespace])
        if result.returncode and not _is_not_found(result):
            raise RuntimeError("could not remove managed LAS egress NetworkPolicy")
        return
    if _comparable_object(existing) != _comparable_object(desired):
        _apply_object("networkpolicy", name, namespace, desired, runner)


def registry_secret_manifest(namespace: str, name: str, server: str, username: str, token: str) -> dict[str, Any]:
    # AWS CLI prints the token followed by a line delimiter. Docker's
    # --password-stdin removes it; normalize here before encoding the Secret.
    token = token.rstrip("\r\n")
    if not token:
        raise ValueError("registry token is empty")
    auth = base64.b64encode(f"{username}:{token}".encode()).decode()
    config = {"auths": {server: {"username": username, "password": token, "auth": auth}}}
    docker_config = json.dumps(config, separators=(",", ":")).encode()
    return {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {"app.kubernetes.io/managed-by": MANAGED_BY},
        },
        "type": "kubernetes.io/dockerconfigjson",
        "data": {".dockerconfigjson": base64.b64encode(docker_config).decode()},
    }


def _read_config(path: str) -> dict[str, Any]:
    result = subprocess.run(["yq", "-o=json", ".", path], text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError("could not read deployment configuration")
    try:
        value = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError("deployment configuration did not render as JSON") from exc
    if not isinstance(value, dict):
        raise RuntimeError("deployment configuration must be a mapping")
    return value


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for command in ("provision-secrets", "reconcile-egress"):
        item = commands.add_parser(command)
        item.add_argument("--config", required=True)
        item.add_argument("--namespace", required=True)
        item.add_argument("--platform", required=True)
    registry = commands.add_parser("registry-secret")
    registry.add_argument("--namespace", required=True)
    registry.add_argument("--name", required=True)
    registry.add_argument("--server", required=True)
    registry.add_argument("--username", default="AWS")
    args = parser.parse_args(argv)
    try:
        if args.command == "registry-secret":
            token = sys.stdin.read()
            if not token:
                raise ValueError("registry token is empty")
            json.dump(registry_secret_manifest(args.namespace, args.name, args.server, args.username, token), sys.stdout, separators=(",", ":"))
            return 0
        config = _read_config(args.config)
        if args.command == "provision-secrets":
            result = provision_secrets(config, args.namespace, args.platform)
            json.dump(result, sys.stdout, separators=(",", ":"))
            return 0
        reconcile_egress_policy(config, args.namespace, args.platform)
        return 0
    except (ValueError, RuntimeError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
