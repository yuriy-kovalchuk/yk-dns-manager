# Remote Deployment Guide

This guide explains how to install `yk-dns-manager` in your cluster using the remote Helm chart and Docker images from GHCR.

## Prerequisites

1. **Namespace**: Create a namespace for the controller.
   ```bash
   kubectl create namespace yk-dns-manager
   ```

2. **Provider Credentials**: Create a secret containing your DNS provider API keys.
   ```bash
   kubectl create secret generic yk-dns-manager-opnsense-credentials \
     --namespace yk-dns-manager \
     --from-literal=OPNSENSE_API_KEY="your-key" \
     --from-literal=OPNSENSE_API_SECRET="your-secret"
   ```

## Configuration (`values.local.yaml`)

Create a local override file. Below is a minimal example for an OPNsense setup:

```yaml
domainMap:
  "*.example.com": "10.0.0.100" # Replace with your LoadBalancer IP

dnsProvider:
  provider: opnsense
  settings:
    base_url: "https://opnsense.example.com/api" # Replace with your OPNsense URL
    # Pin the CA that signed the appliance certificate rather than turning
    # verification off. Put the CA in a ConfigMap under the key ca.pem and name
    # that ConfigMap in caConfigMap.
    ca_file: "/etc/yk-dns-manager/ca/ca.pem"
  existingSecret: "yk-dns-manager-opnsense-credentials"
  caConfigMap: "opnsense-ca"

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 128Mi
```

The chart renders `domainMap` and `dnsProvider` into one config file, mounted at
`/etc/yk-dns-manager/config/config.yaml`. To run the binary outside the chart,
write that file yourself (see `examples/config.yaml`) and pass it with `--config`
or `CONFIG_PATH`.

## Installation

Run the following command to install the controller from the OCI registry:

```bash
helm install yk-dns-manager oci://ghcr.io/yuriy-kovalchuk/charts/yk-dns-manager \
  --version <version> \
  --namespace yk-dns-manager \
  --create-namespace \
  -f values.local.yaml
```

The chart is published under `oci://ghcr.io/yuriy-kovalchuk/charts/`. The image it
defaults to is `ghcr.io/yuriy-kovalchuk/yk-dns-manager`, tagged with the chart's
`appVersion`, which the release pipeline stamps from the release tag.

## Verification

Check the logs to ensure the controller has started and correctly identified its version:

```bash
kubectl logs -l app.kubernetes.io/name=yk-dns-manager -n yk-dns-manager -f
```
