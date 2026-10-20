#!/usr/bin/env bash
# Test-only dependencies for the private CPU-only k0s cluster. This is separate
# from the customer-facing installer. No install mode command downloads code.
set -euo pipefail
set +x
umask 077
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MC_CONFIG_DIR=""
SCOPED_TMP=""

usage() {
  cat >&2 <<'EOF'
Usage:
  install_airgap_test_dependencies.sh prepare --config FILE --artifact-dir DIR
  install_airgap_test_dependencies.sh install --config FILE --artifact-dir DIR
  install_airgap_test_dependencies.sh secrets --config FILE --kubeconfig FILE --license-file FILE

Nonsecret YAML config:
  host:
    bindIp: 10.0.1.20
    dataRoot: /var/lib/las-deps
    storageMode: encrypted-root-ebs # or mounted-volume
    rootEbsEncryptedConfirmed: true # required for encrypted-root-ebs
    outputFile: /root/las-deps-connection.json
  images:
    minio: sha256:<64 hex image ID from a local source build>
    postgres: docker.io/library/postgres@sha256:<64 hex characters>
    redis: docker.io/library/redis@sha256:<64 hex characters>
  minio:
    distribution: community-source # default; aistor is optional
    sourceCommit: <40 hex characters of minio/minio source build>
    localArchive: /path/to/source-built-minio.oci.tar
    localArchiveSha256: <64 hex characters>
    # licenseFile: /root/secrets/minio.license # required for aistor
  client:
    mcBinary: /path/to/prestaged/mc-linux-amd64
    mcSha256: <64 hex characters>
  # Optional credentials paths. Omit to generate root-owned 0600 files under
  # dataRoot/config/credentials during install. Existing files are reused.
  # credentials:
  #   minioUserFile: /root/secrets/minio-root-user
  #   minioPasswordFile: /root/secrets/minio-root-password
  #   postgresPasswordFile: /root/secrets/postgres-password
  #   redisPasswordFile: /root/secrets/redis-password
  database:
    user: las
    name: las
  objectStore:
    bucket: ai-platform
  kubernetes:
    namespace: ai-platform
    licenseSecretName: las-auth
    postgresSecretName: las-postgres
    redisSecretName: las-redis

prepare runs on a connected Linux x86_64 host with Podman. It imports a local
commit-pinned community MinIO OCI archive when configured, pulls other exact
digest references, and records archive SHA-256 and image IDs. install runs as
root on a prepared private Linux x86_64 host with Podman, jq, yq, Python 3,
and either an encrypted root EBS volume or a mounted persistent data volume.
It imports only the supplied archives. Secret file contents never enter command arguments,
unit files, Podman inspect environment values, or stdout. The protected
outputFile contains application credentials, a protected app-key file path,
and URLs: protect and transfer it as a secret. The default MinIO distribution
is a source-built community image. AIStor requires a staged license;
community-source requires an independently built, commit-pinned image. The
source-built image must have ENTRYPOINT ["/minio"] and a static Linux AMD64
binary; install supplies `server /data` arguments and a writable /tmp.
sourceCommit is recorded as operator-supplied provenance, not independently
verified against image bytes. secrets runs on a trusted Linux host with
kubectl access after k0s is ready and applies
the three LAS Secrets using the output and an Agent Server license file.
EOF
  exit 2
}

