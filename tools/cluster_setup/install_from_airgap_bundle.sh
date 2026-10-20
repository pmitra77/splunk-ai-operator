#!/usr/bin/env bash
# install_from_airgap_bundle.sh
# Run on the air-gapped install machine (needs kubectl/helm/ssh pre-installed,
# but NO outbound internet). Extracts the bundle produced by
# prepare_airgap_bundle.sh, sets environment-variable overrides, then invokes
# k0s_cluster_with_stack.sh.
#
# Usage:
#   ./install_from_airgap_bundle.sh --bundle airgap-bundle-<date>.tar.gz \
#       --config my-cluster-config.yaml [--extract-dir /opt/airgap]
#
# The installer (k0s_cluster_with_stack.sh) must be in the same directory as
# this script, or pass --installer /path/to/k0s_cluster_with_stack.sh.

set -euo pipefail

BUNDLE_TARBALL=""
CONFIG_FILE="${CONFIG_FILE:-}"
EXTRACT_DIR="${EXTRACT_DIR:-/opt/airgap}"
INSTALLER_SCRIPT=""
SUBCOMMAND="${SUBCOMMAND:-install}"

# ── Argument parsing ─────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --bundle)      BUNDLE_TARBALL="$2"; shift 2 ;;
    --config)      CONFIG_FILE="$2"; shift 2 ;;
    --extract-dir) EXTRACT_DIR="$2"; shift 2 ;;
    --installer)   INSTALLER_SCRIPT="$2"; shift 2 ;;
    --subcommand)  SUBCOMMAND="$2"; shift 2 ;;
    -h|--help)
      cat <<'HELP'
install_from_airgap_bundle.sh — extract an air-gap bundle and run the
Splunk AI Platform installer with no outbound internet required.

USAGE
  ./install_from_airgap_bundle.sh --bundle BUNDLE.tar.gz [OPTIONS]

REQUIRED
  --bundle FILE         Path to the tar.gz produced by prepare_airgap_bundle.sh.

OPTIONS
  --config FILE         Path to your k0s-cluster-config.yaml.
                        If omitted, the installer looks for CONFIG_FILE in the
                        environment or its default location. Required for a
                        CPU-only bundle.

  --extract-dir DIR     Directory where the bundle is extracted.
                        Default: /opt/airgap
                        Env: EXTRACT_DIR

  --installer SCRIPT    Path to k0s_cluster_with_stack.sh.
                        Default: same directory as this script. A CPU-only
                        bundle runs its included version-matched installer.

  --subcommand CMD      Installer subcommand to run: install | join-workers
                        Default: install
                        Env: SUBCOMMAND

  -h, --help            Show this help text.

WHAT THIS SCRIPT DOES
  1. Extracts the bundle into --extract-dir.
  2. Verifies SHA-256 checksums of all bundled files.
  3. Installs the bundled k0s binary to /usr/local/bin/k0s (if not already present).
  4. Installs the bundled yq binary to /usr/local/bin/yq (if not already present).
  5. Registers a local Helm chart repository pointing at the bundled .tgz files.
  6. Exports the following environment variables so k0s_cluster_with_stack.sh
     uses local files instead of downloading from the internet:

     K0S_INSTALL_URL                   → file://<bundle>/binaries/k0s
     YQ_DOWNLOAD_URL                   → file://<bundle>/binaries/yq
     CERT_MANAGER_MANIFEST_URL         → file://<bundle>/manifests/cert-manager.yaml
     LOCAL_PATH_MANIFEST_URL           → file://<bundle>/manifests/local-path-storage.yaml
     NVIDIA_DEVICE_PLUGIN_MANIFEST_URL → file://<bundle>/manifests/nvidia-device-plugin.yml
     PROMETHEUS_CHART_PATH             → <bundle>/charts/kube-prometheus-stack-*.tgz
     OTEL_CHART_PATH                   → <bundle>/charts/opentelemetry-operator-*.tgz
     KUBERAY_CHART_PATH                → <bundle>/charts/kuberay-operator-*.tgz
     METALLB_CHART_PATH                → <bundle>/charts/metallb-*.tgz
     AIRGAP_PYYAML_WHEEL_PATH          → <bundle>/packages/PyYAML-*.whl (or .tar.gz)
     AIRGAP_MODE                       → true

  7. Invokes k0s_cluster_with_stack.sh <subcommand>.

  CPU-only bundles include the installer and matching operator manifests. The
  wrapper verifies their contents and the OCI image archive references before
  installation, then runs from the bundled source directory.

