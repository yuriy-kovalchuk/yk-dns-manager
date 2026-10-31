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

To run an unreleased build, install the chart from a checkout of the repository
and point it at the image CI publishes for every commit on master:

```bash
helm upgrade --install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager \
  -f values.local.yaml \
  --set image.tag=master-<full commit sha>
```

## Monitoring

The controller serves Prometheus metrics on port 9090 (listed in the
[README](../README.md#observability)). Two values ship what a Prometheus Operator
setup or Grafana Alloy needs:

```yaml
serviceMonitor:
  enabled: true          # scraped by anything that reads ServiceMonitors
grafanaDashboard:
  enabled: true          # ConfigMap labelled grafana_dashboard: "1" for the Grafana sidecar
  # annotations:
  #   grafana_folder: DNS   # only used when the sidecar's folderAnnotation is set
```

The dashboard is also in `charts/yk-dns-manager/dashboards/yk-dns-manager.json`
for importing by hand.

## Verification

Check the logs to ensure the controller has started, loaded its config and
reached the provider:

```bash
kubectl logs -l app.kubernetes.io/name=yk-dns-manager -n yk-dns-manager -f
```

Look for `loaded config`, `loaded domain map` with the number of entries, and
no `DNS provider is unreachable` error. Then:

```bash
# the replica that manages DNS
kubectl -n yk-dns-manager get lease yk-dns-manager.dns.yk
# what happened to each route's records
kubectl get events -A --field-selector reportingComponent=yk-dns-manager
```

Every route the domain map covers carries the `dns.yk/cleanup` finalizer and lists
its hostnames in the `dns.yk/managed-hostnames` annotation.
