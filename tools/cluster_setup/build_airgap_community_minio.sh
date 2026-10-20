#!/usr/bin/env bash
# Build the test cluster's community MinIO image from a pinned local checkout.
# This is a preparation step, separate from the customer-facing installer and
# from install_airgap_test_dependencies.sh. It never downloads source or modules.
set -euo pipefail
set +x
umask 077

SOURCE_COMMIT=58659f26f45f2c189f5fd1c7267da60c10024daa
IMAGE_TAG="localhost/las-test-minio-community:${SOURCE_COMMIT:0:12}"

usage() {
  cat >&2 <<'EOF'
Usage: build_airgap_community_minio.sh --source-dir DIR --output-dir DIR

Run on a connected Linux x86_64 preparation host with Go 1.24, Git, Podman,
and a prepopulated Go module cache. DIR must be a local checkout of
github.com/minio/minio at commit 58659f26f45f2c189f5fd1c7267da60c10024daa
with no local changes. The output directory must not already exist.

The build disables Go module downloads and container networking. It creates:
  minio-community.oci.tar       Linux AMD64 OCI image archive
  minio-community-build.json    Source, image ID, and SHA-256 provenance
  Containerfile                 Exact image recipe
  minio                         Statically linked Linux AMD64 binary

Use minio-community.oci.tar as minio.localArchive and its recorded SHA-256 as
minio.localArchiveSha256 in the test dependency config. Use imageId as
images.minio. This image is for the MPR-63 test setup, not customer packaging.
EOF
  exit 2
}