NOTE ON GPU NODE PACKAGES
  The bundle contains OS package files under packages/:
    - epel-release-latest-<N>.noarch.rpm
    - cuda-<os>.repo
    - nvidia-container-toolkit.repo
    - PyYAML-*.whl (or .tar.gz)

  install_from_airgap_bundle.sh does NOT automatically SCP these to GPU nodes
  because the GPU node IPs and SSH credentials are defined in the cluster config,
  which is loaded by k0s_cluster_with_stack.sh itself. Instead:

  Option A (recommended): pre-install NVIDIA drivers + nvidia-container-toolkit
    on GPU nodes before running this script.  The installer detects and skips
    driver installation if nvidia-smi is already present.

  Option B: use the packages/ files to set up a local RPM mirror, then set
    EPEL_RPM_URL_OVERRIDE / CUDA_REPO_URL_OVERRIDE / NVIDIA_CTK_REPO_URL_OVERRIDE
    to point at that mirror before running this script.

  See AIRGAP.md → 'GPU Node OS Packages' for full step-by-step instructions.

PREREQUISITES ON THIS MACHINE (no internet needed)
  kubectl, helm, tar, ssh — must be pre-installed.
  The installer will use the bundled k0s and yq binaries.

EXAMPLES
  # Minimal — config in current directory
  ./install_from_airgap_bundle.sh --bundle airgap-bundle-20260612-103000.tar.gz

  # Explicit config and extract path
  ./install_from_airgap_bundle.sh \
    --bundle /mnt/transfer/airgap-bundle-20260612-103000.tar.gz \
    --config /etc/splunk-ai/my-cluster-config.yaml \
    --extract-dir /opt/airgap

  # Re-join workers after a failed or partial install
  ./install_from_airgap_bundle.sh \
    --bundle airgap-bundle-20260612-103000.tar.gz \
    --config my-cluster-config.yaml \
    --subcommand join-workers

MANUAL USE (advanced)
  If you extracted the bundle yourself and want to run the installer directly,
  source the env-var file from the bundle then call the installer:

    export AIRGAP_BUNDLE_DIR=/opt/airgap/airgap-bundle-20260612-103000
    source "${AIRGAP_BUNDLE_DIR}/airgap-env.sh"
    CONFIG_FILE=./my-config.yaml ./k0s_cluster_with_stack.sh install

HELP
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2
       echo "Run with --help for usage." >&2
       exit 1 ;;
  esac
done

# ── Helpers ──────────────────────────────────────────────────────────────────
log()  { echo "[$(date '+%Y-%m-%d %H:%M:%S')] INFO  $*"; }
warn() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] WARN  $*" >&2; }
err()  { echo "[$(date '+%Y-%m-%d %H:%M:%S')] ERROR $*" >&2; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || err "Required tool not found: $1 — install it on this machine before running."
}

# ── Pre-flight ────────────────────────────────────────────────────────────────
[[ -n "${BUNDLE_TARBALL}" ]] || err "No bundle specified. Use --bundle airgap-bundle-<date>.tar.gz"
[[ -f "${BUNDLE_TARBALL}" ]] || err "Bundle file not found: ${BUNDLE_TARBALL}"

require_cmd tar
require_cmd helm
require_cmd kubectl

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log "=== Splunk AI Platform — Air-Gap Installation ==="
log "Bundle          : ${BUNDLE_TARBALL}"
log "Extract dir     : ${EXTRACT_DIR}"
log "Installer       : ${INSTALLER_SCRIPT}"
log "Subcommand      : ${SUBCOMMAND}"
[[ -n "${CONFIG_FILE}" ]] && log "Config          : ${CONFIG_FILE}"
log ""

