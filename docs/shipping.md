# Shipping Artifacts to GHCR

This guide explains how to build and push the `yk-dns-manager` image and Helm chart to the GitHub Container Registry (GHCR).

## 1. Authentication

You must authenticate both `docker` and `helm` using a GitHub Personal Access Token (PAT) with `write:packages` scope.

```bash
export GH_PAT=your_token_here
echo $GH_PAT | docker login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
echo $GH_PAT | helm registry login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
```

## 2. Pushing the Container Image

We use `docker buildx` to create a single manifest that supports both Intel/AMD and Apple Silicon/ARM servers.

```bash
# This builds for linux/amd64 and linux/arm64 and pushes to:
# ghcr.io/yuriy-kovalchuk/yk-dns-manager:0.1.0
make docker-buildx
```

## 3. Pushing the Helm Chart

Helm charts are pushed as OCI artifacts. We use a `-chart` suffix to prevent naming collisions with the container image.

```bash
# This packages the chart and pushes to:
# oci://ghcr.io/yuriy-kovalchuk/yk-dns-manager-chart
make helm-push
```

## 4. Verification

After pushing, you should see two packages in your GitHub profile under "Packages":
1. `yk-dns-manager` (Docker Image)
2. `yk-dns-manager-chart` (Helm Chart)

## 5. Remote Installation

Once shipped, anyone with access can install the controller using:

```bash
helm install yk-dns-manager 
  oci://ghcr.io/yuriy-kovalchuk/yk-dns-manager-chart/yk-dns-manager 
  --version 0.1.0 
  --namespace yk-dns-manager 
  --create-namespace 
  -f values.local.yaml
```
