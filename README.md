# kubexa-agent

**The Kubernetes agent of the Kubexa platform — and the part of it that runs inside *your* cluster, which is why it is open source.**

`kubexa-agent` is a single Go binary, deployed as one replica in the cluster it
observes. It collects logs, cluster state and metrics, answers live read
requests, and — only when the cluster owner explicitly turns each one on —
performs writes, opens consoles and runs node jobs. Everything it does travels
over **one outbound gRPC stream** to the Kubexa gateway; it opens no inbound
port, needs no ingress, and no component of the platform ever dials into the
cluster.

[![License: GPL v3](https://img.shields.io/badge/license-GPL--3.0-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)
![Platforms](https://img.shields.io/badge/linux-amd64%20%7C%20arm64-lightgrey.svg)

- Chart: `oci://ghcr.io/kubexa/charts/kubexa-agent` · Image: `ghcr.io/kubexa/kubexa-agent`
- License: **GPL-3.0** · Go **1.26** · Runtime image: distroless, non-root, `linux/amd64` + `linux/arm64`

## Contents

- [How it works](#how-it-works)
- [What the agent does](#what-the-agent-does)
- [Why it is open source](#why-it-is-open-source)
- [Trust boundaries](#trust-boundaries)
- [Helm install](#helm-install)
- [Singleton deployment](#singleton-deployment)
- [Configuration](#configuration)
- [Memory and resource sizing](#memory-and-resource-sizing)
- [Repository layout](#repository-layout)
- [Development](#development)
- [CI releases](#ci-releases)
- [Compatibility](#compatibility)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## How it works

One process, one replica. Three collectors read from the cluster — the **log
tailer** (container logs, with its read position checkpointed in a local SQLite
store), the **state watcher** (client-go informers over the resources the owner's
rules name) and the **metrics scraper** (Metrics API, cAdvisor,
kube-state-metrics, custom Prometheus endpoints). Everything they produce goes
into one **buffer**: an in-memory queue that spills to disk segments when it
fills or when the gateway is unreachable. A single stream manager drains that
buffer over **one outbound gRPC stream** to the Kubexa gateway, which forwards to
the platform and its UI — and carries the platform's requests (live reads,
mutations, consoles, node jobs, config updates) back down the same connection.
Nothing dials into the cluster; the agent always calls out.

```
  your Kubernetes cluster
  ───────────────────────────────────────────────────────────────────
    kube-apiserver         kubelet / cAdvisor     kube-state-metrics
          ▲                        ▲                      ▲
          │ watch · get/list       │ scrape               │ scrape
          │ exec · eviction        │                      │
          └────────────┬───────────┴──────────────────────┘
                       │
             ┌─────────┴──────────┐
             │    kubexa-agent    │  1 replica · non-root · distroless
             │     (this repo)    │  no inbound port
             └─────────┬──────────┘
                       │  in-memory queue → disk spill
                       │  (survives restarts and gateway outages)
  ─────────────────────┼─────────────────────────────────────────────
                       │  outbound gRPC over TLS, agent-initiated
                       ▼
               Kubexa gateway ──► platform · UI
```

Two streams, both opened by the agent:

| Stream | Direction | Carries |
|--------|-----------|---------|
| `Connect` | bidirectional, long-lived | **up:** logs, state events, metrics, heartbeat/health, capability catalog, query / mutation / node-job results · **down:** handshake, acks, config updates, backpressure, shutdown, resource queries, mutation requests, console opens, node jobs |
| `ExecSession` | one per console session | the session's bytes only — a slow or oversized console frame must not be able to stall telemetry |

Three properties fall out of that shape, and they are the ones to design
around:

- **Outbound only.** No exposed port, no ingress, no inbound firewall rule; the
  agent's identity is a tenant token it presents at handshake.
- **Nothing is lost to a bad afternoon.** The disk queue survives the pod, and
  log checkpoints survive it too — a restart neither re-sends nor skips lines.
  A gateway outage fills the buffer instead of dropping telemetry.
- **Versions are negotiated, not assumed.** The proto version is settled at
  handshake ([`pkg/protoversion`](pkg/protoversion/)), so an agent and a gateway
  of different ages produce an explicit "upgrade the agent" answer rather than a
  timeout.

## What the agent does

The agent has four jobs. **Collect**, **answer** and **report** are on in the
chart's defaults; everything under **act** is off until it is turned on in two
independent places (see [Trust boundaries](#trust-boundaries)).

**1. Collect — continuous, push-only.** *(on by default)*

- **Logs** — tails container logs by namespace / label / pod-name / container
  rules, joins wrapped lines, caps line size, and checkpoints its read position
  in a local SQLite store so a restart neither re-sends nor skips lines.
- **State** — client-go informers watch the resources the owner's rules name
  (pods, deployments, services, ingresses, CRDs…) and push add/update/delete
  events. `managedFields` and the `last-applied-configuration` annotation are
  stripped from every object, unconditionally.
- **Metrics** — the Kubernetes Metrics API, each node's cAdvisor endpoint,
  kube-state-metrics, and any custom Prometheus `/metrics` URL (the Metrics API
  is on by default; the other three are opt-in per endpoint). Scrape health is
  reported per target kind, and "we never looked" is reported as a different
  thing from "it returned nothing".

**2. Answer — live, on-demand reads.** *(on by default, inheriting the state
rules)* A request for the current state of one
object or a list, served synchronously from the cluster's API server rather than
from whatever the platform last stored. Full-object or table view, with the same
sanitization on both paths.

**3. Act — explicit, gated, auditable.** *(every item off by default)*

- **Mutations**: `patch`, `delete`, `restart`, `scale`, `create`.
- **Pod console**: an interactive shell in a running container, resizable and
  resumable across a proxy cut.
- **Node console**: a root shell on the node itself, via a privileged helper Pod
  the agent creates and enters with `nsenter`.
- **Node jobs**: `cordon`, `uncordon`, `drain` — drain follows `kubectl drain`
  semantics (DaemonSet/mirror pods skipped, PodDisruptionBudgets honoured) and
  never evicts the agent's own pod.

**4. Report — so the UI never has to guess.** On every handshake the agent
publishes a capability catalog: every API resource in the cluster, and whether
*RBAC* or the *owner's policy* is what blocks reading it. On every heartbeat it
reports health, queue pressure, scrape health, both Secret-redaction flags, which
platform-managed config revision it actually applied, and whether a restart is
still pending.

## Why it is open source

This agent is the one Kubexa component that runs on infrastructure we do not
own, holding credentials to a cluster we cannot see. Asking an operator to take
that on faith is the wrong request, so we don't:

- **Verifiable trust, not asserted trust.** The agent's privileges are as wide as
  the cluster owner allows, and in the extreme (`rbac.readAll` with
  `redactSecrets: false`) they include reading every Secret in the cluster. What
  it reads, what it redacts, and what crosses the network is readable here, in
  the source, by the person who has to sign off on it.
- **Security claims you can check.** Every powerful feature is designed to need
  two independent yeses — Kubernetes RBAC *and* the owner's config — and no RBAC
  grant in the chart is derived from the agent's own config, deliberately. Those
  are claims about code, and claims about code belong in public code.
- **The dangerous parts deserve the most eyes.** A node console is root on the
  node. A drain evicts every pod on it. Those paths ship off, and they ship
  readable.
- **Reproducible supply chain.** Build the binary and the image from this tree,
  push it to your own registry, and run that. Nothing in the chart requires our
  published image.
- **No lock-in on the data path.** `gateway.host` points wherever you point it,
  including at a self-hosted Kubexa installation. The wire contract is in
  [`proto/`](proto/), not hidden behind a client library.
- **Portability we cannot test alone.** arm64 node pools, managed distributions,
  odd RBAC setups and CRD-heavy clusters show up in other people's clusters
  first. Issues and patches from those clusters make the agent correct for them.
- **GPL-3.0 keeps it that way.** Improvements to the component that holds the
  cluster's keys stay inspectable by the people whose clusters it runs in.

## Trust boundaries

Nothing powerful is reachable through a single switch. Each row needs every gate
in it, and the chart's RBAC flags are **never** derived from the agent's config —
the config can be mounted from a file the chart never sees, and a derived Role
would then be narrow while the policy said yes.

| What | Kubernetes RBAC (chart) | Owner policy (agent config) | Platform permission | Default |
|------|-------------------------|-----------------------------|---------------------|---------|
| Read cluster state, live queries | generated read role; `rbac.readAll` for everything incl. CRDs | `collect.state.rules`, `query.rules` | — | narrow read on |
| Secret **values** leaving the cluster | part of the read role | `collect.state.redactSecrets`, `query.redactSecrets` | — | **not stripped** — see [Secret handling](#secret-handling) |
| Writes (`patch`/`delete`/`restart`/`scale`/`create`) | `rbac.write` | `mutate.enabled` + `mutate.rules` | — | off |
| Pod console | `rbac.exec` | `exec.pod.enabled` + `exec.pod.rules` | — | off |
| Node console (root on the node) | `rbac.nodeShell` | `exec.node.enabled` + `exec.node.*` | `cluster:node_console` (admin) | off |
| Node jobs (cordon / uncordon / drain) | `rbac.nodeOps` | `mutate.node.enabled` + `nodes` / `verbs` | `cluster:node_write` (write) | off |

Two more boundaries worth knowing:

- **RBAC and policy are reported separately.** A refusal is `RBAC_DENIED` or
  `POLICY_DENIED`, never silence, so the UI can say which of the two is blocking
  a resource — and a disabled feature answers explicitly instead of hanging.
- **What the platform may manage is an allowlist, and it is deliberately narrow.**
  The platform can own `collect.logs`, `collect.state`, `collect.metrics`, `query`
  and `buffer`, and only named keys inside them. `redact_secrets` is **never**
  settable from the platform — a gate is worth nothing if the party it gates can
  open it — and `mutate.*`, `exec.*` and `rbac` are not managed at all: write and
  console authority stays in this chart's values and in the ServiceAccount. A key
  outside the allowlist rejects the whole block rather than being silently
  dropped. Both redaction flags and the applied revision ride the heartbeat, so
  the owner can see what is actually in force.

## Helm install

The chart ships with the hosted platform's gateway as its default
(`gateway.host: agents.kubexa.dev`), so a hosted install needs only a tenant
token. A self-hosted platform sets `gateway.host` to its own agentserver ingress
host — the examples below spell it out either way. Everything beyond reading is
off until you turn it on; see [Trust boundaries](#trust-boundaries).

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
  --version 0.11.1 \
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
  --version 0.11.1 \
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
| `exec.pod.*` | `exec.pod.*` | A shell in a container the platform may open. `rules` (namespace / names / containers patterns), `defaultShell`, `maxSessionSec` [10, 86400], `maxSessions` [1, 64], `resumeWindowSec` (0 = 60, negative = no resume, at most 600). Needs `rbac.exec`. A rule that fails validation is fatal when `enabled`, a warning otherwise -- unless `exec.node` is enabled, which needs a valid `exec.pod` section (rules AND scalars: both are validated at load while `exec.node` is on). |
| `exec.node.*` | `exec.node.*` | A shell on the node itself, through a privileged helper Pod the agent creates and enters with `nsenter`. Three gates, all off: `rbac.nodeShell`, `exec.node.enabled`, and the platform's own `cluster:node_console` admin permission. `image` has no default on purpose -- any image with `sleep` and `nsenter` works. A node session's resume window is exec.pod.resumeWindowSec (the two consoles share one Manager). `maxSessionSec` must be at least `helperReadyTimeoutSec`: the session clock starts at open and the helper wait (image pull included) runs under it. The boot sweep removes orphan helpers from exec.node.namespace AND the release namespace; helpers left in any other namespace (one configured before a change) are not swept and end on activeDeadlineSeconds. With exec.node.namespace set, the rbac.nodeShell Role does not reach it: grant pods create/get/list/watch/delete there yourself. |
| `mutate.node.*` | `mutate.node.*` | Node jobs the platform may run: `cordon`, `uncordon`, `drain`. Three gates, all off: `rbac.nodeOps`, `mutate.node.enabled`, and the platform's own `cluster:node_write` permission at write level. `nodes` (patterns; empty matches nothing), `verbs` (subset of cordon/uncordon/drain; empty grants nothing and is refused), `maxTimeoutSec` [30, 86400] caps a drain's timeout. Shares nothing with `mutate.enabled`/`mutate.rules`. Drain follows `kubectl drain`: DaemonSet and mirror pods are skipped, PodDisruptionBudgets are honoured (blocked pods retry every 5 s until the timeout), pods with no controller need `force`, pods mounting emptyDir need `delete_emptydir_data`; the agent's OWN pod is never evicted (`skipped: agent_self`). A refused drain (needs force / needs emptydir) has changed nothing -- it does not cordon first. `dry_run` sends every write with `dryRun=All`. One job runs at a time. |
| `buffer.*` | `buffer.*` | Memory/disk queue |
| `observability.*` | `observability.*` | Health and metrics ports |
| `log.*` | `log.*` | Agent logger level/format |

`rbac.*` is chart-only — it controls the generated ClusterRole, not a field inside
`pkg/config.Config`, so it is not part of the table above:

| Values path | Description |
|-------------|-------------|
| `rbac.create` | Whether the chart creates the ClusterRole/ClusterRoleBinding at all. Default `true`. |
| `rbac.readAll` | Grants `get`/`list`/`watch` on every apiGroup and resource, CRDs included. Off by default, and deliberately NOT derived from `query.rules`/`collect.state.rules` — the agent's config can be supplied as a mounted file this chart never sees, in which case a derived template would find no rules and silently render the narrow role. Pair with a query/collect rule naming `resources: ["*"]`, or the API server refuses everything the enumerated rules do not name. |
| `rbac.write` | Grants `patch`/`update`/`delete`/`create` and the `*/scale` subresource, enumerated over the same resource lists `readAll`'s neighboring read rules cover — never a wildcard, because `mutate.rules` rejects every wildcard resource form. Off by default, and deliberately NOT derived from `mutate.rules`, for the same mounted-config reason as `readAll`. `mutate.enabled` alone is not enough to let the agent write: this flag is the ServiceAccount side of the same decision. |
| `rbac.exec` | Grants the ServiceAccount `pods` `get` (resolve the default container) and `pods/exec` `create`. Off by default, and deliberately NOT derived from `exec.pod`, for the same mounted-config reason as `readAll`/`write`. |
| `rbac.nodeShell` | Grants the ServiceAccount a Role (not a ClusterRole — the helper Pod lives beside the agent) scoped to this release's namespace: `create`/`get`/`list`/`watch`/`delete` on `pods`, plus the same `pods/exec` `create` grant `rbac.exec` gives. NOT derived from `exec.node`, for the same reason as `rbac.exec`. A node console is root on the node: leave this `false` unless you mean it. |
| `rbac.nodeOps` | Grants `nodes` `get`/`patch`, `pods` `list` in every namespace and `pods/eviction` `create`. Off by default and NOT derived from `mutate.node`. A drain evicts every pod on a node: leave this `false` unless you mean it. |
| `rbac.extraRules` | Extra `PolicyRule` entries appended to the generated ClusterRole verbatim. |

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
collect.logs.tail_lines=50` is refused), and every other block that ends up
inside the agent's config file: `agent`, `gateway`, `buffer`, `observability`
and `log`. A misspelled key there is refused by name instead of rendering
nothing and reading as a setting that took effect, and so is a value of the
wrong type -- `--set buffer.maxDiskBytes=512Mi` never reaches a running agent,
because the agent's own loader wants a byte count there. Leaving a rule's field
blank stays legal -- yaml.v3 binds a blank to the zero value and skips a blank
list entry entirely, so the agent loads it as a filter that was never set.

Durations are the case worth knowing, because the two positions differ:

```bash
--set gateway.dialTimeout=30            # refused: reaches the agent as "30"
--set gateway.dialTimeout=30s           # fine
--set collect.state.resyncPeriod=0      # fine: "0" is how a resync is turned off
--set-json 'collect.metrics.rules[0]={"resources":["pods"],"pod_interval":0}'  # refused
```

The chart quotes a duration written beside a block key, so the agent parses the
value's string form and a bare `0` still reads as a zero duration. Inside a
`rules` entry nothing quotes anything, and the agent refuses any number there
at startup -- which is a CrashLoopBackOff, so helm refuses it first.

That is also why the refusal reads the way it does. Zero being the one number
that works is expressed as a bound, and helm reports the bound rather than the
reason:

```
at '/gateway/dialTimeout': maximum: got 30, want 0
```

The path names the key; the fix is `30s`, not a smaller number.

Charts older than 0.7.5 describe only `query.rules`, so everything under
`collect.*` installs unchecked there; older than 0.7.6 and the same is true of
`gateway`, `buffer`, `observability` and `log`. Agent images newer than 0.7.3
log `unrecognized config key ignored` at startup as a second line of defence,
but only for keys inside the passed-through lists -- a chart key the template
never reads leaves no trace in the rendered config at all.

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

The agent serves its own liveness/readiness on `observability.health_addr`
(default `:8080`) and Prometheus self-metrics on `observability.metrics_addr`
(default `:9090`). `observability.pprof_addr` is **empty, meaning off** — the
agent's heap holds unredacted Secret values from the informer cache and raw log
lines from every collected pod, so a reachable profile endpoint is an
exfiltration path; set it to a loopback address (`127.0.0.1:6060`) and reach it
through `kubectl port-forward` when you need it.

Useful series for capacity planning:

- Queue depth and memory usage (buffer pressure)
- Active log streams and dropped lines (log backpressure)
- Informer cache sync / state event rates (watch scope too broad?)
- Gateway connection state (disconnected → buffer fills)

## Repository layout

| Path | What lives there |
|------|------------------|
| `cmd/agent` | process entrypoint: boot order, wiring, graceful shutdown |
| `cmd/dev-grpc-server` | a local mock gateway, so the agent can be run without the platform |
| `pkg/config` | config structs, loading, validation, and the platform-managed block allowlist |
| `pkg/protoversion` | handshake version negotiation |
| `pkg/buildinfo` | version / commit / build time, injected at link time |
| `internal/stream` | the gRPC session: handshake, reconnect/backoff, circuit breaker, message routing |
| `internal/collector/logs` | log tailing, discovery, line joining, SQLite checkpoints |
| `internal/collector/state` | informer-driven watch and object sanitization |
| `internal/collector/metrics` | Metrics API, cAdvisor, kube-state-metrics, custom endpoints, series budget |
| `internal/query` + `internal/query/policy` | live on-demand reads and the policy that gates them |
| `internal/mutate` + `internal/mutate/policy` | patch / delete / restart / scale / create and its policy |
| `internal/nodeops` | cordon, uncordon, drain as cancellable jobs with progress |
| `internal/exec` | pod and node console sessions, helper-Pod lifecycle, output ring buffer |
| `internal/capability` | API resource discovery and the capability catalog the UI reads |
| `internal/queue` | in-memory queue and the disk spill |
| `internal/ingestrules` | ingest limits the gateway pushes, plus the agent's explicit defaults |
| `internal/k8s` | client-go wiring, typed helpers, retries |
| `internal/health`, `internal/metrics`, `internal/logger`, `internal/pprof` | the agent's own observability |
| `proto/` | the wire contract (`.proto`) and generated Go |
| `helm/kubexa-agent` | the chart that installs all of the above |
| `schema/`, `tools/schemagen` | JSON Schema for managed config blocks, generated from `pkg/config` and diffed in CI |

## Development

No cluster of ours is needed to work on this: `make run-dev-grpc` stands up a
local mock gateway, and `make run-dev` runs the agent against your current
kubeconfig with debug logging and localhost binds.

```bash
make run-dev-grpc     # local mock AgentService (insecure gRPC on 127.0.0.1:50051)
make run-dev          # agent in dev mode: debug logs, localhost bind, kubeconfig
make run-local        # agent with config/example-local.yaml (gitignored; holds tokens)

make test             # go test ./...
make lint             # golangci-lint
make build            # compile the binary
make proto            # regenerate protobuf Go from proto/
make docker-build     # build the runtime image

make helm-lint        # lint chart
make helm-template    # render manifests
make helm-package     # package to dist/
make help             # every target, with its one-line description
```

## CI releases

| Workflow | Trigger | Output |
|----------|---------|--------|
| `helm-oci-release.yaml` | `helm/**` on `main` | `oci://ghcr.io/<org>/charts/kubexa-agent` |
| `helm-release.yaml` | `helm/**` on `main` | `https://kubexa.github.io/helm-charts` |
| `build.yaml` | push to `main`/`develop`, `v*` tags, PRs to `main` | build + test, and a multi-arch (`amd64`/`arm64`) image `ghcr.io/<org>/kubexa-agent`. The `:X.Y.Z` tag the chart's `appVersion` points at is produced **only** by a `v*` tag, so a release needs that tag pushed. |

## Compatibility

- **Kubernetes**: built against client-go `v0.36.x`; the agent discovers what the
  cluster actually serves instead of assuming a version, and reports what it
  cannot read.
- **Architectures**: `linux/amd64` and `linux/arm64` (a single multi-arch image
  tag). An arm64 node pool — Graviton, Ampere — is a supported place to run it.
- **Wire protocol**: negotiated at handshake. A capability the gateway does not
  know is ignored; a request the agent is too old for is refused with an
  "upgrade the agent" answer, not a timeout.
- **Chart ↔ image**: `Chart.appVersion` names the image a chart version was
  tested with, and `helm/kubexa-agent/Chart.yaml` carries a per-version upgrade
  note — read it before an upgrade, especially where it states an install order
  against the platform side.

## Contributing

Issues and pull requests are welcome, particularly from clusters we cannot
reproduce: managed distributions, arm64 pools, restrictive RBAC, CRD-heavy
installs. `make test && make lint && make helm-lint` is what CI runs; see
[Development](#development) for the full target list.

A few conventions this tree holds to, worth knowing before a first patch:

- **Comments explain *why*, at the place where the decision lives.** Several of
  this repo's sharpest edges (why an RBAC flag is not derived from config, why a
  duration is quoted in one position and not another) are recorded in the code
  and chart comments; keep that habit rather than moving the reasoning out.
- **Every behavioural change comes with a test.** Package test coverage is the
  norm here, not the exception.
- **Default to off.** Anything that reads more, writes, or opens a session ships
  disabled, and gets a separate RBAC gate from its policy gate.
- **The chart's `values.schema.json` is part of the contract.** A new value is
  described there too, so a misspelled key is refused by name at install time
  instead of silently doing nothing. `schema/agent-config-blocks.json` is
  generated — run `go run ./tools/schemagen -o schema/agent-config-blocks.json`;
  CI fails on drift.

## Security

If you find a vulnerability, report it privately rather than in a public issue:
**Security → Report a vulnerability** on this repository. [`SECURITY.md`](SECURITY.md)
says what to include and which findings matter most — chiefly anything that
crosses a boundary [Trust boundaries](#trust-boundaries) documents as closed.

## License

GNU General Public License v3.0 — see [LICENSE](LICENSE).
