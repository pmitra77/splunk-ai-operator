#!/usr/bin/env bash

# Disposable-cluster acceptance check for deployment-config credential rotation.
# This performs two full installer runs and a real LAS thread/run/state check.
set -Eeuo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/las-credential-rotation-acceptance.sh

Required environment:
  LAS_DISPOSABLE_CLUSTER_CONFIRMED=true
  LAS_INITIAL_CONFIG                 Config with initial test credentials.
  LAS_ROTATED_CONFIG                 Config with rotated test credentials.
  LAS_NAMESPACE                      Namespace containing the LAS release.
  LAS_SECRET_NAMES                   Comma-separated names of the managed LAS
                                     auth, PostgreSQL, and Redis Secrets.
  LAS_API_DEPLOYMENT                 LAS API Deployment name.
  LAS_QUEUE_DEPLOYMENT               LAS queue Deployment name.
  LAS_BASE_URL and image/chart metadata required by las-baseline-smoke.sh.

Optional:
  LAS_INSTALLER                      Defaults to tools/cluster_setup/
                                     k0s_cluster_with_stack.sh.
  KUBECTL                            Defaults to "kubectl".
  ROLLOUT_TIMEOUT                    Defaults to 300s.

This changes cluster resources by running the installer twice. Use only with
disposable test credentials and a disposable cluster. Output contains names,
resource versions, and smoke metadata; it does not print credential values.
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

required=(
  LAS_DISPOSABLE_CLUSTER_CONFIRMED LAS_INITIAL_CONFIG LAS_ROTATED_CONFIG
  LAS_NAMESPACE LAS_SECRET_NAMES LAS_API_DEPLOYMENT LAS_QUEUE_DEPLOYMENT
  LAS_BASE_URL LAS_OPERATOR_IMAGE LAS_OPERATOR_IMAGE_DIGEST LAS_CHART_VERSION
  LAS_CHART_SHA256 LAS_AGENT_SERVER_IMAGE LAS_AGENT_SERVER_DIGEST
)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    printf 'Missing required environment variable: %s\n' "$name" >&2
    usage >&2
    exit 2
  fi
done
if [[ "$LAS_DISPOSABLE_CLUSTER_CONFIRMED" != "true" ]]; then
  printf 'Set LAS_DISPOSABLE_CLUSTER_CONFIRMED=true only for a disposable cluster.\n' >&2
  exit 2
fi
if [[ ! -r "$LAS_INITIAL_CONFIG" || ! -r "$LAS_ROTATED_CONFIG" ]]; then
  printf 'Both LAS config files must be readable.\n' >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
INSTALLER="${LAS_INSTALLER:-${REPO_ROOT}/tools/cluster_setup/k0s_cluster_with_stack.sh}"
KUBECTL="${KUBECTL:-kubectl}"
IFS=',' read -r -a SECRET_NAMES <<< "$LAS_SECRET_NAMES"
if (( ${#SECRET_NAMES[@]} != 3 )); then
  printf 'LAS_SECRET_NAMES must list exactly the auth, PostgreSQL, and Redis Secret names.\n' >&2
  exit 2
fi

command -v "$KUBECTL" >/dev/null 2>&1 || { printf 'kubectl not found: %s\n' "$KUBECTL" >&2; exit 2; }
[[ -x "$INSTALLER" ]] || { printf 'Installer is not executable: %s\n' "$INSTALLER" >&2; exit 2; }
[[ -x "${SCRIPT_DIR}/las-baseline-smoke.sh" ]] || { printf 'Baseline smoke script is not executable.\n' >&2; exit 2; }

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/las-rotation-acceptance.XXXXXX")"
cleanup() { rm -rf "$WORK_DIR"; }
trap cleanup EXIT

resource_version() {
  "$KUBECTL" -n "$LAS_NAMESPACE" get secret "$1" -o 'jsonpath={.metadata.resourceVersion}'
}

deployment_revision() {
  "$KUBECTL" -n "$LAS_NAMESPACE" get deployment "$1" -o json \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("metadata",{}).get("annotations",{}).get("deployment.kubernetes.io/revision", ""))'
}

install_config() {
  local config="$1" log_file="$2"
  if ! AUTO_APPROVE=true CONFIG_FILE="$config" "$INSTALLER" install --silent >"$log_file" 2>&1; then
    printf 'Installer failed for a LAS configuration (details withheld to avoid credential leakage).\n' >&2
    return 1
  fi
}

wait_rollout() {
  local deployment="$1"
  "$KUBECTL" -n "$LAS_NAMESPACE" rollout status "deployment/${deployment}" \
    --timeout="${ROLLOUT_TIMEOUT:-300s}" >/dev/null
  printf 'rollout=%s:PASS\n' "$deployment"
}

printf 'phase=install_initial\n'
install_config "$LAS_INITIAL_CONFIG" "$WORK_DIR/initial-installer.log"
declare -a initial_versions=()
for secret in "${SECRET_NAMES[@]}"; do
  version="$(resource_version "$secret")" || { printf 'Could not read managed Secret metadata.\n' >&2; exit 1; }
  [[ -n "$version" ]] || { printf 'Managed Secret has no resource version.\n' >&2; exit 1; }
  initial_versions+=("$version")
done
initial_api_revision="$(deployment_revision "$LAS_API_DEPLOYMENT")"
initial_queue_revision="$(deployment_revision "$LAS_QUEUE_DEPLOYMENT")"
if [[ -z "$initial_api_revision" || -z "$initial_queue_revision" ]]; then
  printf 'Could not read the initial LAS Deployment revisions.\n' >&2
  exit 1
fi

printf 'phase=install_rotated\n'
install_config "$LAS_ROTATED_CONFIG" "$WORK_DIR/rotated-installer.log"
changed=0
for index in "${!SECRET_NAMES[@]}"; do
  version="$(resource_version "${SECRET_NAMES[$index]}")" || { printf 'Could not read managed Secret metadata after rotation.\n' >&2; exit 1; }
  if [[ "$version" == "${initial_versions[$index]}" ]]; then
    printf 'secret_rotation=%s:FAIL (resource version unchanged)\n' "${SECRET_NAMES[$index]}" >&2
    exit 1
  fi
  changed=$((changed + 1))
  printf 'secret_rotation=%s:PASS\n' "${SECRET_NAMES[$index]}"
done

wait_rollout "$LAS_API_DEPLOYMENT"
wait_rollout "$LAS_QUEUE_DEPLOYMENT"
api_revision="$(deployment_revision "$LAS_API_DEPLOYMENT")"
queue_revision="$(deployment_revision "$LAS_QUEUE_DEPLOYMENT")"
if [[ "$api_revision" == "$initial_api_revision" || "$queue_revision" == "$initial_queue_revision" ]]; then
  printf 'LAS API and queue must each roll to a new Deployment revision after credential rotation.\n' >&2
  exit 1
fi
printf 'pod_template_rotation=api:${initial_api_revision}->${api_revision}:PASS\n'
printf 'pod_template_rotation=queue:${initial_queue_revision}->${queue_revision}:PASS\n'
printf 'phase=run_and_persisted_state\n'
"${SCRIPT_DIR}/las-baseline-smoke.sh"
printf 'credential_rotation_acceptance=PASS\n'