# ── Extract bundle ────────────────────────────────────────────────────────────
log "Extracting bundle..."
mkdir -p "${EXTRACT_DIR}"
EXTRACT_DIR="$(cd "${EXTRACT_DIR}" && pwd)"
BUNDLE_NAME="$(tar -tzf "${BUNDLE_TARBALL}" | sed -n '1{s@/.*@@;p;}')"
[[ "${BUNDLE_NAME}" == airgap-bundle-* ]] ||
  err "Bundle archive does not start with an airgap-bundle directory"
tar -xzf "${BUNDLE_TARBALL}" -C "${EXTRACT_DIR}"

# Use the directory named by this archive; another bundle may already be here.
BUNDLE_DIR="${EXTRACT_DIR}/${BUNDLE_NAME}"
[[ -d "${BUNDLE_DIR}" ]] || err "Could not find extracted bundle directory in ${EXTRACT_DIR}"
log "Bundle extracted to: ${BUNDLE_DIR}"

# ── Verify checksums ──────────────────────────────────────────────────────────
if command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1; then
  log "Verifying checksums..."
  (
    cd "${BUNDLE_DIR}"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum --check checksums.sha256 --quiet
    else
      shasum -a 256 --check checksums.sha256 --quiet
    fi
  ) || err "Checksum verification failed — bundle may be corrupt or tampered with."
  log "Checksums verified OK"
else
  err "sha256sum or shasum is required to verify the offline bundle"
fi

# ── Read version manifest ─────────────────────────────────────────────────────
VERSION_FILE="${BUNDLE_DIR}/bundle-versions.txt"
[[ -f "${VERSION_FILE}" ]] || err "bundle-versions.txt missing from bundle — bundle is incomplete."

get_version() {
  grep "^${1}=" "${VERSION_FILE}" | cut -d= -f2
}

K0S_VERSION="$(get_version k0s_version)"
KUBERAY_CHART_VERSION="$(get_version kuberay_chart_version)"
METALLB_CHART_VERSION="$(get_version metallb_chart_version)"
PROMETHEUS_CHART_VERSION="$(get_version prometheus_chart_version)"
OTEL_CHART_VERSION="$(get_version otel_chart_version)"

log "Bundle component versions:"
log "  k0s         : ${K0S_VERSION}"
log "  kuberay     : ${KUBERAY_CHART_VERSION}"
log "  metallb     : ${METALLB_CHART_VERSION}"
log "  prometheus  : ${PROMETHEUS_CHART_VERSION}"
log "  otel        : ${OTEL_CHART_VERSION}"

# Check the selected workload closure before modifying the installer host or
# reaching any cluster node. A bundle prepared for another config must fail
# here, rather than leaving a partially installed private cluster.
CPU_ONLY_BUNDLE=false
if [[ -f "${BUNDLE_DIR}/bundle-profile.txt" ]]; then
  [[ "$(cat "${BUNDLE_DIR}/bundle-profile.txt")" == "cpuOnly" ]] ||
    err "Unknown bundle profile"
  CPU_ONLY_BUNDLE=true
  [[ -n "${CONFIG_FILE}" ]] ||
    err "CPU-only bundle requires an explicit --config or CONFIG_FILE"
