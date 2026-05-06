# kubexa-agent
Secure agent that connects Kubernetes clusters to the Kubexa platform for monitoring, control, and real-time operations.

Free agent is open-source. Premium features require subscription and private repo.

## Features

- Collects Kubernetes pod logs.
- Captures pod events for cluster activity monitoring.
- Deployable with Helm using the included `helm/kubexa-agent` chart.

## Helm install examples

```bash
# Install from local chart directory
helm install kubexa-agent ./helm/kubexa-agent \
  --set agent.clusterId=prod-01 \
  --set agent.backend.host=backend.example.com \
  --set agent.backend.port=443 \
  --set agent.backend.tls=true \
  --set secrets.grpcToken=mytoken

# Install from OCI registry without a local chart checkout
helm install kubexa-agent oci://registry.example.com/charts/kubexa-agent \
  --version 0.1.0 \
  --set agent.clusterId=prod-01 \
  --set agent.backend.host=backend.example.com \
  --set secrets.grpcToken=mytoken

# Enable pod log collection for specific namespace and label selector
helm install kubexa-agent ./helm/kubexa-agent \
  --set agent.clusterId=prod-01 \
  --set agent.backend.host=backend.example.com \
  --set secrets.grpcToken=mytoken \
  --set agent.logs.enabled=true \
  --set agent.logs.targets[0].namespace=production \
  --set agent.logs.targets[0].labelSelector="tier=backend"
```

> The Helm chart supports configuring the agent cluster ID, backend endpoint, secure gRPC token, log collection targets, watch namespaces, and optional Redis spillover buffering.
