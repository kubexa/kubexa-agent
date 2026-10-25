# Security Policy

## Reporting a vulnerability

Report privately, not in a public issue: **Security → Report a vulnerability**
on this repository (GitHub private vulnerability reporting is enabled). You will
get an acknowledgement, and a fix lands as a new chart `appVersion` with the
upgrade note in `helm/kubexa-agent/Chart.yaml` saying what it changes.

Please do not test against clusters you do not own.

## What to include

This agent's behaviour depends almost entirely on which gates an installation
has opened, so a report is much faster to act on with:

- chart version and `appVersion` (`helm list` / the image tag actually running);
- which gates were on — `rbac.*` (`readAll`, `write`, `exec`, `nodeShell`,
  `nodeOps`), `mutate.*`, `exec.*`, `query.*`;
- whether `collect.state.redactSecrets` / `query.redactSecrets` were set;
- the cluster: distribution, Kubernetes version, node architecture;
- whether any config block was owned by the platform at the time.

## Supported versions

Fixes go to `main` and ship in the next chart release. Only the latest chart
version and the image its `appVersion` names are supported; there are no
backports to older chart lines.

## Scope

In scope: everything in this repository — the agent binary, the Helm chart, the
generated RBAC, the wire contract in `proto/`.

The findings that matter most here are the ones that cross a boundary the README
documents as closed:

- anything that lets the platform act beyond the two independent gates
  (Kubernetes RBAC **and** the owner's config) — see
  [Trust boundaries](README.md#trust-boundaries);
- Secret values leaving the cluster while `redactSecrets: true`, on any path
  (state events, live query full or table view, logs, heartbeat);
- a pod or node console, a mutation or a node job reachable without its own
  RBAC flag, its own config flag, and — for node paths — the platform
  permission;
- a platform-pushed config block that changes a key outside the managed
  allowlist, `redact_secrets` above all;
- the agent reporting a capability, an applied revision or a redaction flag
  that does not match what it is actually enforcing. An honest refusal is a
  feature of this agent; a dishonest "applied" is a bug worth reporting.

Out of scope here: the hosted Kubexa platform and its gateway. Report those
through the platform's own channel rather than this repository.