fi
if [[ -n "${CONFIG_FILE}" ]]; then
  [[ -f "${CONFIG_FILE}" ]] || err "Config file not found: ${CONFIG_FILE}"
  BUNDLE_YQ="${BUNDLE_DIR}/binaries/yq"
  [[ -x "${BUNDLE_YQ}" ]] || err "Bundled yq is missing or not executable"
  if [[ "$("${BUNDLE_YQ}" eval -r '.aiPlatform.profile // ""' "${CONFIG_FILE}")" == "cpuOnly" ]]; then
    [[ "${CPU_ONLY_BUNDLE}" == "true" ]] ||
      err "CPU-only config requires a CPU-only bundle made with --config"
    [[ "$("${BUNDLE_YQ}" eval -r '.cluster.airgap // false' "${CONFIG_FILE}")" == "true" ]] || \
      err "cpuOnly offline bundle requires cluster.airgap: true"
    for required in k0s-images.tar k0s-images.list addon-images.tar addon-images.list workload-images.tar workload-images.list; do
      [[ -s "${BUNDLE_DIR}/images/${required}" ]] || err "Required CPU-only bundle artifact missing: images/${required}"
    done
    for expression in \
      '.images.operator.image' \
      '.images.splunk.image' \
      '.images.splunk.operatorImage' \
      '.images.slim.apiImage'; do
      expected="$("${BUNDLE_YQ}" eval -r "${expression} // \"\"" "${CONFIG_FILE}")"
      [[ -n "${expected}" && "${expected}" != "null" ]] || err "Missing ${expression} in CPU-only config"
      grep -Fxq -- "${expected}" "${BUNDLE_DIR}/images/workload-images.list" || \
        err "CPU-only bundle does not contain configured image ${expression}: ${expected}"
    done
    las_repository="$("${BUNDLE_YQ}" eval -r '.aiPlatform.features[] | select(.name == "agentruntime") | .las.image.repository' "${CONFIG_FILE}")"
    las_tag="$("${BUNDLE_YQ}" eval -r '.aiPlatform.features[] | select(.name == "agentruntime") | .las.image.tag' "${CONFIG_FILE}")"
    [[ -n "${las_repository}" && -n "${las_tag}" && "${las_repository}" != "null" && "${las_tag}" != "null" ]] || \
      err "Missing Agent Runtime image repository/tag in CPU-only config"
    grep -Fxq -- "${las_repository}:${las_tag}" "${BUNDLE_DIR}/images/workload-images.list" || \
      err "CPU-only bundle does not contain configured Agent Runtime image"
    log "CPU-only workload image closure verified against ${CONFIG_FILE}"
  elif [[ "${CPU_ONLY_BUNDLE}" == "true" ]]; then
    err "CPU-only bundle requires aiPlatform.profile: cpuOnly"
  fi
fi

