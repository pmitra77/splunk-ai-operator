#!/usr/bin/env bash
set -euo pipefail

chart_dir="$(cd "$(dirname "$0")" && pwd)"
output_dir="${1:-/tmp}"
staging_dir="$(mktemp -d)"
trap 'rm -rf "$staging_dir"' EXIT

tar -xzf "$chart_dir/langgraph-cloud-0.3.4.tgz" -C "$staging_dir"
git -C "$staging_dir" apply -p1 "$chart_dir/rollout-policy.patch"

# Helm preserves source mtimes in the archive. Normalize them for a stable digest.
python3 - "$staging_dir/langgraph-cloud" <<'PY'
from datetime import datetime, timezone
from pathlib import Path
import os
import sys

root = Path(sys.argv[1])
stamp = datetime(2026, 10, 12, tzinfo=timezone.utc).timestamp()
for path in sorted(root.rglob('*'), key=lambda p: len(p.parts), reverse=True):
    if not path.is_symlink():
        os.utime(path, (stamp, stamp))
os.utime(root, (stamp, stamp))
PY

helm lint "$staging_dir/langgraph-cloud"
helm package "$staging_dir/langgraph-cloud" --destination "$output_dir"
shasum -a 256 "$output_dir/langgraph-cloud-0.3.5-splunk.2.tgz"
