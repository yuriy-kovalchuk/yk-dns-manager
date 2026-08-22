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

Copy the example env file and point it at your config:

```bash
cp .env.example .env
# Edit .env — set CONFIG_PATH to your config file
```

Credentials do not go in `.env` at all. Create the credential Secret in your
cluster yourself — each provider declares the keys it expects (OPNsense:
`API_KEY` / `API_SECRET`), so the format depends on the provider you use:

```bash
kubectl create secret generic yk-dns-manager-credentials \
  --namespace yk-dns-manager-system \
  --from-literal=API_KEY=your-key \
  --from-literal=API_SECRET=your-secret
```

The controller reads **one** YAML config file at startup (path from `CONFIG_PATH`):

| File | Purpose | Default location (from .env) |
|---|---|---|
| Config | Domain map (hostname → LB IP) + DNS provider instances | `examples/config.yaml` |

Example config:

```yaml
# examples/config.yaml
domainMap:
  "*.example.com": "10.0.0.1"
  "special.example.com": "10.0.0.2"

providers:
  opnsense:
    upsert: true
    # The app reads this Secret from the cluster via the API at startup.
    secret: yk-dns-manager-credentials
    settings:
      base_url: "https://opnsense.example.com/api"
      skip_tls_verify: "true"
      # Credentials (API_KEY / API_SECRET) are not written here.
```

Omit `providers` (or leave it empty) to run in no-op mode.

**How credentials work:** the config names a Kubernetes `Secret` per provider instance; the app reads it from the API at startup. Each provider declares and validates the keys it expects — OPNsense needs `API_KEY` and `API_SECRET`. When running locally (outside the cluster), set `secretsNamespace` at the top of the config file to the namespace where the Secret lives, e.g. `secretsNamespace: yk-dns-manager-system`.

### 2. Run locally (standalone)

The controller runs as a standalone binary, connecting to your K8s cluster via kubeconfig (`~/.kube/config` or `KUBECONFIG`):

```bash
make run
```

This sources `.env`, builds the binary with `-ldflags` for version injection, and starts the controller in debug mode. No Docker setup required — but you need cluster access (kubeconfig) for two things: the controller itself watches HTTPRoutes, and any provider instance with a `secret:` field reads its credentials via the API. Create that Secret in your cluster first with `kubectl create secret` (the keys must match what the provider expects).

### 3. Run locally (kind cluster)

For a fully isolated local environment:

```bash
# Start a kind cluster (once)
make kind-up

# Create the credential Secret yourself — the keys must match what the
# provider expects (OPNsense: API_KEY / API_SECRET):
kubectl create namespace yk-dns-manager-system
kubectl create secret generic yk-dns-manager-credentials \
  --namespace yk-dns-manager-system \
  --from-literal=API_KEY=your-key \
  --from-literal=API_SECRET=your-secret

# Build the image and push it into the kind cluster
make kind-load

# Deploy the chart from local values
make kind-deploy   # helm install/upgrade with chart defaults + local image
# or with your own values file (providers, domain map, ...):
make kind-deploy VALUES=my-values.yaml
```

This uses `hack/kind-config.yaml` to create a single-node cluster. `kind-deploy` installs the chart with its defaults (no-op mode) plus the local kind image (`image.tag=local`, `pullPolicy=Never`) and debug logging. Pass `VALUES=my-values.yaml` to layer a custom values file on top (helm precedence: your file wins over the built-in `--set` flags), e.g. to configure provider instances. `kind-load` and `kind-deploy` are independent steps: `kind-load` builds and pushes the image, `kind-deploy` just installs/upgrades the chart. After a code change, `make kind-reload` (push image + restart the deployment) is the fast loop. Create the Secret in the release namespace (`yk-dns-manager-system`) — the same namespace the pod runs in, so the chart's default (read secrets from the app's own namespace) just works.

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