if [[ "${CPU_ONLY_BUNDLE}" == "true" ]]; then
  require_cmd cmp
  for required in binaries/k0s binaries/yq manifests/cert-manager.yaml \
                  manifests/local-path-storage.yaml packages/pyyaml.filename; do
    [[ -s "${BUNDLE_DIR}/${required}" ]] ||
      err "Required CPU-only bundle artifact missing: ${required}"
  done
  bundled_pyyaml="$(cat "${BUNDLE_DIR}/packages/pyyaml.filename")"
  [[ "${bundled_pyyaml}" == "$(basename "${bundled_pyyaml}")" &&
     -s "${BUNDLE_DIR}/packages/${bundled_pyyaml}" ]] ||
    err "Bundled PyYAML package is missing or has an invalid filename"
  BUNDLED_SETUP="${BUNDLE_DIR}/cluster_setup"
  for required in k0s_cluster_with_stack.sh install_from_airgap_bundle.sh \
                  artifacts.yaml splunk-operator-cluster.yaml las_deployment.py; do
    [[ -s "${BUNDLED_SETUP}/${required}" ]] ||
      err "Version-matched installer artifact missing: cluster_setup/${required}"
  done
  cmp -s "${SCRIPT_DIR}/install_from_airgap_bundle.sh" \
         "${BUNDLED_SETUP}/install_from_airgap_bundle.sh" ||
    err "Bundle was built from a different installer revision; run its bundled installer"
  if [[ -n "${INSTALLER_SCRIPT}" ]]; then
    cmp -s "${INSTALLER_SCRIPT}" "${BUNDLED_SETUP}/k0s_cluster_with_stack.sh" ||
      err "--installer differs from the version-matched installer in the bundle"
  fi
  INSTALLER_SCRIPT="${BUNDLED_SETUP}/k0s_cluster_with_stack.sh"

  # The config's relative files are resolved from the bundled script directory
  # when we invoke it below. Absolute files are permitted only if identical.
  for entry in 'aiPlatform:artifacts.yaml' 'splunkOperator:splunk-operator-cluster.yaml'; do
    key="${entry%%:*}"
    file="${entry#*:}"
    configured="$("${BUNDLE_YQ}" eval -r ".files.${key} // \"\"" "${CONFIG_FILE}")"
    [[ -n "${configured}" ]] || err "Missing files.${key} in CPU-only config"
    if [[ "${configured}" == /* ]]; then
      candidate="${configured}"
    else
      candidate="${BUNDLED_SETUP}/${configured}"
    fi
    [[ -f "${candidate}" ]] && cmp -s "${candidate}" "${BUNDLED_SETUP}/${file}" ||
      err "files.${key} must match the bundled ${file}"
  done

  # The CPU profile has no Helm add-ons. Its two static manifests must account
  # for exactly the add-on references that were packed into the OCI archive.
  manifest_refs="$(grep -hoE 'image:[[:space:]]*["'"'"']?[^"'"'"' ]+' \
      "${BUNDLE_DIR}/manifests/cert-manager.yaml" \
      "${BUNDLE_DIR}/manifests/local-path-storage.yaml" \
      | sed -E 's/image:[[:space:]]*["'"'"']?//' \
      | grep -E '[:/]' | sort -u)" ||
    err "Could not enumerate CPU-only manifest images"
  [[ -n "${manifest_refs}" && "${manifest_refs}" == "$(cat "${BUNDLE_DIR}/images/addon-images.list")" ]] ||
    err "CPU-only add-on image list does not match its manifests"
  for archive in "${BUNDLE_DIR}"/images/*.tar; do
    case "$(basename "${archive}")" in
      k0s-images.tar|addon-images.tar|workload-images.tar) ;;
      *) err "Unexpected image archive in CPU-only bundle: ${archive}" ;;
    esac
  done

  # k0s emits OCI Image Layout archives. Verify actual named references in
  # all three archives, then compare the workload list with the chosen config.
  for kind in k0s addon workload; do
    archive="${BUNDLE_DIR}/images/${kind}-images.tar"
    list="${BUNDLE_DIR}/images/${kind}-images.list"
    index="$(tar -xOf "${archive}" index.json 2>/dev/null)" ||
      index="$(tar -xOf "${archive}" ./index.json 2>/dev/null)" ||
      err "OCI index.json missing from images/${kind}-images.tar"
    refs="$(printf '%s' "${index}" |
      "${BUNDLE_YQ}" eval -r '.manifests[] | .annotations."org.opencontainers.image.ref.name" // .annotations."io.containerd.image.name" // ""' -)" ||
      err "Cannot read OCI references in images/${kind}-images.tar"
    while IFS= read -r image_ref; do
      [[ -z "${image_ref}" ]] && continue
      canonical="${image_ref}"
      first="${image_ref%%/*}"
      if [[ "${image_ref}" != */* ]]; then
        canonical="docker.io/library/${image_ref}"
      elif [[ "${first}" != *.* && "${first}" != *:* && "${first}" != "localhost" ]]; then
        canonical="docker.io/${image_ref}"
      fi
      grep -Fxq -- "${image_ref}" <<< "${refs}" ||
        grep -Fxq -- "${canonical}" <<< "${refs}" ||
        err "OCI archive images/${kind}-images.tar is missing image ${image_ref}"
    done < "${list}"
  done
  log "CPU-only OCI archive and version-matched installer closure verified"
else
  [[ -n "${INSTALLER_SCRIPT}" ]] ||
    INSTALLER_SCRIPT="${SCRIPT_DIR}/k0s_cluster_with_stack.sh"
fi
[[ -f "${INSTALLER_SCRIPT}" ]] ||
  err "Installer not found: ${INSTALLER_SCRIPT}"
[[ -x "${INSTALLER_SCRIPT}" ]] || chmod +x "${INSTALLER_SCRIPT}"

