#!/usr/bin/env bash
# refresh_ecr_credentials.sh - Refresh ECR image pull secrets using an ECR token
#
# Usage (run on controller node with AWS ECR authorization):
#   ./refresh_ecr_credentials.sh
set -euo pipefail

ECR_ACCOUNT="${ECR_ACCOUNT:-658391232643}"
ECR_REGION="${ECR_REGION:-us-east-2}"
ECR_SERVER="${ECR_ACCOUNT}.dkr.ecr.${ECR_REGION}.amazonaws.com"
NAMESPACES="${TARGET_NAMESPACES:-ai-platform splunk-ai-operator-system}"
KUBECTL="${KUBECTL:-k0s kubectl}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export KUBECTL

info()  { echo "[INFO]  $*"; }
error() { echo "[ERROR] $*" >&2; }

# --- Step 1: Refresh the managed registry Secret in place. New/replacement
# Pods read this Secret at pull time, so keep it present while the token rotates.
for ns in ${NAMESPACES}; do
  info "Refreshing ecr-registry-secret in ${ns}..."
  $KUBECTL annotate secret ecr-registry-secret -n "${ns}" kubectl.kubernetes.io/last-applied-configuration- >/dev/null 2>&1 || true
  if aws ecr get-login-password --region "${ECR_REGION}" 2>/dev/null \
    | python3 "${SCRIPT_DIR}/las_deployment.py" registry-secret --namespace "${ns}" \
      --name ecr-registry-secret --server "${ECR_SERVER}" \
    | $KUBECTL apply --server-side --force-conflicts --field-manager=splunk-ai-operator-deployment -f -; then
    info "  ✓ ecr-registry-secret refreshed in ${ns}"
  else
    error "  Failed to refresh ecr-registry-secret in ${ns}"
  fi
done

# --- Step 2: Delete pods stuck in ImagePullBackOff ---
info "Cleaning up ImagePullBackOff pods..."
for ns in ${NAMESPACES}; do
  backoff_pods=$($KUBECTL -n "${ns}" get pods 2>/dev/null \
    | grep -i "ImagePullBackOff\|ErrImagePull" \
    | awk '{print $1}' || true)

  if [[ -n "$backoff_pods" ]]; then
    while IFS= read -r pod; do
      [[ -z "$pod" ]] && continue
      $KUBECTL -n "${ns}" delete pod "${pod}" --grace-period=0 --force 2>/dev/null && \
        info "  Deleted: ${pod}" || true
    done <<< "$backoff_pods"
  else
    info "  No stuck pods in ${ns}"
  fi
done

# --- Step 3: Restart deployments that use ECR images ---
info "Restarting ECR-based deployments..."
for ns in ${NAMESPACES}; do
  for dep in $($KUBECTL -n "${ns}" get deployments -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    [[ -z "$dep" ]] && continue
    images=$($KUBECTL -n "${ns}" get deployment "${dep}" -o jsonpath='{.spec.template.spec.containers[*].image}' 2>/dev/null || "")
    if echo "$images" | grep -q "${ECR_ACCOUNT}" 2>/dev/null; then
      $KUBECTL -n "${ns}" rollout restart deployment "${dep}" 2>/dev/null && \
        info "  Restarted: ${dep}" || true
    fi
  done
done

echo ""
info "=========================================="
info "ECR credentials refreshed!"
info "  Server: ${ECR_SERVER}"
info "  Namespaces: ${NAMESPACES}"
info ""
info "  Token expires in ~12 hours. Re-run this script to refresh."
info "=========================================="
