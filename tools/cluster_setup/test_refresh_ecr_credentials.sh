#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/refresh-ecr-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT
mkdir -p "$WORK_DIR/bin"

cat > "$WORK_DIR/bin/aws" <<'SH'
#!/usr/bin/env bash
[[ "$*" == "ecr get-login-password --region us-east-2" ]] || exit 2
printf 'fixture-token\n'
SH

cat > "$WORK_DIR/bin/kubectl-stub" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
args="$*"
if [[ "$1" == "annotate" ]]; then exit 0; fi
if [[ "$1" == "apply" ]]; then
  cat >/dev/null
  [[ "${KUBECTL_STUB_FAIL_APPLY:-false}" == "true" ]] && exit 1
  exit 0
fi
if [[ "$1" == "-n" && "$3" == "get" ]]; then
  case "$4" in
    pods) printf 'NAME READY STATUS\nlas-api-0 1/1 Running\n'; exit 0 ;;
    deployments) printf 'las-api\nlas-queue\n'; exit 0 ;;
    deployment) printf '658391232643.dkr.ecr.us-east-2.amazonaws.com/las:test\n'; exit 0 ;;
  esac
fi
if [[ "$1" == "-n" && "$3" == "rollout" ]]; then
  if [[ "$4" == "restart" ]]; then exit 0; fi
  if [[ "$4" == "status" ]]; then
    [[ "${KUBECTL_STUB_FAIL_ROLLOUT:-false}" == "true" ]] && exit 1
    exit 0
  fi
fi
printf 'Unexpected kubectl arguments: %s\n' "$args" >&2
exit 2
SH
chmod +x "$WORK_DIR/bin/aws" "$WORK_DIR/bin/kubectl-stub"

run_refresh() {
  PATH="$WORK_DIR/bin:$PATH" \
    AWS_DEFAULT_REGION=us-east-2 \
    ECR_REGION=us-east-2 \
    TARGET_NAMESPACES="ai-platform" \
    KUBECTL="$WORK_DIR/bin/kubectl-stub" \
    "$SCRIPT_DIR/refresh_ecr_credentials.sh" >"$WORK_DIR/output.log" 2>&1
}

if ! run_refresh; then
  cat "$WORK_DIR/output.log" >&2
  printf 'Expected successful ECR refresh and rollouts.\n' >&2
  exit 1
fi
grep -q 'Ready: las-api in ai-platform' "$WORK_DIR/output.log"
grep -q 'Ready: las-queue in ai-platform' "$WORK_DIR/output.log"
if grep -q 'fixture-token' "$WORK_DIR/output.log"; then
  printf 'Registry token leaked to output.\n' >&2
  exit 1
fi

if KUBECTL_STUB_FAIL_ROLLOUT=true run_refresh; then
  printf 'Expected a failed rollout to fail the ECR refresh command.\n' >&2
  exit 1
fi
grep -q 'Rollout failed' "$WORK_DIR/output.log"

if KUBECTL_STUB_FAIL_APPLY=true run_refresh; then
  printf 'Expected a failed Secret apply to fail the ECR refresh command.\n' >&2
  exit 1
fi
grep -q 'no Pods were restarted' "$WORK_DIR/output.log"
if grep -Eq 'Cleaning up ImagePullBackOff pods|Restarted:' "$WORK_DIR/output.log"; then
  printf 'Pods were touched after the ECR Secret refresh failed.\n' >&2
  exit 1
fi
printf 'ECR credential refresh rollout checks passed.\n'