# ── Install bundled k0s binary into PATH for node provisioning ────────────────
log "Installing bundled k0s binary to /usr/local/bin/k0s ..."
if [[ -f "${BUNDLE_DIR}/binaries/k0s" ]]; then
  if [[ "$(id -u)" -eq 0 ]]; then
    cp "${BUNDLE_DIR}/binaries/k0s" /usr/local/bin/k0s
    chmod +x /usr/local/bin/k0s
  else
    sudo cp "${BUNDLE_DIR}/binaries/k0s" /usr/local/bin/k0s
    sudo chmod +x /usr/local/bin/k0s
  fi
  log "k0s ${K0S_VERSION} installed to /usr/local/bin/k0s"
else
  warn "k0s binary not found in bundle — ensure k0s is pre-installed on all nodes."
fi

# Install yq if not present
if ! command -v yq >/dev/null 2>&1; then
  log "Installing bundled yq binary..."
  if [[ "$(id -u)" -eq 0 ]]; then
    cp "${BUNDLE_DIR}/binaries/yq" /usr/local/bin/yq
    chmod +x /usr/local/bin/yq
  else
    sudo cp "${BUNDLE_DIR}/binaries/yq" /usr/local/bin/yq
    sudo chmod +x /usr/local/bin/yq
  fi
  log "yq installed to /usr/local/bin/yq"
fi

# ── Set up local Helm chart repository (best-effort convenience only) ─────────
# NOTE: The main installer (k0s_cluster_with_stack.sh) installs every chart by
# ABSOLUTE PATH via the *_CHART_PATH env vars exported below — it never references
# this "airgap-charts" repo. Registration is therefore purely optional convenience.
#
# Modern Helm (v3.7+) dropped support for "file://" as a `helm repo add` protocol
# ("Error: could not find protocol handler for: file"). Since the repo is unused,
# we make the whole block strictly non-fatal so it cannot abort the install under
# `set -euo pipefail` on a host with a newer Helm.
log "Registering local Helm chart repository (optional; charts install by path)..."
LOCAL_CHARTS_DIR="${BUNDLE_DIR}/charts"
helm repo index "${LOCAL_CHARTS_DIR}" --url "file://${LOCAL_CHARTS_DIR}" >/dev/null 2>&1 || true
if helm repo add airgap-charts "file://${LOCAL_CHARTS_DIR}" >/dev/null 2>&1; then
  helm repo update airgap-charts >/dev/null 2>&1 || true
else
  log "  Helm rejected file:// repo (expected on Helm 3.7+); charts will install by path — no action needed."
fi

# ── Export env-var overrides read by k0s_cluster_with_stack.sh ───────────────
# The main installer respects these vars via ${VAR:-default} patterns.
export AIRGAP_BUNDLE_DIR="${BUNDLE_DIR}"

# Binaries
export K0S_INSTALL_URL="file://${BUNDLE_DIR}/binaries/k0s"
export YQ_DOWNLOAD_URL="file://${BUNDLE_DIR}/binaries/yq"