log() { printf '[test-deps] %s\n' "$*" >&2; }
die() { printf '[test-deps] ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "Missing required tool: $1"; }

# Roll back only the units and Podman secrets created by this invocation.
# Password files and service data remain available for a safe retry.
cleanup_mc_config() {
  [[ -n "$MC_CONFIG_DIR" ]] || return 0
  [[ "$MC_CONFIG_DIR" =~ ^/run/test-deps-mc\.[A-Za-z0-9]{8}$ &&
     -d "$MC_CONFIG_DIR" && ! -L "$MC_CONFIG_DIR" ]] ||
    die "Refusing to remove an unexpected temporary mc config directory"
  rm -rf -- "$MC_CONFIG_DIR"
  MC_CONFIG_DIR=""
}

cleanup_scoped_tmp() {
  [[ -n "$SCOPED_TMP" ]] || return 0
  [[ "$SCOPED_TMP" =~ ^/run/test-deps-scoped\.[A-Za-z0-9]{8}$ &&
     -f "$SCOPED_TMP" && ! -L "$SCOPED_TMP" ]] ||
    die "Refusing to remove an unexpected temporary access-key file"
  rm -f -- "$SCOPED_TMP"
  SCOPED_TMP=""
}

rollback_partial_install() {
  local rc=$?
  [[ "${INSTALL_PHASE:-}" == applying && "$rc" -ne 0 ]] || return "$rc"
  set +e
  systemctl disable --now test-deps-minio.service test-deps-postgres.service \
    test-deps-redis.service >/dev/null 2>&1
  rm -f /etc/systemd/system/test-deps-{minio,postgres,redis}.service
  systemctl daemon-reload >/dev/null 2>&1
  podman secret rm test-deps-minio-user test-deps-minio-pass \
    test-deps-postgres-pass test-deps-redis-acl test-deps-minio-license \
    >/dev/null 2>&1
  cleanup_scoped_tmp
  cleanup_mc_config
  log "Removed partial test dependency units and Podman secrets; rerun install with the same config"
  return "$rc"
}

MODE="${1:-}"
[[ "$MODE" == prepare || "$MODE" == install || "$MODE" == secrets ]] || usage
shift
CONFIG="" ARTIFACT_DIR="" KUBECONFIG_PATH="" LICENSE_FILE=""
while (($#)); do
  case "$1" in
    --config) (($# >= 2)) || usage; CONFIG="$2"; shift 2 ;;
    --artifact-dir) (($# >= 2)) || usage; ARTIFACT_DIR="$2"; shift 2 ;;
    --kubeconfig) (($# >= 2)) || usage; KUBECONFIG_PATH="$2"; shift 2 ;;
    --license-file) (($# >= 2)) || usage; LICENSE_FILE="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -f "$CONFIG" ]] || die "--config must name an existing YAML file"
need yq
need jq
need sha256sum
need python3

cfg() { yq eval -r "$1" "$CONFIG"; }
required_cfg() {
  local value
  value="$(cfg "$1 // \"\"")"
  [[ -n "$value" && "$value" != null ]] || die "Missing config field $1"
  printf '%s' "$value"
}
sha_file() { sha256sum "$1" | awk '{print $1}'; }
check_sha() {
  local file="$1" expected="$2" actual
  [[ -f "$file" && ! -L "$file" ]] || die "Missing regular artifact file: $file"
  [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || die "Invalid SHA-256 for $file"
  actual="$(sha_file "$file")"
  [[ "$actual" == "$expected" ]] || die "SHA-256 mismatch for $file"
}
require_amd64() {
  [[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] ||
    die "This mode requires Linux x86_64"
}
require_absolute_path() {
  [[ "$1" == /* && "$1" != *$'\n'* && "$1" != *$'\r'* ]] ||
    die "Expected an absolute, single-line path: $2"
}
check_protected_file() {
  local file="$1" label="$2" mode owner bytes
  require_absolute_path "$file" "$label"
  [[ -f "$file" && ! -L "$file" ]] || die "$label must be a regular file"
  owner="$(stat -c %u "$file")"; mode="$(stat -c %a "$file")"
  [[ "$owner" == 0 && ( "$mode" == 400 || "$mode" == 600 ) ]] ||
    die "$label must be root-owned with mode 0400 or 0600"
  bytes="$(wc -c < "$file")"
  [[ "$bytes" -gt 0 ]] || die "$label must be nonempty"
}
check_single_line_secret_file() {
  local file="$1" label="$2" bytes no_newlines
  check_protected_file "$file" "$label"
  bytes="$(wc -c < "$file")"
  no_newlines="$(tr -d '\r\n' < "$file" | wc -c)"
  [[ "$bytes" == "$no_newlines" ]] ||
    die "$label must be nonempty and contain no newline"
}
credential_file() {
  local field="$1" fallback="$2" kind="$3" value
  value="$(cfg ".credentials.$field // \"\"")"
  [[ -n "$value" ]] || value="$DATA_ROOT/config/credentials/$fallback"
  require_absolute_path "$value" "credentials.$field"
  [[ -d "$(dirname "$value")" ]] || die "Parent of credentials.$field does not exist"
  if [[ ! -e "$value" ]]; then
    python3 - "$value" "$kind" <<'PY'
import os, secrets, sys
path, kind = sys.argv[1:]
value = ("lasroot" + secrets.token_hex(8)) if kind == "user" else secrets.token_urlsafe(36)
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, "w") as out:
    out.write(value)
PY
  fi
  check_single_line_secret_file "$value" "credentials.$field"
  printf '%s' "$value"
}
read_common() {
  BIND_IP="$(required_cfg '.host.bindIp')"
  DATA_ROOT="$(required_cfg '.host.dataRoot')"
  OUTPUT_FILE="$(required_cfg '.host.outputFile')"
  BUCKET="$(required_cfg '.objectStore.bucket')"
  DB_USER="$(required_cfg '.database.user')"
  DB_NAME="$(required_cfg '.database.name')"
  NAMESPACE="$(required_cfg '.kubernetes.namespace')"
  LICENSE_SECRET="$(cfg '.kubernetes.licenseSecretName // "las-auth"')"
  POSTGRES_SECRET="$(cfg '.kubernetes.postgresSecretName // "las-postgres"')"
  REDIS_SECRET="$(cfg '.kubernetes.redisSecretName // "las-redis"')"
  require_absolute_path "$DATA_ROOT" host.dataRoot
  require_absolute_path "$OUTPUT_FILE" host.outputFile
  [[ "$DATA_ROOT" =~ ^/[A-Za-z0-9_./-]+$ ]] || die "dataRoot must use simple path characters"
  [[ "$DATA_ROOT" != / && "$DATA_ROOT" != /tmp* ]] || die "dataRoot must be a dedicated persistent mount"
  [[ "$DB_USER" =~ ^[a-z_][a-z0-9_]*$ && "$DB_NAME" =~ ^[a-z_][a-z0-9_]*$ ]] ||
    die "database.user and database.name must be simple lowercase identifiers"
  [[ "$BUCKET" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || die "Invalid objectStore.bucket"
  [[ "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Invalid kubernetes.namespace"
  for secret_name in "$LICENSE_SECRET" "$POSTGRES_SECRET" "$REDIS_SECRET"; do
    [[ "$secret_name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "Invalid Kubernetes Secret name"
  done
  read_minio_distribution
  python3 - "$BIND_IP" <<'PY' || die "host.bindIp must be a private, non-loopback IPv4 address"
import ipaddress, sys
addr = ipaddress.ip_address(sys.argv[1])
sys.exit(0 if addr.version == 4 and addr.is_private and not addr.is_loopback else 1)
PY
}
read_minio_distribution() {
  MINIO_DISTRIBUTION="$(cfg '.minio.distribution // "community-source"')"
  [[ "$MINIO_DISTRIBUTION" == community-source || "$MINIO_DISTRIBUTION" == aistor ]] ||
    die "minio.distribution must be community-source or aistor"
  if [[ "$MINIO_DISTRIBUTION" == community-source ]]; then
    MINIO_SOURCE_COMMIT="$(required_cfg '.minio.sourceCommit')"
    [[ "$MINIO_SOURCE_COMMIT" =~ ^[a-f0-9]{40}$ ]] ||
      die "minio.sourceCommit must be a pinned 40-character source commit"
  else
    MINIO_SOURCE_COMMIT=""
  fi
}

image_ref() {
  local ref local_archive
  ref="$(required_cfg ".images.$1")"
  local_archive="$(cfg '.minio.localArchive // ""')"
  if [[ "$1" == minio && -n "$local_archive" ]]; then
    [[ "$ref" =~ ^sha256:[a-f0-9]{64}$ ]] ||
      die "images.minio must be the exact image ID for minio.localArchive"
  else
    [[ "$ref" =~ ^[A-Za-z0-9._:/-]+@sha256:[a-f0-9]{64}$ ]] ||
      die "images.$1 must be an exact repository@sha256 digest reference"
  fi
  printf '%s' "$ref"
}

prepare() {
  require_amd64
  need podman
  read_minio_distribution
  [[ -n "$ARTIFACT_DIR" ]] || usage
  require_absolute_path "$ARTIFACT_DIR" artifact-dir
  [[ ! -e "$ARTIFACT_DIR/manifest.json" ]] || die "Refusing to overwrite existing artifact manifest"
  mkdir -p "$ARTIFACT_DIR"
  local mc_source mc_sha manifest component ref archive id actual_arch actual_os local_archive local_archive_sha export_tag
  mc_source="$(required_cfg '.client.mcBinary')"
  mc_sha="$(required_cfg '.client.mcSha256')"
  require_absolute_path "$mc_source" client.mcBinary
  check_sha "$mc_source" "$mc_sha"
  cp "$mc_source" "$ARTIFACT_DIR/mc-linux-amd64"
  chmod 0500 "$ARTIFACT_DIR/mc-linux-amd64"
  manifest="$(jq -nc --arg distribution "$MINIO_DISTRIBUTION" \
    --arg commit "$MINIO_SOURCE_COMMIT" '{schema:1,minio:{distribution:$distribution,sourceCommit:$commit},images:{}}')"
  for component in minio postgres redis; do
    ref="$(image_ref "$component")"
    archive="$ARTIFACT_DIR/$component.oci.tar"
    [[ ! -e "$archive" ]] || die "Refusing to overwrite $archive"
    local_archive="$(cfg '.minio.localArchive // ""')"
    if [[ "$component" == minio && -n "$local_archive" ]]; then
      local_archive_sha="$(required_cfg '.minio.localArchiveSha256')"
      require_absolute_path "$local_archive" minio.localArchive
      check_sha "$local_archive" "$local_archive_sha"
      log "Importing pinned local community MinIO OCI archive"
      podman load --input "$local_archive" >/dev/null
      cp "$local_archive" "$archive"
    else
      log "Pulling pinned $component image and exporting an OCI archive"
      podman pull --arch=amd64 "$ref" >/dev/null
      # Podman cannot reload an OCI archive whose ref-name annotation is a
      # repository@digest if it reserializes the manifest while exporting.
      # Keep the verified digest in the manifest and use a local tag only as
      # the transport name inside the archive.
      export_tag="localhost/las-test-deps-$component:prepared"
      podman tag "$ref" "$export_tag"
      podman save --format oci-archive -o "$archive" "$export_tag" >/dev/null
    fi
    id="$(podman image inspect --format '{{.Id}}' "$ref")"
    actual_arch="$(podman image inspect --format '{{.Architecture}}' "$ref")"
    actual_os="$(podman image inspect --format '{{.Os}}' "$ref")"
    id="${id#sha256:}"
    [[ "$id" =~ ^[a-f0-9]{64}$ && "$actual_arch" == amd64 && "$actual_os" == linux ]] ||
      die "Pinned $component image is not Linux AMD64 with an inspectable ID"
    id="sha256:$id"
    if [[ "$component" == minio && -n "$local_archive" && "$id" != "$ref" ]]; then
      die "Local MinIO image ID differs from images.minio"
    fi
    manifest="$(jq -c --arg component "$component" --arg ref "$ref" \
      --arg archive "$(basename "$archive")" --arg sha "$(sha_file "$archive")" --arg id "$id" \
      '.images[$component] = {ref:$ref, archive:$archive, sha256:$sha, id:$id}' <<< "$manifest")"
  done
  manifest="$(jq -c --arg sha "$mc_sha" '.mc = {archive:"mc-linux-amd64",sha256:$sha}' <<< "$manifest")"
  printf '%s\n' "$manifest" > "$ARTIFACT_DIR/manifest.json"
  log "Prepared three pinned OCI archives, mc, and nonsecret manifest at $ARTIFACT_DIR"
}

load_images() {
  local manifest="$ARTIFACT_DIR/manifest.json" component ref archive sha id loaded_id
  [[ -f "$manifest" && ! -L "$manifest" ]] || die "Missing artifact manifest"
  jq -e '.schema == 1 and (.images | keys == ["minio","postgres","redis"])' "$manifest" >/dev/null ||
    die "Invalid artifact manifest"
  [[ "$(jq -r '.minio.distribution' "$manifest")" == "$MINIO_DISTRIBUTION" && \
     "$(jq -r '.minio.sourceCommit' "$manifest")" == "$MINIO_SOURCE_COMMIT" ]] ||
    die "MinIO distribution or source commit differs from prepared artifacts"
  for component in minio postgres redis; do
    ref="$(image_ref "$component")"
    [[ "$(jq -r --arg c "$component" '.images[$c].ref' "$manifest")" == "$ref" ]] ||
      die "$component archive belongs to a different image reference"
    archive="$(jq -r --arg c "$component" '.images[$c].archive' "$manifest")"
    [[ "$archive" == "$component.oci.tar" ]] || die "Unexpected archive path for $component"
    sha="$(jq -r --arg c "$component" '.images[$c].sha256' "$manifest")"
    id="$(jq -r --arg c "$component" '.images[$c].id' "$manifest")"
    [[ "$id" =~ ^sha256:[a-f0-9]{64}$ ]] || die "Invalid pinned image ID for $component"
    check_sha "$ARTIFACT_DIR/$archive" "$sha"
    podman load --input "$ARTIFACT_DIR/$archive" >/dev/null
    loaded_id="$(podman image inspect --format '{{.Id}}' "$id" 2>/dev/null || true)"
    loaded_id="sha256:${loaded_id#sha256:}"
    [[ "$loaded_id" == "$id" ]] || die "Loaded $component image ID differs from manifest"
    printf -v "${component^^}_IMAGE_ID" '%s' "$id"
  done
  local mc_archive mc_sha
  mc_archive="$(jq -r '.mc.archive' "$manifest")"
  mc_sha="$(jq -r '.mc.sha256' "$manifest")"
  [[ "$mc_archive" == mc-linux-amd64 ]] || die "Unexpected mc archive path"
  check_sha "$ARTIFACT_DIR/$mc_archive" "$mc_sha"
  install -m 0755 "$ARTIFACT_DIR/$mc_archive" /usr/local/bin/mc-test-deps
  check_sha /usr/local/bin/mc-test-deps "$mc_sha"
}

write_unit() {
  local name="$1" body="$2" unit="/etc/systemd/system/test-deps-$1.service"
  [[ ! -e "$unit" ]] || die "Refusing to overwrite existing $unit"
  printf '%s\n' "$body" > "$unit"
  chmod 0644 "$unit"
}

wait_http() {
  local url="$1" attempt
  for ((attempt=0; attempt<60; attempt++)); do
    curl --noproxy '*' --silent --fail --max-time 2 "$url" >/dev/null 2>&1 && return 0
    sleep 2
  done
  die "Timed out waiting for private MinIO health endpoint"
}

write_mc_config() {
  python3 - "$MC_CONFIG_DIR/config.json" "$1" "$2" "$BIND_IP" <<'PY'
import json, os, pathlib, sys
path, user_file, password_file, ip = sys.argv[1:]
payload = {"version": "10", "aliases": {"testdeps": {
    "url": f"http://{ip}:9000",
    "accessKey": pathlib.Path(user_file).read_text(),
    "secretKey": pathlib.Path(password_file).read_text(),
    "api": "S3v4", "path": "auto",
}}}
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, "w") as out:
    json.dump(payload, out)
PY
}

mc_testdeps() { /usr/local/bin/mc-test-deps --config-dir "$MC_CONFIG_DIR" "$@"; }

install_deps() {
  require_amd64
  [[ "$(id -u)" == 0 ]] || die "install requires root"
  for tool in podman systemctl mountpoint findmnt ip curl stat; do need "$tool"; done
  [[ -n "$ARTIFACT_DIR" ]] || usage
  require_absolute_path "$ARTIFACT_DIR" artifact-dir
  read_common
  local storage_mode minio_license_file="" minio_license_opt="" minio_license_arg="" minio_command="server"
  storage_mode="$(cfg '.host.storageMode // "mounted-volume"')"
  case "$storage_mode" in
    mounted-volume)
      [[ -d "$DATA_ROOT" ]] || die "dataRoot mount point does not exist"
      mountpoint -q "$DATA_ROOT" || die "dataRoot must itself be a mounted persistent volume" ;;
    encrypted-root-ebs)
      [[ "$(cfg '.host.rootEbsEncryptedConfirmed // false')" == true ]] ||
        die "encrypted-root-ebs requires host.rootEbsEncryptedConfirmed: true"
      install -d -m 0700 "$DATA_ROOT"
      [[ "$(findmnt -n -o TARGET --target "$DATA_ROOT")" == / ]] ||
        die "encrypted-root-ebs requires dataRoot on the root filesystem"
      log "Using operator-confirmed encrypted root EBS; preserve this instance/volume for dependency data" ;;
    *) die "host.storageMode must be mounted-volume or encrypted-root-ebs" ;;
  esac
  case "$(findmnt -n -o FSTYPE --target "$DATA_ROOT")" in
    tmpfs|ramfs|overlay) die "dataRoot is not persistent storage" ;;
  esac
  ip -4 -o addr show | awk '{print $4}' | cut -d/ -f1 | grep -Fxq "$BIND_IP" ||
    die "host.bindIp is not assigned to this host"
  [[ ! -e "$OUTPUT_FILE" ]] || die "Refusing to overwrite protected outputFile"
  [[ -d "$(dirname "$OUTPUT_FILE")" ]] || die "outputFile parent directory does not exist"
  if [[ "$MINIO_DISTRIBUTION" == aistor ]]; then
    minio_license_file="$(required_cfg '.minio.licenseFile')"
    check_protected_file "$minio_license_file" minio.licenseFile
    minio_license_opt="--secret source=test-deps-minio-license,type=mount,target=minio-license"
    minio_license_arg="--license /run/secrets/minio-license"
    minio_command="minio server"
  fi
  for name in minio postgres redis; do
    [[ ! -e "/etc/systemd/system/test-deps-$name.service" ]] ||
      die "Dependency unit test-deps-$name already exists; use systemctl restart instead of reinstalling"
  done
  for name in test-deps-minio-user test-deps-minio-pass test-deps-postgres-pass \
              test-deps-redis-acl test-deps-minio-license; do
    ! podman secret inspect "$name" >/dev/null 2>&1 ||
      die "Podman secret $name already exists; refusing to replace it"
  done
  load_images
  local postgres_version
  postgres_version="$(podman run --rm --pull=never --entrypoint postgres "$POSTGRES_IMAGE_ID" --version)" ||
    die "Cannot inspect the pinned PostgreSQL image version"
  [[ "$postgres_version" == 'postgres (PostgreSQL) 16.'* ]] ||
    die "This test helper requires PostgreSQL 16 with /var/lib/postgresql/data layout"
  install -d -m 0700 "$DATA_ROOT/config" "$DATA_ROOT/config/credentials"
  local minio_user_file minio_pass_file pg_pass_file redis_pass_file
  minio_user_file="$(credential_file minioUserFile minio-root-user user)"
  minio_pass_file="$(credential_file minioPasswordFile minio-root-password password)"
  pg_pass_file="$(credential_file postgresPasswordFile postgres-password password)"
  redis_pass_file="$(credential_file redisPasswordFile redis-password password)"
  install -d -m 0700 "$DATA_ROOT/minio" "$DATA_ROOT/postgres" "$DATA_ROOT/redis"
  INSTALL_PHASE=applying
  trap rollback_partial_install EXIT
  podman secret create test-deps-minio-user "$minio_user_file" >/dev/null
  podman secret create test-deps-minio-pass "$minio_pass_file" >/dev/null
  podman secret create test-deps-postgres-pass "$pg_pass_file" >/dev/null
  if [[ "$MINIO_DISTRIBUTION" == aistor ]]; then
    podman secret create test-deps-minio-license "$minio_license_file" >/dev/null
  fi
  local redis_hash redis_uid redis_gid
  redis_hash="$(sha256sum "$redis_pass_file" | awk '{print $1}')"
  # Redis 7 applies channel patterns separately from key patterns. LAS uses
  # pub/sub for run streams; without &* AUTH and PING succeed but runs fail.
  printf 'user default on #%s ~* &* +@all\n' "$redis_hash" > "$DATA_ROOT/config/redis-users.acl"
  chmod 0600 "$DATA_ROOT/config/redis-users.acl"
  redis_uid="$(podman run --rm --pull=never --entrypoint id "$REDIS_IMAGE_ID" -u redis)" ||
    die "Cannot resolve the pinned Redis image's redis UID"
  redis_gid="$(podman run --rm --pull=never --entrypoint id "$REDIS_IMAGE_ID" -g redis)" ||
    die "Cannot resolve the pinned Redis image's redis GID"
  [[ "$redis_uid" =~ ^[0-9]+$ && "$redis_gid" =~ ^[0-9]+$ ]] ||
    die "Pinned Redis image returned invalid redis identity"
  podman secret create test-deps-redis-acl "$DATA_ROOT/config/redis-users.acl" >/dev/null
  cat > "$DATA_ROOT/config/redis.conf" <<'EOF'
bind 0.0.0.0
protected-mode yes
port 6379
appendonly yes
dir /data
aclfile /run/secrets/redis-acl
EOF
  chmod 0644 "$DATA_ROOT/config/redis.conf"

  write_unit minio "[Unit]
Description=Test-only private MinIO for LAS cluster
Wants=network-online.target
After=network-online.target
RequiresMountsFor=$DATA_ROOT
[Service]
Type=simple
ExecStartPre=-/usr/bin/podman rm -f test-deps-minio
ExecStart=/usr/bin/podman run --rm --name test-deps-minio --pull=never --publish $BIND_IP:9000:9000 --tmpfs /tmp:rw,size=64m --env HOME=/tmp --secret source=test-deps-minio-user,type=mount,target=minio-user --secret source=test-deps-minio-pass,type=mount,target=minio-pass $minio_license_opt --env MINIO_ROOT_USER_FILE=/run/secrets/minio-user --env MINIO_ROOT_PASSWORD_FILE=/run/secrets/minio-pass --volume $DATA_ROOT/minio:/data:Z,U $MINIO_IMAGE_ID $minio_command /data --address :9000 $minio_license_arg
ExecStop=/usr/bin/podman stop -t 30 test-deps-minio
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target"
  write_unit postgres "[Unit]
Description=Test-only private PostgreSQL for LAS cluster
Wants=network-online.target
After=network-online.target
RequiresMountsFor=$DATA_ROOT
[Service]
Type=simple
ExecStartPre=-/usr/bin/podman rm -f test-deps-postgres
ExecStart=/usr/bin/podman run --rm --name test-deps-postgres --pull=never --publish $BIND_IP:5432:5432 --secret source=test-deps-postgres-pass,type=mount,target=postgres-pass --env POSTGRES_PASSWORD_FILE=/run/secrets/postgres-pass --env POSTGRES_USER=$DB_USER --env POSTGRES_DB=$DB_NAME --volume $DATA_ROOT/postgres:/var/lib/postgresql/data:Z,U $POSTGRES_IMAGE_ID
ExecStop=/usr/bin/podman stop -t 30 test-deps-postgres
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target"
  write_unit redis "[Unit]
Description=Test-only private Redis for LAS cluster
Wants=network-online.target
After=network-online.target
RequiresMountsFor=$DATA_ROOT
[Service]
Type=simple
ExecStartPre=-/usr/bin/podman rm -f test-deps-redis
ExecStart=/usr/bin/podman run --rm --name test-deps-redis --pull=never --publish $BIND_IP:6379:6379 --secret source=test-deps-redis-acl,type=mount,target=redis-acl,uid=$redis_uid,gid=$redis_gid,mode=0400 --volume $DATA_ROOT/redis:/data:Z,U --volume $DATA_ROOT/config/redis.conf:/etc/redis/redis.conf:ro,Z $REDIS_IMAGE_ID redis-server /etc/redis/redis.conf
ExecStop=/usr/bin/podman stop -t 30 test-deps-redis
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target"
  systemctl daemon-reload
  systemctl enable --now test-deps-minio.service test-deps-postgres.service test-deps-redis.service >/dev/null
  wait_http "http://$BIND_IP:9000/minio/health/live"
  local attempt
  for ((attempt=0; attempt<60; attempt++)); do
    podman exec test-deps-postgres pg_isready -U "$DB_USER" -d "$DB_NAME" >/dev/null 2>&1 && break
    sleep 2
  done
  ((attempt < 60)) || die "PostgreSQL did not become ready"
  [[ "$(podman exec test-deps-postgres sh -c \
    'PGPASSWORD="$(cat /run/secrets/postgres-pass)" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "SELECT 1"' \
    2>/dev/null | tr -d '[:space:]')" == 1 ]] || die "PostgreSQL password authentication failed"
  python3 - "$BIND_IP" "$redis_pass_file" <<'PY' || die "Redis authentication failed"
import pathlib, socket, sys, time
host, password_file = sys.argv[1:]
password = pathlib.Path(password_file).read_bytes()
def packet(*parts):
    return b"*" + str(len(parts)).encode() + b"\r\n" + b"".join(
        b"$" + str(len(p)).encode() + b"\r\n" + p + b"\r\n" for p in parts)
for _ in range(60):
    try:
        with socket.create_connection((host, 6379), 2) as sock:
            sock.sendall(packet(b"AUTH", password))
            if not sock.recv(128).startswith(b"+OK"):
                raise RuntimeError("AUTH rejected")
            sock.sendall(packet(b"PING"))
            if not sock.recv(128).startswith(b"+PONG"):
                raise RuntimeError("PING rejected")
            sock.sendall(packet(b"SUBSCRIBE", b"las_acl_probe"))
            response = sock.recv(256)
            if response.startswith(b"*3") and b"subscribe" in response.lower():
                sys.exit(0)
            raise RuntimeError("SUBSCRIBE rejected")
    except (OSError, RuntimeError):
        time.sleep(2)
sys.exit(1)
PY

  # A protected mc config avoids URL-encoding ambiguities for generated
  # credentials and keeps them out of command arguments and process output.
  MC_CONFIG_DIR="$(mktemp -d /run/test-deps-mc.XXXXXXXX)"
  write_mc_config "$minio_user_file" "$minio_pass_file"
  mc_testdeps mb "testdeps/$BUCKET" --ignore-existing >/dev/null 2>&1 ||
    die "Could not create MinIO bucket"
  local policy_file="$DATA_ROOT/config/bucket-policy.json" scoped_file
  jq -n --arg bucket "$BUCKET" '{Version:"2012-10-17",Statement:[
    {Effect:"Allow",Action:["s3:ListBucket","s3:GetBucketLocation"],Resource:["arn:aws:s3:::" + $bucket]},
    {Effect:"Allow",Action:["s3:GetObject","s3:PutObject","s3:DeleteObject"],Resource:["arn:aws:s3:::" + $bucket + "/*"]}
  ]}' > "$policy_file"
  chmod 0600 "$policy_file"
  local app_user_file="$DATA_ROOT/config/credentials/minio-app-access-key"
  local app_pass_file="$DATA_ROOT/config/credentials/minio-app-secret-key"
  if [[ -e "$app_user_file" || -e "$app_pass_file" ]]; then
    [[ -f "$app_user_file" && -f "$app_pass_file" ]] ||
      die "Only one scoped MinIO key file exists; repair the pair before retrying"
    check_protected_file "$app_user_file" minio-app-access-key
    check_protected_file "$app_pass_file" minio-app-secret-key
    log "Reusing the existing bucket-scoped MinIO application key"
  else
    scoped_file="$(mktemp /run/test-deps-scoped.XXXXXXXX)"
    SCOPED_TMP="$scoped_file"
    chmod 0600 "$scoped_file"
    mc_testdeps --json admin accesskey create testdeps/ \
      --policy "$policy_file" --name las-test-cluster > "$scoped_file" 2>/dev/null ||
      die "Could not create bucket-scoped MinIO application key"
    jq -e -s 'map(select(.accessKey and .secretKey)) | length == 1' "$scoped_file" >/dev/null ||
      die "MinIO did not return one application key"
    python3 - "$scoped_file" "$app_user_file" "$app_pass_file" <<'PY'
import json, os, pathlib, sys
source, user_path, secret_path = sys.argv[1:]
records = [json.loads(line) for line in pathlib.Path(source).read_text().splitlines()]
records = [item for item in records if item.get("accessKey") and item.get("secretKey")]
if len(records) != 1:
    raise SystemExit("MinIO did not return one application key")
for path, value in ((user_path, records[0]["accessKey"]), (secret_path, records[0]["secretKey"])):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as out:
        out.write(value)
PY
    cleanup_scoped_tmp
  fi
  write_mc_config "$app_user_file" "$app_pass_file"
  printf 'private dependency probe' | mc_testdeps pipe "testdeps/$BUCKET/.bootstrap-probe" >/dev/null 2>&1 ||
    die "Bucket-scoped application key cannot write to MinIO"
  mc_testdeps ls "testdeps/$BUCKET/" 2>/dev/null | grep -Fq '.bootstrap-probe' ||
    die "Bucket-scoped application key cannot list MinIO objects"
  [[ "$(mc_testdeps cat "testdeps/$BUCKET/.bootstrap-probe" 2>/dev/null)" == 'private dependency probe' ]] ||
    die "Bucket-scoped application key cannot read from MinIO"
  mc_testdeps rm "testdeps/$BUCKET/.bootstrap-probe" >/dev/null 2>&1 ||
    die "Bucket-scoped application key cannot delete from MinIO"
  cleanup_mc_config

  python3 - "$OUTPUT_FILE" "$BIND_IP" "$BUCKET" "$DB_USER" "$DB_NAME" \
    "$app_user_file" "$app_pass_file" "$pg_pass_file" "$redis_pass_file" <<'PY'
import json, os, pathlib, sys, urllib.parse
path, ip, bucket, user, db, app_user, app_pass, pg_pass, redis_pass = sys.argv[1:]
read = lambda p: pathlib.Path(p).read_text()
quote = lambda s: urllib.parse.quote(s, safe="")
payload = {
    "minioEndpoint": f"http://{ip}:9000",
    "minioBucket": bucket,
    "minioAccessKey": read(app_user),
    "minioSecretKey": read(app_pass),
    "minioSecretKeyFile": app_pass,
    "postgresConnectionUrl": f"postgresql://{quote(user)}:{quote(read(pg_pass))}@{ip}:5432/{quote(db)}?sslmode=disable",
    "redisConnectionUrl": f"redis://:{quote(read(redis_pass))}@{ip}:6379/0",
}
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as out:
    json.dump(payload, out)
    out.write("\n")
PY
  log "Installed private MinIO, PostgreSQL, and Redis; protected connection file: $OUTPUT_FILE"
  log "Keep the dependency host and its persistent volume available through cluster restarts"
  INSTALL_PHASE=done
  trap - EXIT
}

create_secrets() {
  require_amd64
  [[ "$(id -u)" == 0 ]] || die "secrets requires root to read protected files"
  need kubectl
  read_common
  [[ -f "$OUTPUT_FILE" ]] || die "Protected connection file is missing: $OUTPUT_FILE"
  check_protected_file "$OUTPUT_FILE" host.outputFile
  [[ -f "$KUBECONFIG_PATH" ]] || die "--kubeconfig must name a readable file"
  check_protected_file "$LICENSE_FILE" license-file
  kubectl --kubeconfig "$KUBECONFIG_PATH" get namespace "$NAMESPACE" >/dev/null ||
    die "Kubernetes namespace $NAMESPACE is unavailable"
  python3 - "$OUTPUT_FILE" "$LICENSE_FILE" "$NAMESPACE" \
    "$LICENSE_SECRET" "$POSTGRES_SECRET" "$REDIS_SECRET" <<'PY' | kubectl --kubeconfig "$KUBECONFIG_PATH" apply --server-side --force-conflicts --field-manager=airgap-test-dependencies -f - >/dev/null
import base64, json, pathlib, sys
connection = json.loads(pathlib.Path(sys.argv[1]).read_text())
license_bytes = pathlib.Path(sys.argv[2]).read_bytes()
namespace = sys.argv[3]
license_secret, postgres_secret, redis_secret = sys.argv[4:7]
if not license_bytes or not connection["postgresConnectionUrl"] or not connection["redisConnectionUrl"]:
    raise SystemExit("Required LAS secret source is empty")
def secret(name, key, value):
    return {"apiVersion":"v1","kind":"Secret","metadata":{"name":name,"namespace":namespace},
            "type":"Opaque","data":{key:base64.b64encode(value).decode()}}
items = [
    secret(license_secret,"langgraph_cloud_license_key",license_bytes),
    secret(postgres_secret,"postgres_connection_url",connection["postgresConnectionUrl"].encode()),
    secret(redis_secret,"redis_connection_url",connection["redisConnectionUrl"].encode()),
]
print(json.dumps({"apiVersion":"v1","kind":"List","items":items}))
PY
  log "Applied $LICENSE_SECRET, $POSTGRES_SECRET, and $REDIS_SECRET Secret references in $NAMESPACE"
}

case "$MODE" in
  prepare) prepare ;;
  install) install_deps ;;
  secrets) create_secrets ;;
esac
