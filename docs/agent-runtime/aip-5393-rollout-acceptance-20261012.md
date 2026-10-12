# AIP-5393 rollout acceptance record — 2026-10-12

Target: `agentruntime-dev` k0s test cluster, namespace `ai-platform`. This record uses the story branch based on `agent-runtime/ai-tier` commit `21b8be42194203e89aa7e9dca2a4fad8da45ef2c`. It distinguishes the isolated chart rollout from operator-owned reconciliation. No credential or connection URL is recorded.

## Local contract and package checks

- The operator supplies `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` independently to API and queue, fixes API autoscaling off and its replica count at one, and uses the same desired values for install and upgrade.
- The embedded chart is `langgraph-cloud-0.3.5-splunk.1`, SHA256 `ef80afdef0c44a232946e67dc84521bed4e2b3a0234643eafc2ec811b6b092c8`, built from pinned upstream `0.3.4`. Its templates render the supplied strategy into both Deployment specs. The source patch and deterministic rebuild command are in `pkg/ai/features/agentruntime/chart/`.
- The focused test parses both rendered Kubernetes Deployments and checks their strategy and one API replica. It checks chart version/digest and that the new desired values differ from the old ones but compare equal on the next evaluation. All four affected Go packages passed their tests; `helm lint`, deterministic package reconstruction, and `git diff --check` passed.

## Isolated chart upgrade on the test cluster

The pre-existing operator-owned LAS release `las-agentruntime-dev-ai-p-eb88ba0f` was left untouched. Its chart was `0.3.4` at Helm revision 6 and its AIService Ready condition was True. A disposable Helm release `aip5393-rollout` used the same sample graph image and license reference but its own PostgreSQL database `aip5393_rollout` and Redis DB 15 (verified empty before use). Both fixture Pods were pinned to CPU worker `ip-10-0-34-28.splunkcorp.com`, so the worker's 8-CPU allocatable capacity had 6010m in requests before the fixture and 7260m with its one API and one queue. The resulting 740m of request headroom could not fit a second 1-CPU queue worker.

| Observation | Baseline | After upgrade |
| --- | --- | --- |
| Helm release | Revision 1, chart `0.3.4`, deployed | Revision 2, chart `0.3.5-splunk.1`, deployed (`--wait --atomic --timeout 5m`) |
| API Deployment | 1/1 Ready, default 25% surge / 25% unavailable | 1/1 Ready, `RollingUpdate` / 0 surge / 1 unavailable |
| Queue Deployment | 1/1 Ready, default 25% surge / 25% unavailable | 1/1 Ready, `RollingUpdate` / 0 surge / 1 unavailable |
| Service | `/ok` returned HTTP 200 and `{"ok":true}` | EndpointSlice ready; `/ok` returned HTTP 200 and `{"ok":true}` |
| Sample work | Not submitted before upgrade | `echo` `runs/wait` HTTP 200 returned the expected deterministic response; test thread `01a12fea-4f39-7510-a6ab-8d7e4d15ae92` was deleted (HTTP 204) |

The upgrade explicitly supplied the fixed strategy and changed a Pod annotation for both workloads to force replacement. The credential-free Pod watcher recorded the following UTC order:

1. **11:38:31:** old API UID `332e42b0-0177-4b80-b4e1-e5d3ae177e8a` and old queue UID `a3b2165e-fdce-4a3e-89eb-272df36e65f7` were both Ready on the pinned node.
2. **11:39:07:** both old Pods were terminating; both new Pods existed but the queue replacement was Pending with no assigned node.
3. **11:39:08–09:** the old queue Pod disappeared, then the new queue was assigned to the pinned node. Both replacement Pods were running but not yet Ready.
4. **11:39:19:** replacement API UID `584701cf-8672-48ca-b32b-4effb6723a34` and queue UID `d93d032e-9d82-4d5d-859a-8168d81dddd6` were Ready. The fixture Service had a ready EndpointSlice.

The upgrade used Helm; no live LAS Deployment patch was issued. The existing operator-owned release remained at revision 6, chart `0.3.4`, with AIService Ready=True. This demonstrates the packaged chart and constrained one-replica rollout. The Go test covers the operator's desired-value/no-op comparison, but this direct Helm fixture does **not** by itself establish a live `LASReconciler` upgrade. An operator-owned test is still required for that acceptance claim. Active-run interruption, retry/idempotency, and HPA scale-down are separate qualification work; the smoke run above began after the upgrade.

## Operator-owned test gate

A Linux/amd64 manager was built from this branch (binary SHA256 `2efbed0000ae3b9c5821ae320b38db5a3e97e81de2335db13b0d294c999eb3c5`) and packaged with the current configuration and distroless runtime. The unique test image was pushed to the existing ECR repository as `aip5393-rollout-20261012-21b8be4`, immutable digest `sha256:7092502ae18ec1d5fd5ebcfc1ad7194efdf0dba0ad4c3abcbe7949b8ca895b9c`. The live CRD supports `las.image` and `assistantId`. The existing sample image/tag and assistant `echo` were staged in both AIPlatform and AIService so a candidate manager would not change the runtime graph image; both preparatory fields were later removed.

The cluster has one cluster-wide operator manager. Replacing it would automatically reconcile the existing primary AIService and upgrade its Helm release. Automatic approval review **rejected that manager Deployment update** because it could disrupt the shared primary LAS service and make rollback trigger another rollout. The candidate image was **not deployed**. The current manager remained `las-poc-20261005-114954-483a832`; the primary release remained at revision 6/chart `0.3.4`, and the AIService remained Ready=True. Therefore the operator-owned upgrade/no-op acceptance path is **unverified on the cluster** and must be run with explicit approval or on a separate test cluster. No Jira Done claim should be made from this record alone.

## Cleanup and access

After the direct chart test, `helm uninstall aip5393-rollout --wait` completed. Redis DB 15 contained one fixture key and was flushed to zero; the dedicated `aip5393_rollout` PostgreSQL database was dropped. Both fixture connection Secrets were deleted. The preparatory LAS fields were removed from AIPlatform and AIService, and their original nonsecret fields and Ready state were verified. Temporary installer scripts, values, chart packages, build context, and generated TLS key were removed. The exact temporary SSH ingress rule `sgr-035ebaf0f4095e0fb` was revoked. The candidate image remains in ECR for a future authorized operator test; the existing operator namespace ECR pull Secret was refreshed with a short-lived token.