# Pre-loaded container-image bundles (OCI tarballs). The installer scp's EVERY
# *.tar in this directory to each node's /var/lib/k0s/images/, where k0s
# auto-imports all of them into containerd at startup — covering both k0s
# control-plane images (k0s-images.tar: pause/calico/kube-proxy/coredns/…) AND
# add-on component images (addon-images.tar: cert-manager/prometheus/metallb/…).
# This removes the need to pull from quay.io/ghcr.io/etc. over a blocked link.
if compgen -G "${BUNDLE_DIR}/images/*.tar" >/dev/null 2>&1; then
  export AIRGAP_K0S_IMAGE_DIR="${BUNDLE_DIR}/images"
  log "Pre-loaded image bundles found in: ${AIRGAP_K0S_IMAGE_DIR}"
  for _t in "${BUNDLE_DIR}/images"/*.tar; do log "  - $(basename "${_t}") ($(du -h "${_t}" | cut -f1))"; done
else
  err "No pre-loaded image bundles in air-gap bundle (images/*.tar)"
fi

# Static manifests
export CERT_MANAGER_MANIFEST_URL="file://${BUNDLE_DIR}/manifests/cert-manager.yaml"
export LOCAL_PATH_MANIFEST_URL="file://${BUNDLE_DIR}/manifests/local-path-storage.yaml"
if [[ "${CPU_ONLY_BUNDLE}" != "true" ]]; then
  export NVIDIA_DEVICE_PLUGIN_MANIFEST_URL="file://${BUNDLE_DIR}/manifests/nvidia-device-plugin.yml"
fi

# Helm chart paths — installer uses these instead of remote repos.
# helm pull sometimes produces underscores instead of dashes in the filename,
# so resolve via glob; fail fast if the chart is genuinely missing.
_resolve_chart() {
  local pattern="$1" label="$2"
  # shellcheck disable=SC2206
  local matches=( ${pattern} )
  if [[ ${#matches[@]} -eq 0 || ! -f "${matches[0]}" ]]; then
    err "Chart not found for ${label} (expected: ${pattern}). Bundle may be corrupt or version-mismatched."
  fi
  echo "${matches[0]}"
}

if [[ "${CPU_ONLY_BUNDLE}" != "true" ]]; then
  PROM_TGZ="$(_resolve_chart "${LOCAL_CHARTS_DIR}/kube-prometheus-stack-${PROMETHEUS_CHART_VERSION}*.tgz" "kube-prometheus-stack")"
  OTEL_TGZ="$(_resolve_chart "${LOCAL_CHARTS_DIR}/opentelemetry-operator-${OTEL_CHART_VERSION}*.tgz" "opentelemetry-operator")"
  KUBERAY_TGZ="$(_resolve_chart "${LOCAL_CHARTS_DIR}/kuberay-operator-${KUBERAY_CHART_VERSION}*.tgz" "kuberay-operator")"
  METALLB_TGZ="$(_resolve_chart "${LOCAL_CHARTS_DIR}/metallb-${METALLB_CHART_VERSION}*.tgz" "metallb")"

  export PROMETHEUS_CHART_PATH="${PROM_TGZ}"
  export OTEL_CHART_PATH="${OTEL_TGZ}"
  export KUBERAY_CHART_PATH="${KUBERAY_TGZ}"
  export METALLB_CHART_PATH="${METALLB_TGZ}"
fi

# GPU node OS packages — pyyaml wheel path for offline pip3 install on all nodes
PYYAML_FNAME=""
[[ -f "${BUNDLE_DIR}/packages/pyyaml.filename" ]] && \
  PYYAML_FNAME="$(cat "${BUNDLE_DIR}/packages/pyyaml.filename")"
if [[ -n "${PYYAML_FNAME}" && -f "${BUNDLE_DIR}/packages/${PYYAML_FNAME}" ]]; then
  export AIRGAP_PYYAML_WHEEL_PATH="${BUNDLE_DIR}/packages/${PYYAML_FNAME}"
  log "PyYAML wheel found: ${AIRGAP_PYYAML_WHEEL_PATH}"
else
  log "No PyYAML wheel in bundle — nodes will fall back to OS package manager for pyyaml."
fi

# Disable helm repo add/update calls inside the installer — all charts are local
export AIRGAP_MODE="true"

log ""
log "Environment overrides set. Launching installer..."
log ""

# ── Invoke main installer ─────────────────────────────────────────────────────
if [[ -n "${CONFIG_FILE}" ]]; then
  CONFIG_FILE="$(realpath "${CONFIG_FILE}")"
  if [[ "${CPU_ONLY_BUNDLE}" == "true" ]]; then
    cd "${BUNDLED_SETUP}"
  fi
  exec env CONFIG_FILE="${CONFIG_FILE}" "${INSTALLER_SCRIPT}" "${SUBCOMMAND}"
else
  log "No --config supplied. The installer will look for CONFIG_FILE in the environment"
  log "or its default location. Set CONFIG_FILE if it is not in the current directory."
  exec "${INSTALLER_SCRIPT}" "${SUBCOMMAND}"
fi
