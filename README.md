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
