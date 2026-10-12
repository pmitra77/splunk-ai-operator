# LAS rollout policy and capacity

The LAS API and queue Deployments use `RollingUpdate` with `maxSurge: 0` and `maxUnavailable: 1`. The operator supplies this fixed policy to the embedded chart on install and upgrade. An unchanged desired configuration does not create another Helm revision. Make an image or supported configuration change through AIPlatform/AIService and observe its AIService condition, Helm release and Deployment rollout; do not make a live Deployment patch part of the upgrade procedure.

The policy lets a one-replica API or queue go briefly unavailable while its old Pod terminates and a replacement becomes ready. The current chart gives each Pod a 30-second termination grace period. That interval allows normal termination but does not prove active runs finish. A queued job should remain claimable after replacement; an active job may complete, retry from a checkpoint, or fail according to the selected Agent Server runtime and graph behavior. Capture the run ID and its final state during qualification. Product actions that could repeat need their own idempotency policy. Zero downtime and exactly-once execution are not guaranteed.

Queue HPA scale-down can also terminate an active worker. The ERD requires a 300-second scale-down stabilization window and removal of at most one worker per 60 seconds; those HPA controls do not drain an active run. Their chart implementation and scale-down tests are tracked separately in AIP-5394/AIP-5406/AIP-5411.

## Capacity floor

For one API and a maximum queue pool of `M` workers, reserve at least the following LAS container requests at steady state:

| Resource | Formula | ERD defaults with M = 3 |
| --- | --- | --- |
| CPU | API request + M × queue request | 250m + 3 × 1 CPU = 3.25 CPU |
| Memory | API request + M × queue request | 2 GiB + 3 × 2 GiB = 8 GiB |

The default container limits total 8 CPU and 16 GiB at that size. These LAS figures are only part of the required cluster capacity: also include the operator and full AI Tier profile, Kubernetes/system reservations, sidecars and startup needs. Check that each workload can fit on a node under its node selectors, affinities and taints. PostgreSQL connection limits, Redis capacity and license entitlement must support the API plus the maximum worker pool.

Zero surge avoids requesting an extra *available* replica for a rollout, but a terminating Pod may still hold CPU and memory while its replacement is created. Measure that overlap on the qualified cluster instead of assuming maxSurge alone guarantees immediate scheduling.

## Upgrade and acceptance observations

1. Record the pre-upgrade chart version/digest, Helm revision, API/queue Deployment strategies, Pod UIDs, node allocation, AIService Ready condition and Service endpoint.
2. Apply the supported image/configuration change and wait for the operator-owned Helm upgrade. Confirm the rendered Deployments retain `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` and the API stays at one replica.
3. Confirm the old Pod terminates, a replacement becomes ready, the stable Service resolves to the API, and the Helm revision advances once. Reconcile again with no change and confirm there is no further revision or Pod rollout.
4. Exercise queued and active work across replacement and record each run's terminal state and any retries. Repeat for HPA scale-down once the HPA work is integrated. A healthy Deployment alone does not establish run recovery.
5. Record scheduling events and usable CPU/memory headroom. If the five-minute Helm wait is too short for the observed startup/termination behavior, qualify a bounded timeout change before accepting the rollout.

The chart package and rendered strategy can be checked without a running LAS license. Application readiness and interruption behavior require a valid license, reachable PostgreSQL/Redis, and a representative graph image.
