# Makefile User Guide

This project uses a `Makefile` to automate building, testing, and deploying the `yk-dns-manager`.

## Core Variables

You can override these variables by passing them as arguments to `make`.

| Variable | Default Value | Description |
|----------|---------------|-------------|
| `VERSION` | `0.1.0` | The version tag for images and charts. |
| `REGISTRY` | `ghcr.io/yuriy-kovalchuk` | The OCI registry base path. |
| `PLATFORMS` | `linux/amd64,linux/arm64` | Target architectures for multi-arch builds. |

## Common Tasks

### Development
- `make build`: Compiles the binary for your local OS (e.g., macOS Darwin) into `bin/`.
- `make run`: Runs the controller locally. Requires a `.env` file (see `.env.example`).
- `make test`: Runs all unit and integration tests.
- `make fmt`: Formats the Go code.

### Docker & Shipping
- `make docker-build`: Builds a Docker image for your local architecture only.
- `make docker-buildx`: Builds and pushes a multi-arch image (AMD64/ARM64) to the registry.
- `make helm-push`: Packages the Helm chart and pushes it to the OCI registry.

## Passing Parameters

You can customize the build without editing the Makefile:

```bash
# Build a specific version
make build VERSION=0.2.0

# Push to a different registry
make docker-buildx REGISTRY=my.private.reg/project

# Run tests with a different platform target
make docker-buildx PLATFORMS=linux/amd64
```
