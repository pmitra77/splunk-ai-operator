# AIP-5409 / AIP-5410 / AIP-5411 acceptance evidence

Captured 2026-10-22 from the installer at `52.12.115.104`. Commands used the
`ai-platform` namespace. Secret values and LAS request/response bodies were not
recorded.

## Deployment under test

- Operator-managed `AIService`:
  `ai-platform/agentruntime-dev-ai-platform-slim` (`Ready=True`, observed
  generation 2400).
- LAS Helm release: `las-agentruntime-dev-ai-p-eb88ba0f`, chart
  `langgraph-cloud-0.3.5-splunk.2`.
- LAS API and queue each had one ready replica before the checks. The HPA
  targeted CPU at 80%, with a minimum of 1 and maximum of 3 queue replicas.
- The deployed LAS image was
  `658391232643.dkr.ecr.us-west-2.amazonaws.com/ml-platform/las-sample-agent:las-poc-20260930-115209`.
  The registered graph was `echo` (assistant ID
  `046b5dc7-2108-582c-9c6c-31d556d138f0`). This is the sample graph, not an
  approved Launchpad product graph or the CPU-burn contract fixture.

## AIP-5409: LAS run, stream, and persistence

The health check returned HTTP 200. The sanitized runtime qualifier then ran
the `echo` graph through all three LAS REST modes. Each completed successfully,
and the unique marker was found under the graph state's `values` field:

| Mode | Result | Thread |
| --- | --- | --- |
| Synchronous | PASS | `01a161e3-5f48-75c3-bc4f-06222c14d8c4` |
| Background queue | PASS | `01a161e3-5fa2-7763-9b3b-8dc6f4eb4add` |
| SSE stream | PASS | `01a161e3-678f-7091-aa14-d2fbf4421223` |

The API Deployment was rolled and became ready at 1/1. All three markers were
then read successfully from graph state. The queue Deployment was rolled next
and became ready at 1/1; all three state reads passed again. This demonstrates
the persisted state survived both workload restarts.

**Acceptance gap:** these runs called the LAS REST API directly from its API
Pod. The deployed SLIM Pod returned HTTP 200 from `/health`, but it did not
expose `/openapi.json`, and its image did not have the `langchain_splunk`
caller package used by the caller-route acceptance harness. Therefore this
run is evidence for LAS protocols and restart persistence, but it does not
prove the intended SLIM/AITK caller route. AIP-5409 needs the caller-route
probe run against a deployment containing that caller integration before it
can be marked fully accepted.

## AIP-5410: product route qualification

The sanitized product-route harness is present at
`tools/cluster_setup/validation/aip5410_product_route_acceptance.py`. The
current deployed graph and image are the POC `echo` sample; no approved
Launchpad graph/image or representative product cases were available in this
deployment. The harness was therefore not run with product input, and no
product result is claimed.

The product-input blocker is recorded in the qualification guide: the product
owner must supply an immutable approved image and graph identity, trusted
representative request/resume cases and expected results, and the required
model/MCP/knowledge-base access. Until those are available, the acceptance
criterion for actual product results remains blocked. The probe is designed to
read cases from stdin and emit sanitized check names/results; credentials stay
in the trusted caller's Secret-backed inputs.

## AIP-5411: queue autoscaling and dependency capacity

### Static and live configuration evidence

- Existing Go tests cover typed scaling validation, CPU-only HPA configuration,
  omission of fixed queue replicas when HPA owns scaling, disabled scaling,
  missing metrics, API fixed at one replica, and preservation behavior in
  reconciliation/rotation fixtures.
- The live HPA reported min 1, max 3, CPU target 80%, current CPU 0%, current
  replicas 1, desired replicas 1. The API Deployment was 1/1 and the queue was
  1/1. This is a healthy idle baseline only; it does not demonstrate scaling.
- The queue's configured `N_JOBS_PER_WORKER` was 10.
- PostgreSQL reported `max_connections=100` and 11 current sessions.
- Redis reported `maxclients=10000` and 7 connected clients.

### Runtime acceptance still outstanding

The deployed graph is `echo`, not the CPU-burn fixture, so the bounded load
scenario cannot generate the intended queue CPU pressure. No claims are made
for scale-up, reaching the maximum, stabilized scale-down, or active-run
recovery across worker replacement. The namespace also had unrelated Pending
SLIM/Ray workloads and recent CPU scheduling warnings, so a load test would
not provide a controlled capacity result. Database and Redis limits were read,
but peak dependency connections under the maximum queue workload were not
measured. AIP-5411's runtime acceptance criteria therefore remain unproven.

## Local verification

- `python3 tools/cluster_setup/test_las_runtime_acceptance.py`: **12 passed**
  after the PR review regression cases were added.
- `go test ./pkg/ai/features/agentruntime`: **not run to completion**; the Go
  toolchain download/check was blocked because this machine could not resolve
  `proxy.golang.org` (`lookup proxy.golang.org: no such host`).

## Acceptance summary

| Story | Evidence recorded | Acceptance status |
| --- | --- | --- |
| AIP-5409 | Direct LAS sync, queued, SSE; graph-state persistence after API and queue restarts | Partial: caller-route probe remains outstanding |
| AIP-5410 | Harness and missing product-input prerequisites recorded | Blocked: approved product graph and cases unavailable |
| AIP-5411 | Static coverage and live HPA/PostgreSQL/Redis baseline recorded | Partial: controlled load, scale bounds/floor, and active-run recovery not demonstrated |
