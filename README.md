# yk-dns-manager

A Kubernetes controller that watches Gateway API HTTPRoutes and automatically manages DNS records on pluggable DNS backends.

## Installation

### Helm (Recommended)

Create a Secret with your DNS provider credentials in the release namespace. The OPNsense provider expects the keys `API_KEY` and `API_SECRET` (each provider declares the keys it needs):

```bash
kubectl create secret generic opnsense-creds \
  --namespace yk-dns-manager \
  --from-literal=API_KEY=your-key \
  --from-literal=API_SECRET=your-secret
```

Install locally from the `charts/` directory:

```bash
helm install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager --create-namespace \
  --set dnsProviders.opnsense.provider=opnsense \
  --set 'dnsProviders.opnsense.settings.base_url=https://opnsense.example.com/api' \
  --set dnsProviders.opnsense.secret=opnsense-creds
```

The app reads that Secret from the Kubernetes API at startup — credentials never appear in the config file. The chart grants the pod `get` access to exactly the Secret names referenced in `dnsProviders` (a namespace-scoped `Role` restricted via `resourceNames`).

See [docs/deployment.md](docs/deployment.md) for OCI registry deployment.

## Quickstart

### Prerequisites

- Kubernetes cluster with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/) installed
- A supported DNS provider (currently OPNsense)

### Deploy an HTTPRoute

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example-route
spec:
  hostnames:
    - "app.example.com"
  rules:
    - backendRefs:
        - name: my-service
          port: 8080
```

The controller watches for the hostname `app.example.com`, looks up its IP from the **domain map**, and creates a DNS A record pointing to that IP.

### Domain Map

Define which IPs hostnames should resolve to (the `domainMap` section of the config file):

```yaml
example.com: 10.0.0.1
"*.homelab.local": 10.0.0.2
"special.homelab.local": 10.0.0.3   # exact match wins over wildcard
```

Matching priority: **exact** > **wildcard** > **parent domain walk**.

## Configuration

The app reads **one config file** containing both the domain map and the DNS provider instances:

```yaml
# examples/config.yaml
domainMap:
  example.com: 10.0.0.1

providers:
  opnsense:
    provider: opnsense   # optional; defaults to the instance name
    upsert: false
    # Name of the Kubernetes Secret holding this instance's credentials.
    secret: opnsense-creds
    settings:
      base_url: "https://opnsense.example.com/api"
      skip_tls_verify: "false"
      # Credentials (API_KEY, API_SECRET) are deliberately absent here —
      # see "Secrets" below.
```

Optionally set `secretsNamespace` at the top level when the Secrets live in a
namespace other than the app's own (mainly useful when running locally
against a dev cluster). In-cluster, leave it unset.

- **An empty `providers` map is valid:** the controller runs in no-op mode (HTTPRoutes are watched but no records are managed) and starts without failure.
- Every configured instance is an independent backend; each managed record is applied to **all** of them (broadcast).
- `provider` selects the backend type (see [Supported Providers](#supported-providers)); it defaults to the instance name.
- `upsert` is per instance: `true` updates existing records on every reconcile; `false` only creates missing records.

### Secrets

Credentials never live in the config file — they live in a Kubernetes `Secret`, and the app reads them from the API at startup. The config only names the Secret per provider instance (`secret:`); **each provider implementation declares which keys it expects and validates them** in its constructor, failing startup with an error naming any missing key.

Different providers need different credential shapes, and the mechanism is key-agnostic — it just passes the raw Secret data to the provider:

| Provider | Expected Secret keys |
|---|---|
| OPNsense | `API_KEY`, `API_SECRET` |
| (future) single-token | e.g. `API_TOKEN` |
| (future) basic auth | e.g. `USERNAME`, `PASSWORD` |

- **In-cluster:** the app reads the Secret from its own namespace (override with `secretsNamespace`). The Helm chart creates a namespace-scoped `Role` granting `get` on exactly the referenced Secret names.
- **Locally:** the same code path runs against your dev cluster — create the Secret there and point `secretsNamespace` at its namespace (see [docs/local-testing.md](docs/local-testing.md)).

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `CONFIG_PATH` | — (required) | Path to the config YAML file |
| `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

### Command-Line Flags

| Flag | Env Var Override | Default | Description |
|---|---|---|---|
| `--config-path` | `CONFIG_PATH` | — (required) | Path to the config file |
| `--zap-log-level` | `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

Full Helm chart values are in [`charts/yk-dns-manager/values.yaml`](charts/yk-dns-manager/values.yaml).

## Supported Providers

| Provider | Backend |
|---|---|
| OPNsense | Unbound DNS host overrides via OPNsense API |
| Pi-hole | Planned |
| AdGuard Home | Planned |
| CoreDNS | Planned |

For details on adding a new provider, see the [Architecture doc](docs/architecture.md#adding-a-new-provider).

## Helm Chart

Key values:

| Value | Description |
|---|---|
| `domainMap` | Domain-to-IP mapping (rendered as ConfigMap) |
| `dnsProviders` | Map of provider instances; each gets `provider`, `upsert`, `secret`, `settings` |
| `secretsNamespace` | Namespace to read provider credential Secrets from (default: release namespace) |
| `serviceMonitor.enabled` | Create Prometheus ServiceMonitor |

## Testing

```bash
make test              # all tests
make test-unit         # unit tests only
make test-integration  # integration tests
```

See [docs/testing.md](docs/testing.md) for details.

## License

[Apache-2.0](LICENSE)
