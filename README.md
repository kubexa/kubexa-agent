# kubexa-agent

Secure agent that connects Kubernetes clusters to the Kubexa platform for monitoring, control, and real-time operations.

## Features

- Collects Kubernetes pod logs (with checkpointing and namespace rules).
- Watches cluster state (pods, deployments, ingresses, and more).
- Scrapes Kubernetes Metrics API and custom Prometheus endpoints.
- Streams data to the Kubexa gateway over a resilient gRPC connection.
- Deployable with the included `helm/kubexa-agent` chart.

## Helm install

### Local chart

```bash
helm install kubexa-agent ./helm/kubexa-agent \
  --namespace kubexa \
  --create-namespace \
  --set secret.tenantToken="<tenant-token>" \
  --set gateway.host=gateway.example.com \
  --set gateway.port=443 \
  --set gateway.tls=true
```

### OCI registry (GHCR)

Published by `.github/workflows/helm-oci-release.yaml` on push to `main`:

```bash
helm install kubexa-agent oci://ghcr.io/kubexa/charts/kubexa-agent \
  --version 0.2.0 \
  --namespace kubexa \
  --create-namespace \
  --set secret.tenantToken="<tenant-token>" \
  --set gateway.host=gateway.example.com
```

### Classic Helm repo

Published by `.github/workflows/helm-release.yaml` to the `kubexa/helm-charts` GitHub Pages repo:

```bash
helm repo add kubexa https://kubexa.github.io/helm-charts
helm repo update
helm install kubexa-agent kubexa/kubexa-agent \
  --version 0.2.0 \
  --namespace kubexa \
  --create-namespace \
  --set secret.tenantToken="<tenant-token>" \
  --set gateway.host=gateway.example.com
```

## Singleton deployment

The agent runs **one replica per cluster**. Each instance has its own `agent_id`, gRPC session, and collectors; scaling beyond 1 duplicates logs, state events, and metrics at the gateway (leader election is not implemented yet).

The Helm chart fixes `replicas: 1` and uses `strategy: Recreate` so upgrades do not briefly run two collectors side by side. Do not scale the Deployment manually or via HPA until HA is supported.

## Configuration

Chart values map directly to `pkg/config.Config`. Key sections:

| Values path | Config field | Description |
|-------------|--------------|-------------|
| `secret.tenantToken` | `agent.tenant_token` | Injected via `KUBEXA_TENANT_TOKEN` |
| `gateway.*` | `gateway.*` | Gateway address, TLS, reconnect |
| `collect.logs.*` | `collect.logs.*` | Log tail/follow, rules |
| `collect.state.*` | `collect.state.*` | Resource watch rules |
| `collect.metrics.*` | `collect.metrics.*` | K8s metrics + custom endpoints |
| `buffer.*` | `buffer.*` | Memory/disk queue |
| `observability.*` | `observability.*` | Health and metrics ports |
| `log.*` | `log.*` | Agent logger level/format |

### Example: namespace-scoped log collection

```bash
helm upgrade kubexa-agent ./helm/kubexa-agent \
  --namespace kubexa \
  --reuse-values \
  --set collect.logs.rules[0].id=stage-api \
  --set collect.logs.rules[0].namespace=stage \
  --set collect.logs.rules[0].labelSelector=log=backend-log \
  --set-json 'collect.logs.rules[0].podNames=["be-*"]' \
  --set-json 'collect.logs.rules[0].containers=["backend-admin"]'
```

Or pass a custom `values.yaml` with full rule definitions.

### Existing secret

```bash
kubectl create secret generic kubexa-agent-token \
  --namespace kubexa \
  --from-literal=tenant-token=<token>

helm install kubexa-agent ./helm/kubexa-agent \
  --namespace kubexa \
  --set secret.create=false \
  --set secret.existingSecret=kubexa-agent-token
```

## Memory and resource sizing

The Helm chart defaults to **256 MiB request / 512 MiB limit** (`helm/kubexa-agent/values.yaml`). Actual RSS depends on cluster size, collector settings, and whether the gateway keeps up with export. Figures below are **order-of-magnitude estimates** for planning—not hard guarantees.

### Where memory goes

| Component | Default cap | Notes |
|-----------|-------------|-------|
| Buffer queue (RAM) | 64 MiB (`buffer.max_memory_bytes`) | Hard cap; overflow spills to disk |
| Buffer queue (disk) | 512 MiB (`buffer.max_disk_bytes`) | Requires PVC when persistence is enabled |
| Log streams | Up to 200 concurrent (`MaxConcurrentStreams`) | Per-stream read buffers; each log line is a separate queue item |
| State informer cache | Uncapped | Scales with watched objects (pods, secrets, etc.) |
| Go runtime + client-go + gRPC | ~60–80 MiB baseline | Always present |

### Scenarios (estimated RSS)

| Scenario | Estimated memory |
|----------|------------------|
| Agent + stream only (all collectors disabled) | 60–100 MiB |
| Default Helm install, small cluster (~50 pods), gateway connected | 150–200 MiB |
| Medium cluster (~500 pods), all collectors enabled | 200–280 MiB |
| Gateway disconnected + heavy log volume | 250–400 MiB |
| Large cluster (5000+ pods), broad state watch + many log streams | 400–600+ MiB |

### What drives spikes

- **Log collector** — highest variable cost when enabled. High line rates fill the 64 MiB RAM buffer quickly; if the gateway is slow or offline, streams keep producing and spill to disk. Narrow `collect.logs.rules` and `exclude_namespaces` in busy clusters.
- **State watcher** — client-go informer caches hold full object copies in memory. Watching `secrets` across many namespaces is expensive. Prefer namespace-scoped rules and only the resources you need.
- **Metrics scraper** — usually low (1–10 MiB transient spikes). Large custom Prometheus `/metrics` pages can add short-lived pressure.

### Tuning

```yaml
# Reduce in-memory buffering (chart values → config.yaml)
buffer:
  maxMemoryBytes: 33554432   # 32 MiB — lower RAM, earlier disk spill
  maxDiskBytes: 1073741824   # 1 GiB — more headroom when gateway is down

# Raise pod limit for large clusters
resources:
  limits:
    memory: 1Gi
```

Disable collectors you do not need (`collect.logs.enabled`, `collect.state.enabled`, `collect.metrics.enabled`).

### Observability

Expose agent self-metrics on `observability.metrics_addr` (default `:9090`). Useful series for capacity planning:

- Queue depth and memory usage (buffer pressure)
- Active log streams and dropped lines (log backpressure)
- Informer cache sync / state event rates (watch scope too broad?)
- Gateway connection state (disconnected → buffer fills)

## Development

```bash
make run-dev          # agent with example-local.yaml
make run-dev-grpc     # local demo gateway
make helm-lint        # lint chart
make helm-template    # render manifests
make helm-package     # package to dist/
```

## CI releases

| Workflow | Trigger | Output |
|----------|---------|--------|
| `helm-oci-release.yaml` | `helm/**` on `main` | `oci://ghcr.io/<org>/charts/kubexa-agent` |
| `helm-release.yaml` | `helm/**` on `main` | `https://kubexa.github.io/helm-charts` |
| `build.yaml` | push/PR | Docker image `ghcr.io/<org>/kubexa-agent` |
