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
| `collect.state.redactSecrets` | `collect.state.redact_secrets` | Whether Secret `data`/`stringData` are stripped before leaving the cluster; default `false` |
| `collect.metrics.*` | `collect.metrics.*` | K8s metrics + custom endpoints |
| `query.*` | `query.*` | Live, on-demand resource reads; omit to inherit `collect.state` |
| `query.redactSecrets` | `query.redact_secrets` | Strips Secret `data`/`stringData` from live query responses; unset inherits `collect.state.redactSecrets` |
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
  --set collect.logs.rules[0].label_selector=log=backend-log \
  --set-json 'collect.logs.rules[0].pod_names=["be-*"]' \
  --set-json 'collect.logs.rules[0].containers=["backend-admin"]'
```

Keys inside a `rules` entry are snake_case (`pod_names`, `label_selector`),
unlike the camelCase chart values around them: the chart copies these lists
into the agent's config file verbatim, so the keys are the agent's own config
keys. `values.schema.json` names every key these lists accept, so the same
command written with `podNames` is refused, and the message names the key
(helm 3.18 replaced the validator, so older helm 3 words it differently),
instead of installing a rule that
silently lost its filter -- which would collect more than intended, not less.
The check covers the camelCase keys around the lists as well (`--set
collect.logs.tail_lines=50` is refused), and the durations inside them:
`pod_interval: 30` -- or `"30"` -- is a config the agent rejects at startup, so
helm refuses it rather than rendering a CrashLoopBackOff. Leaving a rule's field blank stays
legal -- yaml.v3 binds a blank to the zero value and skips a blank list entry
entirely, so the agent loads it as a filter that was never set.

Charts older than 0.7.5 describe only `query.rules`, so everything under
`collect.*` installs unchecked there. Agent images newer than 0.7.3 log
`unrecognized config key ignored` at startup as a second line of defence.

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

### Secret handling

`collect.state.redact_secrets` (chart: `collect.state.redactSecrets`) controls whether watched
Secret objects have their `data`/`stringData` payloads stripped before the agent sends state
events to the gateway. **Default: `false` — Secret values are NOT stripped.** This is a
deliberate choice: with stripping off, Secret values leave the cluster over the agent's
existing gRPC stream and are persisted by the Kubexa platform, which is the trade-off required
to let cluster admins and owners view Secret values in the resource explorer. Set
`redactSecrets: true` for an installation that must keep Secret values inside the cluster and
never send them to the gateway.

Regardless of this setting, `managedFields` and the
`kubectl.kubernetes.io/last-applied-configuration` annotation are always stripped from every
object, Secret or not — for a Secret applied with `kubectl apply`, that annotation is a second,
independent copy of the full manifest including every base64-encoded value.

### Live resource query

`query.*` configures live, on-demand resource reads: a request from the Kubexa platform for the
current state of a specific object or list, answered synchronously, as opposed to
`collect.state`'s continuous watch-and-push feed. Omit the section to inherit `collect.state`
entirely; set individual fields (e.g. only `redactSecrets`) to override just those and inherit
the rest — see the commented block in `values.yaml` for the per-field inheritance rules.

The chart ships with `query.enabled: true`, `query.rules: []` (which inherits
`collect.state.rules`'s cluster-wide `cluster-core` rule, covering `secrets` with no `verbs`
restriction), and `redactSecrets: false` — so a plain `helm upgrade` permits live reads of
Secret values through this path. This is not a new exposure: the same `cluster-core` rule
already streams those Secret values to the platform continuously via `collect.state` (see
[Secret handling](#secret-handling)); live query just adds a second, on-demand path to data the
platform already receives.

This policy is a **second gate stacked on top of Kubernetes RBAC**, not a replacement for it.
Both must allow a read before the agent returns data: RBAC answers "may this ServiceAccount read
this object", and `query` answers "did the cluster owner agree the Kubexa platform may read it".
The two are reported separately in the capability catalog so the UI can tell a viewer which of
the two is actually blocking a given resource.

`enabled: false` refuses every live query with `POLICY_DENIED` — an explicit, diagnosable
refusal, not silence. A gateway or UI waiting on a query gets an answer either way.

`verbs` controls which *operations* a rule permits, not how much of an object comes back.
Restricting a rule to `[list]` on `secrets` prevents fetching an individual Secret by name via
`get`; it does **not** redact anything — a full-view `list` still returns every matched Secret's
`data`/`stringData`, exactly as `kubectl get secrets -o json` does against the Kubernetes API
itself. The knob that actually keeps Secret **values** inside the cluster is `redactSecrets:
true` (`query.redactSecrets`, which when unset inherits `collect.state.redactSecrets` — see
[Secret handling](#secret-handling)). To expose Secret **names** without values, either use the
TABLE view (`QUERY_VIEW_TABLE`), which returns printed columns and `PartialObjectMetadata` and
never includes `data`/`stringData`, or set `redactSecrets: true`.

The TABLE view is not a pass-through of the API server's response. Every row's object goes
through the same sanitization as the full view before the payload leaves the agent, which
matters more than it sounds: `PartialObjectMetadata` copies annotations verbatim, and for a
Secret written with `kubectl apply` the `kubectl.kubernetes.io/last-applied-configuration`
annotation is a second complete copy of the manifest — base64 values included. That annotation
and `managedFields` are stripped unconditionally, independent of `redactSecrets`. Rows are
also filtered against the rule's `names` patterns, so a TABLE listing shows exactly the objects
the equivalent full-view `list` shows. Printed **cells** are left as the API server rendered
them; a CRD's `additionalPrinterColumns` can aim a cell at any field its author chose, so cells
reflect definitions that already exist in the cluster and show what `kubectl get` shows.
Kubernetes' built-in Secret columns are NAME/TYPE/DATA/AGE, where DATA is a key count.

The per-GVR verdict published in the capability catalog is deliberately coarser than the real
enforcement: a policy scoped to a namespace or a name prefix cannot be reduced to a single
boolean for the whole GVR, so that verdict is only a hint for the UI. The full rule evaluation
still runs against every individual request, regardless of what the catalog reported.

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
