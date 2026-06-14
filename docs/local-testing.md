# Local Testing

## Prerequisites

- Go 1.26+
- Docker (for buildx and kind)
- A local or remote Kubernetes cluster with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/) installed
- `kubectl` configured to your target cluster
- `helm` v3.x
- For kind-based testing: [`kind`](https://kind.sigs.k8s.io/) and a supported DNS provider (OPNsense) reachable from the cluster

## Setup

### 1. Configuration files

Copy the example env file and fill in your credentials:

```bash
cp .env.example .env
# Edit .env — add OPNSENSE_API_KEY and OPNSENSE_API_SECRET, update paths as needed
```

The controller reads two YAML config files at startup:

| File | Purpose | Default location (from .env) |
|---|---|---|
| Domain map | Maps hostnames to LB IPs | `examples/domain-map.yaml` |
| DNS provider config | Backend selection + credentials | `examples/dns-provider.yaml` |

Example domain map:

```yaml
# examples/domain-map.yaml
"*.example.com": "10.0.0.1"
"special.example.com": "10.0.0.2"
```

Example provider config:

```yaml
# examples/dns-provider.yaml
provider: opnsense
upsert: true
settings:
  base_url: "https://opnsense.example.com/api"
  skip_tls_verify: "true"
  api_key: ""       # or hardcode for local dev
  api_secret: ""
  default_ttl: "60"
```

### 2. Run locally (standalone)

The controller runs as a standalone binary, connecting to your K8s cluster via kubeconfig (`~/.kube/config` or `KUBECONFIG`):

```bash
make run
```

This sources `.env`, builds the binary with `-ldflags` for version injection, and starts the controller in debug mode. No Docker or cluster setup required — just ensure you have cluster access configured.

### 3. Run locally (kind cluster)

For a fully isolated local environment:

```bash
# Start a kind cluster
make kind-up

# Create namespace + install DNS provider credentials
make kind-secret   # creates the k8s Secret for OPNsense credentials

# Deploy the chart from local values
make kind-deploy   # builds image, loads into kind, runs helm install
```

This uses `hack/kind-config.yaml` to create a single-node cluster, `hack/local-values.yaml` for preconfigured Helm overrides (debug logging, upsert mode, local domain map), and automates the build→load→install pipeline.

To tear down:

```bash
make kind-down   # removes the kind cluster
```

## Running Tests

### Unit tests

Fast, no network, no external processes. Runs in milliseconds:

```bash
make test-unit
```

Tests live alongside the code they test (`*_test.go` files in the same package). No mocking frameworks — plain structs implementing interfaces. Table-driven tests for multiple input cases.

### Integration tests

Spin up a real HTTP server with in-memory OPNsense-like handlers and exercise the provider over actual HTTP:

```bash
make test-integration
```

These verify wire-format correctness: JSON marshaling, URL construction, HTTP status handling, and response parsing against a fake OPNsense API.

### All tests

```bash
make test          # runs unit + integration
```

### Code coverage

```bash
make test-cover    # HTML report at coverage.html + function summary to stdout
```

### Specific package or file

```bash
go test ./internal/config/ -v
go test -run TestLoadDomainMap ./internal/config/ -v
```

## External Dependencies

The controller needs:

| Dependency | How to provide locally |
|---|---|
| Kubernetes cluster | kind (`make kind-up`), minikube, or any remote cluster with kubeconfig access |
| Gateway API CRDs | Install via `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.1/standard-install.yaml` |
| DNS provider (OPNsense) | For real integration, point to your OPNsense instance. For unit tests only, the integration test suite uses `httptest.NewServer` — no external service needed. |
