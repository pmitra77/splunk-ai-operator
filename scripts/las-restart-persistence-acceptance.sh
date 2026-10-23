#!/usr/bin/env bash

# Disposable-cluster-only verification of persisted LAS state across restarts.
set -Eeuo pipefail

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/las-restart-persistence-acceptance.sh

Required environment:
  LAS_DISPOSABLE_CLUSTER_CONFIRMED=true
  LAS_BASE_URL                    LAS API base URL (normally a port-forward).
  LAS_NAMESPACE                   Namespace containing the LAS release.
  LAS_API_DEPLOYMENT              Exact API Deployment name.
  LAS_QUEUE_DEPLOYMENT            Exact queue Deployment name.

This script creates one fixture thread and queued run, then performs two
Kubernetes rollout restarts (API first, queue second). It checks that the
thread's graph state values remain readable after each restart. Use only with
a disposable cluster and the deterministic contract fixture. It does not
install or upgrade the operator or chart.
EOF
  exit 0
fi

required=(LAS_DISPOSABLE_CLUSTER_CONFIRMED LAS_BASE_URL LAS_NAMESPACE LAS_API_DEPLOYMENT LAS_QUEUE_DEPLOYMENT)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    printf 'Missing required environment variable: %s\n' "$name" >&2
    exit 2
  fi
done
if [[ "$LAS_DISPOSABLE_CLUSTER_CONFIRMED" != "true" ]]; then
  printf 'Set LAS_DISPOSABLE_CLUSTER_CONFIRMED=true only for a disposable cluster.\n' >&2
  exit 2
fi
for resource_name in "$LAS_NAMESPACE" "$LAS_API_DEPLOYMENT" "$LAS_QUEUE_DEPLOYMENT"; do
  if [[ ! "$resource_name" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]]; then
    printf 'Namespace and Deployment names must be simple Kubernetes names.\n' >&2
    exit 2
  fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUALIFIER="${SCRIPT_DIR}/las-runtime-acceptance.py"
KUBECTL="${KUBECTL:-kubectl}"
command -v "$KUBECTL" >/dev/null 2>&1 || { printf 'kubectl is required.\n' >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { printf 'python3 is required.\n' >&2; exit 2; }
[[ -r "$QUALIFIER" ]] || { printf 'LAS runtime acceptance script is missing.\n' >&2; exit 2; }

marker="aip5409-restart-$(python3 -c 'import uuid; print(uuid.uuid4().hex)')"
result="$(python3 - "$marker" <<'PY' | python3 "$QUALIFIER" protocol --request-timeout 45 --run-timeout 300
import json
import sys
marker = sys.argv[1]
print(json.dumps({
    "graph_id": "contract_fixture",
    "cases": [{
        "name": "restart-persistence",
        "input": {"request": marker},
        "expected_state_values": [marker],
        "run_modes": ["sync", "queued"],
    }],
}, separators=(",", ":")))
PY
)" || { printf 'Initial LAS protocol qualification failed.\n' >&2; exit 1; }

mapfile -t thread_ids < <(python3 -c 'import json,sys; [print(x["thread_id"]) for x in json.load(sys.stdin)["checks"]]' <<<"$result")
if (( ${#thread_ids[@]} != 2 )); then
  printf 'Protocol qualification did not return both thread IDs.\n' >&2
  exit 1
fi

check_states() {
  local stage="$1" thread_id
  for thread_id in "${thread_ids[@]}"; do
    LAS_BASE_URL="$LAS_BASE_URL" python3 "$QUALIFIER" state --thread-id "$thread_id" --expected-value "$marker" >/dev/null
  done
  printf 'persisted_state_after_%s=PASS\n' "$stage"
}

restart_and_wait() {
  local deployment="$1"
  "$KUBECTL" -n "$LAS_NAMESPACE" rollout restart "deployment/${deployment}" >/dev/null
  "$KUBECTL" -n "$LAS_NAMESPACE" rollout status "deployment/${deployment}" --timeout="${ROLLOUT_TIMEOUT:-300s}" >/dev/null
}

restart_and_wait "$LAS_API_DEPLOYMENT"
check_states api_restart
active_result="$(LAS_BASE_URL="$LAS_BASE_URL" python3 "$QUALIFIER" submit-active --cpu-seconds 15 --running-timeout 45)" || {
  printf 'Could not place an active fixture run before queue replacement.\n' >&2
  exit 1
}
read -r active_thread active_run active_marker < <(python3 -c 'import json,sys; x=json.load(sys.stdin); print(x["thread_id"], x["run_id"], x["marker"])' <<<"$active_result")
restart_and_wait "$LAS_QUEUE_DEPLOYMENT"
LAS_BASE_URL="$LAS_BASE_URL" python3 "$QUALIFIER" verify-run \
  --thread-id "$active_thread" --run-id "$active_run" --expected-marker "$active_marker" \
  --run-timeout "${ACTIVE_RUN_TIMEOUT:-300}" >/dev/null
printf 'active_run_after_queue_restart=PASS\n'
check_states queue_restart
printf 'las_restart_persistence=PASS\n'
