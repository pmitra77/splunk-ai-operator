# AIP-5401 / AIP-5403 cluster retest — 2026-10-16

**Status:** The updated SLIM image fixed CPU-only startup and the assistant/thread UUID contract. The dummy `echo` graph has a different input and response shape from the SLIM product request, so this run does not qualify the Launchpad final response and is not evidence of a story failure. That check is deferred to the Launchpad image or a contract-compatible fixture. The shared test deployment and temporary access were cleaned up. This record follows the [October 15 validation](aip-5401-5403-cluster-validation-20261015.md).

## Scope and deployment

- Test cluster: agentruntime-dev, namespace ai-platform, AWS account 658391232643 in us-west-2. STS resolved to the expected splunkcloud_account_admin role.
- Supplied image: 658391232643.dkr.ecr.us-west-2.amazonaws.com/ml-platform/slim-api:feature-las-202610152102-b7cb6e97e381. ECR resolved digest sha256:951ac687c0753399e9a636cd507ec541f55ecaade4717c19276d228115c003ef (3,201,102,724 bytes).
- The isolated candidate used the existing stable LAS Service and graph ID echo. It had ENABLE_AUTHN=true, ENABLE_AUTHZ=true, and the in-cluster Splunk issuer. No PLATFORM_URL was present. A fresh test-only ECR pull Secret was streamed into Kubernetes without printing or saving its credential.
- Kubernetes server-side dry-run passed. The candidate pulled the supplied digest in about 2m5s, became Ready 1/1 with zero restarts, and returned HTTP 200 from /health. Its Pod imageID matched the ECR digest. The primary SLIM, LAS, and sole operator were not replaced.

## Executed acceptance checks

| Check | Observed result | Interpretation |
| --- | --- | --- |
| Missing bearer on SLIM create and invoke | HTTP 401 on both; Missing Bearer token | AITK-to-SLIM denial works for these requests. |
| Malformed bearer on SLIM create | HTTP 401; Malformed JWT | Invalid identity is denied. |
| Splunk-issued interactive JWT | Mint HTTP 200; current-context HTTP 200 with one entry | The tested Splunk admin identity was accepted. This does not prove an actual AITK principal or least-privilege grant. |
| Authenticated SLIM create | HTTP 201; runtime_id was a UUID, for example f2d52ce5-cd98-4eed-a793-be9e35f21b3f | The new image creates a LAS-compatible assistant. The prior assistant-ID blocker is fixed. |
| Authenticated SLIM invoke on a fresh session | HTTP 200 NDJSON: Thinking, then Completed with success=false and http_error_code=500 | LAS accepted the generated UUID thread ID and started the echo graph. The dummy graph failed with KeyError('request'): SLIM sent messages/system_prompt, while this fixture requires request. This is a fixture contract mismatch, not a test of the Launchpad final response. The HTTP status did not reflect the terminal run failure. |
| Fresh failed thread state | HTTP 200; empty state values | No expected fixture result was persisted from the failed run. |
| Diagnostic: direct LAS prime, then SLIM invoke on the same synthetic thread | Direct LAS prime returned the expected response; SLIM returned Completed success=true but response was an empty string | This establishes that the post-UUID SLIM run can complete when the echo graph's request field is preseeded. It is not an unassisted product-path success. LAS thread state retained the echo graph's top-level response; SLIM's final envelope used a different response shape. This does not establish a defect for the Launchpad graph. |
| Per-run context diagnostic | A unique llm model marker appeared in the LAS run list for the synthetic thread and was absent from persisted thread state values, which contained only request and response | The marker reached the run context without moving into this graph's application state. Broader context and resume behavior remain untested. |
| Synthetic cleanup | SLIM assistant DELETE returned HTTP 200; a later direct thread DELETE returned 404 because the assistant delete removed its threads | Disposable LAS objects were removed. |

The developer clarified that the intended agent path needs only a final response; incremental LAS events are not required. The dummy `echo` graph does not implement that product request/response contract, so its final response is not an acceptance check in this development phase. Authentication is applied on AITK to SLIM. The deployed adapter creates an Agent Server client with no API key for SLIM to LAS over the internal Service, matching the stated boundary. The candidate Service was internal ClusterIP over HTTP. This test did not establish network isolation of LAS, TLS/mTLS, an actual product caller grant, or external transport behavior.

## Acceptance disposition

- **AIP-5401 selected graph and fixture result:** The stable LAS route, UUID assistant creation, and UUID thread run reached the selected dummy graph. The graph's `request` input and top-level `response` differ from SLIM's `messages`/`system_prompt` product request and final-envelope shape. The `KeyError` and empty echo response are fixture diagnostics, not grounds to fail or block these stories. Jira's returned-and-persisted fixture-result criterion remains unverified and is deferred to the Launchpad image or a contract-compatible fixture.
- **AIP-5401 context and errors:** A distinctive context value reached a run and was absent from thread state values. The observed failure returned HTTP 200 with a terminal error envelope rather than an HTTP error status. Resume/HITL and broader context mapping are unverified.
- **AIP-5401 cpuOnly:** The isolated supplied image started without PLATFORM_URL, resolving the prior image startup blocker. The operator branch now omits PLATFORM_URL when AIPlatformUrl is empty; focused local tests passed. Operator-owned reconciliation was not deployed to this shared cluster.
- **AIP-5403 identity and access:** Missing/malformed credentials were denied and a Splunk admin token was accepted by SLIM. The actual AITK identity and an explicit least-privilege product grant were not exercised. SLIM to Agent Server uses the internal unauthenticated application route by design.
- **AIP-5403 ingress/transport/egress:** The candidate had only an internal ClusterIP HTTP Service. No namespace NetworkPolicy or TLS/mTLS was applied or tested; the pre-existing primary SLIM NodePort and other shared resources were left unchanged. Unsupported external and network-restricted options remain unqualified.

## Local changes and cleanup

The checked-in k0s profile and isolated fixture now pin the new image by digest. The fixture omits PLATFORM_URL. The reconciler only sets PLATFORM_URL when a model URL exists, and its focused deployment test asserts absence in cpuOnly. The synthetic adapter probe now uses the returned runtime_id. Focused Go tests, YAML parsing, and Git diff checks passed locally.

The isolated Deployment, Service, both ConfigMaps, and test-only pull Secret were deleted; a label query and direct Secret lookup returned no resources. The primary LAS and SLIM AIServices remained Ready=True with their original UIDs (39598b65-89e6-4b32-ba5b-547dc9902036 and 0acaaa07-59be-4428-8ba8-5fe7fa0392b8). Primary LAS API, LAS queue, and original SLIM Deployments remained 1/1 with their original images. The temporary loopback port-forwards ended with the probes, the private tunnel was stopped, and the exact temporary installer security-group rule sgr-0bb3a4d975e251b7b was revoked; readback returned NotFound. The short-lived Splunk interactive JWTs were held in memory only and will expire automatically. No bearer, password, private key, kubeconfig, or application credential bytes were printed or committed.

## Next verification

When the Launchpad image or a contract-compatible fixture is available, repeat authenticated create/invoke and verify AIP-5401's returned and persisted result. The dummy `echo` graph's final response is not a gate for this development phase or for AIP-5403, which has no final-response criterion. Separately qualify the actual AITK principal and grant, relevant error mapping and resume behavior, and operator-owned rollout in an isolated operator environment against their applicable criteria.
