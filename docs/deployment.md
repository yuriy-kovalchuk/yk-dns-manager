# Remote Deployment Guide

This guide explains how to install `yk-dns-manager` in your cluster using the remote Helm chart and Docker images from GHCR.

## Prerequisites

1. **Namespace**: Create a namespace for the controller.
   ```bash
   kubectl create namespace yk-dns-manager
   ```

2. **Provider Credentials**: Create a secret containing your DNS provider API keys.
   ```bash
   kubectl create secret generic yk-dns-manager-opnsense-credentials 
     --namespace yk-dns-manager 
     --from-literal=OPNSENSE_API_KEY="your-key" 
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
    skip_tls_verify: "false"
  existingSecret: "yk-dns-manager-opnsense-credentials"

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
helm install yk-dns-manager oci://ghcr.io/yuriy-kovalchuk/yk-dns-manager-chart/yk-dns-manager 
  --version 0.3.1 
  --namespace yk-dns-manager 
  --create-namespace 
  -f values.local.yaml
```

## Verification

Check the logs to ensure the controller has started and correctly identified its version:

```bash
kubectl logs -l app.kubernetes.io/name=yk-dns-manager -n yk-dns-manager -f
```
