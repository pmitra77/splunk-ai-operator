# Embedded LAS chart rollout and queue autoscaling patch

The operator embeds `langgraph-cloud-0.3.5-splunk.2.tgz`, SHA256 `4fa2580f6e38254f399cc23e230f314dfdf42566f71df41ff1e4b23d36426058`. It derives from the pinned upstream `langgraph-cloud-0.3.4.tgz`, SHA256 `31dab99c9b1c6ddfb8672a4a9a287a044d50bb628665c641eb2b1170833df953`, without changing the upstream appVersion `0.2.3`.

`rollout-policy.patch` is the complete source difference. It preserves `RollingUpdate` with zero surge and one unavailable on the API and queue Deployments, and renders the ERD queue HPA scale-down policy: a 300-second stabilization window and at most one Pod per 60 seconds. Scale-up behavior remains the Kubernetes default. The operator uses a CPU-only queue HPA; with autoscaling disabled, the queue stays at one fixed replica and no HPA is rendered.

To reproduce the archive from this directory:

```sh
./rebuild-rollout-package.sh /tmp
shasum -a 256 /tmp/langgraph-cloud-0.3.5-splunk.2.tgz
```

The package checksum is verified by `LASReconciler` before every install or upgrade. A changed package must receive a new version and checksum.
