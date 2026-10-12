# Embedded LAS chart rollout patch

The operator embeds `langgraph-cloud-0.3.5-splunk.1.tgz`, SHA256 `ef80afdef0c44a232946e67dc84521bed4e2b3a0234643eafc2ec811b6b092c8`. It derives from the pinned upstream `langgraph-cloud-0.3.4.tgz`, SHA256 `31dab99c9b1c6ddfb8672a4a9a287a044d50bb628665c641eb2b1170833df953`, without changing the upstream appVersion `0.2.3`.

`rollout-policy.patch` is the complete source difference. It adds `spec.strategy` rendering to the API and queue Deployments, sets zero surge and one unavailable as chart defaults, and changes only the chart package version. HPA behavior is handled separately.

To reproduce the archive from this directory:

```sh
./rebuild-rollout-package.sh /tmp
shasum -a 256 /tmp/langgraph-cloud-0.3.5-splunk.1.tgz
```

The package checksum is verified by `LASReconciler` before every install or upgrade. A changed package must receive a new version and checksum.
