# LAS contract fixture

This is a deterministic Agent Server graph for integration checks while the
Launchpad product image is developed. It makes no model, MCP, or knowledge-base
connections. The graph ID and default assistant ID are `contract_fixture`.

The graph accepts both its original `{"request":"hello"}` input and the SLIM
caller shape with a `messages` list. It returns a deterministic assistant
message and reports which of `llm_config`, `mcp_configs`, and `kb_configs`
arrived in per-run context. It exposes only a model identifier and MCP/KB names
as safe context summaries; it never returns credential values or copies them
into graph state. A message containing `AIP5402_INTERRUPT` triggers one
interrupt so the SLIM route can be exercised again with a fresh resume context.

For isolated queue-HPA qualification, set `cpu_seconds` to an integer from 1
through 15 in the graph input. The fixture burns CPU inside the Agent Server
worker for that bounded duration and persists the request marker in graph
state. Omit the field for normal deterministic runs. Booleans, non-numeric
values, and values outside 0 through 15 are rejected.

The in-Pod caller-route acceptance probe is
[`aip5402_slim_route_acceptance.py`](../../tools/cluster_setup/validation/aip5402_slim_route_acceptance.py).
Run it only against an isolated SLIM candidate configured with
`LAS_GRAPH_ID=contract_fixture` and the stable LAS Service URL. It creates a
temporary assistant and removes it (and its threads) in a `finally` block. It
uses fake credentials and no model, MCP, or knowledge-base endpoints. The probe
fails if fake credential values appear in caller output, captured SLIM logs,
or any returned thread state, including checkpoint metadata.

Copy the probe into the isolated candidate Pod and run it there:

```sh
kubectl -n ai-platform cp tools/cluster_setup/validation/aip5402_slim_route_acceptance.py \
  <isolated-slim-pod>:/tmp/aip5402_slim_route_acceptance.py
kubectl -n ai-platform exec <isolated-slim-pod> -- \
  python /tmp/aip5402_slim_route_acceptance.py
```

Build with `langgraph-cli[inmem]==0.4.32` on Linux AMD64 using a candidate
tag. AIP-5388 validates this candidate; product-image approval and immutable
identity belong to AIP-5567 when the Launchpad image is ready. The LAS state
API persisted fake per-run credential values in checkpoint metadata during
candidate testing, so the fixture is not qualified for real credential-bearing
context until the caller-route probe passes. Runtime license, PostgreSQL, and Redis
credentials belong in Kubernetes Secrets and must never be included in the
image build context.
