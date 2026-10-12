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

The fixture upgrade used Helm; no live LAS Deployment patch was issued. It demonstrates the packaged chart and constrained one-replica rollout independently of the operator. The operator-owned test below establishes the live `LASReconciler` upgrade/no-op path. Active-run interruption, retry/idempotency, and HPA scale-down are separate qualification work; the fixture smoke run began after the upgrade.

## Operator-owned primary release upgrade

A Linux/amd64 manager was built from story commit `4c0a5e206871a00019857a66a2e4f5772a794cc1` (binary SHA256 `2efbed0000ae3b9c5821ae320b38db5a3e97e81de2335db13b0d294c999eb3c5`) and packaged with the current configuration and distroless runtime. The unique test image was pushed to the existing ECR repository as `aip5393-rollout-20261012-21b8be4`, immutable digest `sha256:7092502ae18ec1d5fd5ebcfc1ad7194efdf0dba0ad4c3abcbe7949b8ca895b9c`; the tag suffix references the branch's base commit, not the candidate source commit. The live CRD supports `las.image` and `assistantId`. The existing sample image/tag and assistant `echo` were staged in both AIPlatform and AIService, with the graph image unchanged. The user explicitly approved replacing the sole `agentruntime-dev` operator manager, allowing it to reconcile the existing primary LAS release, and leaving it running after validation. The candidate manager was deployed by immutable digest and rolled out 1/1 Ready.

Before deployment, the primary release was revision 6/chart `0.3.4`, its API and queue were 1/1 Ready with default 25% surge / 25% unavailable, and the AIService was Ready=True. The operator then reconciled the same primary AIService UID `39598b65-89e6-4b32-ba5b-547dc9902036` and emitted `LASUpgrading` at 12:41:38 UTC and `LASUpgraded` at 12:41:42 UTC. The result was revision 7/chart `0.3.5-splunk.1`, status `deployed`. Both Deployment generations advanced from 6 to 7 and both had `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` without a direct Deployment patch. Old API Pod UID `af469a85-04d6-4ac3-a09d-a849cd4b790b` and queue UID `894fc0a9-1355-4207-b900-165000f59c64` were replaced by API UID `a51bd07f-c0a7-452d-924f-e3377f84e31f` and queue UID `6dfcbf2f-594a-472c-8b85-3818aa95c0d5`. The AIService briefly reported `WorkloadsNotReady` while replacement Pods started, then returned to Ready=True/`LASReady` at 12:41:53 UTC with both workloads 1/1 Ready. This observed interval is consistent with the intentional availability tradeoff; it does not establish zero downtime.

The repository's `scripts/las-baseline-smoke.sh` ran through a temporary local tunnel to the AIService compatibility Service. It reported `PASS` for `/ok`, thread creation, `echo` `runs/wait`, and persisted state using the deployed manager digest, chart version/archive digest, and Agent Server image digest `sha256:bc4c1a779904e0c422392deabcf4091f79c1fcdf82bc483d49b82ba79a5b4eba`. Its synthetic thread was `01a13023-d697-7002-a663-21aaca4cb910` and marker was `las-baseline-smoke-49b36adf-a655-40a9-8dad-00c6c670a7f8`; the thread remains as the smoke script's persistence record. The tunnel was stopped afterward.

A metadata-only annotation triggered another AIService reconciliation after the successful upgrade. More than 30 seconds later, the Helm release remained revision 7 and both replacement Pod UIDs were unchanged and Ready. The annotation was removed; a final read still showed revision 7, the same Ready Pods, and the candidate manager 1/1 Ready. This is the live operator no-op check; the local test also checks equality of desired Helm values on a second evaluation. The candidate manager and typed LAS image/assistant fields are intentionally left in place on `agentruntime-dev` under the user's approval. No queued or active run was submitted across the operator-driven replacement, so this record does not qualify interruption recovery required by the broader ERD. No Jira Done claim should be made before the story PR is reviewed and merged.

## Cleanup and access

After the direct chart test, `helm uninstall aip5393-rollout --wait` completed. Redis DB 15 contained one fixture key and was flushed to zero; the dedicated `aip5393_rollout` PostgreSQL database was dropped. Both fixture connection Secrets were deleted. Temporary installer scripts, values, chart packages, build context, and generated TLS key were removed. The earlier temporary SSH ingress rule `sgr-035ebaf0f4095e0fb` was revoked. The current IP already had an existing SSH rule, so the operator-owned test added no ingress rule. The temporary smoke port-forward and no-op annotation were removed. The operator namespace ECR pull Secret had been refreshed with a short-lived token. The candidate manager and the upgraded primary LAS release are left running for review and subsequent testing.
