# Remote Deployment Guide

This guide explains how to install `yk-dns-manager` in your cluster using the remote Helm chart and Docker images from GHCR.

## Prerequisites

1. **Namespace**: Create a namespace for the controller.
   ```bash
   kubectl create namespace yk-dns-manager
   ```

2. **Provider Credentials**: Create a Secret containing your DNS provider credentials in the release namespace. The app reads it from the API at startup, so the keys must be exactly what the provider expects — OPNsense needs `API_KEY` and `API_SECRET`.
   ```bash
   kubectl create secret generic opnsense-creds \
     --namespace yk-dns-manager \
     --from-literal=API_KEY="your-key" \
     --from-literal=API_SECRET="your-secret"
   ```

## Configuration (`values.local.yaml`)

Create a local override file — `domainMap` and `dnsProviders` are rendered into a single `config.yaml` ConfigMap. Below is a minimal example for an OPNsense setup:

```yaml
domainMap:
  "*.example.com": "10.0.0.100" # Replace with your LoadBalancer IP

dnsProviders:
  opnsense:
    provider: opnsense
    # Name of the Secret created above; the app reads it from the cluster.
    secret: "opnsense-creds"
    settings:
      base_url: "https://opnsense.example.com/api" # Replace with your OPNsense URL
      skip_tls_verify: "false"

# Multiple instances are supported — each one receives every managed record
# and can reference its own Secret:
# dnsProviders:
#   opnsense-1:
#     secret: "opnsense-1-creds"
#     settings:
#       base_url: "https://opnsense-1.example.com/api"
#   opnsense-2:
#     secret: "opnsense-2-creds"
#     settings:
#       base_url: "https://opnsense-2.example.com/api"

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 128Mi
```

## Installation

Run the following command to install the controller from the OCI registry:

```bash
helm install yk-dns-manager oci://ghcr.io/yuriy-kovalchuk/yk-dns-manager-chart/yk-dns-manager \
  --version <version> 
  --namespace yk-dns-manager 
  --create-namespace 
  -f values.local.yaml
```

## Verification

Check the logs to ensure the controller has started and correctly identified its version:

```bash
kubectl logs -l app.kubernetes.io/name=yk-dns-manager -n yk-dns-manager -f
```