die() { printf '[minio-build] ERROR: %s\n' "$*" >&2; exit 1; }
log() { printf '[minio-build] %s\n' "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || die "Missing required tool: $1"; }

SOURCE_DIR="" OUTPUT_DIR=""
while (($#)); do
  case "$1" in
    --source-dir) (($# >= 2)) || usage; SOURCE_DIR="$2"; shift 2 ;;
    --output-dir) (($# >= 2)) || usage; OUTPUT_DIR="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
[[ -n "$SOURCE_DIR" && -n "$OUTPUT_DIR" ]] || usage
[[ "$SOURCE_DIR" == /* && "$OUTPUT_DIR" == /* ]] ||
  die "--source-dir and --output-dir must be absolute paths"
[[ "$SOURCE_DIR" != *$'\n'* && "$OUTPUT_DIR" != *$'\n'* &&
   "$SOURCE_DIR" != *$'\r'* && "$OUTPUT_DIR" != *$'\r'* ]] ||
  die "Paths must be single-line"
[[ "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] ||
  die "This build requires Linux x86_64"
for tool in git go podman sha256sum jq mktemp; do need "$tool"; done

[[ -d "$SOURCE_DIR" && ! -L "$SOURCE_DIR" ]] ||
  die "--source-dir must be a local checkout directory"
SOURCE_DIR="$(cd "$SOURCE_DIR" && pwd -P)"
[[ "$(git -C "$SOURCE_DIR" rev-parse --show-toplevel 2>/dev/null)" == "$SOURCE_DIR" ]] ||
  die "--source-dir must be the root of its Git checkout"
[[ "$(git -C "$SOURCE_DIR" rev-parse HEAD)" == "$SOURCE_COMMIT" ]] ||
  die "MinIO checkout HEAD is not the pinned source commit $SOURCE_COMMIT"
[[ -z "$(git -C "$SOURCE_DIR" status --porcelain --untracked-files=all)" ]] ||
  die "MinIO checkout must have no local changes or untracked files"
[[ "$(sed -n '1p' "$SOURCE_DIR/go.mod")" == 'module github.com/minio/minio' ]] ||
  die "MinIO checkout has an unexpected Go module"
[[ -f "$SOURCE_DIR/go.sum" ]] || die "MinIO checkout has no go.sum"
GO_VERSION="$(GOTOOLCHAIN=local go env GOVERSION)"
[[ "$GO_VERSION" =~ ^go1\.24(\.[0-9]+)?$ ]] ||
  die "Go 1.24 is required; found $GO_VERSION"

[[ ! -e "$OUTPUT_DIR" ]] || die "Refusing to overwrite --output-dir"
OUTPUT_PARENT="$(dirname "$OUTPUT_DIR")"
[[ -d "$OUTPUT_PARENT" ]] || die "Output parent directory does not exist"
OUTPUT_PARENT="$(cd "$OUTPUT_PARENT" && pwd -P)"
OUTPUT_DIR="$OUTPUT_PARENT/$(basename "$OUTPUT_DIR")"
STAGING_DIR="$(mktemp -d "$OUTPUT_PARENT/.minio-community-build.XXXXXXXX")"
cleanup() { rm -rf -- "$STAGING_DIR"; }
trap cleanup EXIT

log "Building pinned MinIO source with the local Go module cache"
(
  cd "$SOURCE_DIR"
  export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
  export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=
  go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags='-s -w -buildid=' -o "$STAGING_DIR/minio" .
)
chmod 0555 "$STAGING_DIR/minio"
BINARY_SHA="$(sha256sum "$STAGING_DIR/minio" | awk '{print $1}')"
GO_MOD_SHA="$(sha256sum "$SOURCE_DIR/go.mod" | awk '{print $1}')"
GO_SUM_SHA="$(sha256sum "$SOURCE_DIR/go.sum" | awk '{print $1}')"

cat > "$STAGING_DIR/Containerfile" <<'EOF'
FROM scratch
COPY --chmod=0555 minio /minio
EXPOSE 9000 9001
ENTRYPOINT ["/minio"]
EOF

log "Building Linux AMD64 OCI image without container networking"
podman build --format oci --platform linux/amd64 --pull=never \
  --network=none --no-cache --tag "$IMAGE_TAG" \
  --file "$STAGING_DIR/Containerfile" "$STAGING_DIR" >/dev/null
IMAGE_ID="$(podman image inspect --format '{{.Id}}' "$IMAGE_TAG")"
IMAGE_ARCH="$(podman image inspect --format '{{.Architecture}}' "$IMAGE_TAG")"
IMAGE_OS="$(podman image inspect --format '{{.Os}}' "$IMAGE_TAG")"
IMAGE_ID="sha256:${IMAGE_ID#sha256:}"
[[ "$IMAGE_ID" =~ ^sha256:[a-f0-9]{64}$ && "$IMAGE_ARCH" == amd64 && "$IMAGE_OS" == linux ]] ||
  die "Built image does not have a Linux AMD64 SHA-256 image ID"

ARCHIVE="$STAGING_DIR/minio-community.oci.tar"
podman save --format oci-archive --output "$ARCHIVE" "$IMAGE_TAG" >/dev/null
ARCHIVE_SHA="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
CONTAINERFILE_SHA="$(sha256sum "$STAGING_DIR/Containerfile" | awk '{print $1}')"
jq -n \
  --arg sourceRepository 'github.com/minio/minio' \
  --arg sourceCommit "$SOURCE_COMMIT" \
  --arg goVersion "$GO_VERSION" \
  --arg goModSha256 "$GO_MOD_SHA" \
  --arg goSumSha256 "$GO_SUM_SHA" \
  --arg imageTag "$IMAGE_TAG" \
  --arg imageId "$IMAGE_ID" \
  --arg imageOs "$IMAGE_OS" \
  --arg imageArch "$IMAGE_ARCH" \
  --arg binarySha256 "$BINARY_SHA" \
  --arg containerfileSha256 "$CONTAINERFILE_SHA" \
  --arg archiveSha256 "$ARCHIVE_SHA" \
  '{schema:1,sourceRepository:$sourceRepository,sourceCommit:$sourceCommit,
    goModSha256:$goModSha256,goSumSha256:$goSumSha256,
    goVersion:$goVersion,build:{cgoEnabled:false,goos:"linux",goarch:"amd64",
    moduleDownload:false,containerNetwork:false},imageTag:$imageTag,
    imageId:$imageId,imageOs:$imageOs,imageArch:$imageArch,
    binary:{file:"minio",sha256:$binarySha256},
    containerfile:{file:"Containerfile",sha256:$containerfileSha256},
    archive:{file:"minio-community.oci.tar",sha256:$archiveSha256}}' \
  > "$STAGING_DIR/minio-community-build.json"

[[ ! -e "$OUTPUT_DIR" ]] || die "Output directory appeared during the build"
mv -T -- "$STAGING_DIR" "$OUTPUT_DIR"
trap - EXIT
log "Created pinned community MinIO image artifacts in $OUTPUT_DIR"
