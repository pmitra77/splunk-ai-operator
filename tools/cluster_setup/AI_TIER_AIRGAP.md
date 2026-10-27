# AI Tier CPU-only offline artifact bundle

Prepare a disconnected CPU-only bundle on a connected staging machine from the
exact installer config that will be used in the cluster. The selected config
must set aiPlatform.profile to cpuOnly and cluster.airgap to true.

    PYYAML_VERSION=6.0.2 \
    tools/cluster_setup/prepare_airgap_bundle.sh \
      --config tools/cluster_setup/k0s-cluster-config-agentruntime.yaml \
      --k0s-version v1.31.2+k0s.0 \
      --mirror-registry registry.internal.example.com \
      --output-dir /mnt/transfer

The builder collects k0s system, installer add-on, and configured workload
images into OCI archives. image-inventory.json records source references,
immutable manifest digests, OS/architecture, archive mappings, and optional
internal mirror references. The bundle also contains the exact LAS Helm chart
archive embedded by this operator revision and its SHA-256 digest. The bundle
checksum manifest covers the inventory and staged artifacts.

Skopeo reads credentials from its normal local auth store. No registry auth
file or credential values are copied into the bundle. Authentication and TLS
trust failures stop preparation with a sanitized error.

On the air-gapped installer, checksums, configured image closure, inventory,
archives, and the LAS chart hash are verified before cluster work begins. The
CPU-only installer uses staged artifacts and does not add a public image/chart
fallback.

The fresh CPU-only install and replacement-Pod pull still require isolated
environment qualification. Record the approved image digest and verify a
replacement Pod starts from the staged/internal image after deleting the
original Pod.
