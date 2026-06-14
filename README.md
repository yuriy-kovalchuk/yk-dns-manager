# yk-dns-manager

A Kubernetes controller that watches Gateway API HTTPRoutes and automatically manages DNS records on pluggable DNS backends.

## Installation

### Helm (Recommended)

Create a secret with your DNS provider credentials:

```bash
kubectl create secret generic dns-provider-credentials \
  --namespace yk-dns-manager \
  --from-literal=OPNSENSE_API_KEY=your-key \
  --from-literal=OPNSENSE_API_SECRET=your-secret
```

Install locally from the `charts/` directory:

```bash
helm install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager --create-namespace \
  --set dnsProvider.provider=opnsense \
  --set 'dnsProvider.settings.api_key=${OPNSENSE_API_KEY}' \
  --set 'dnsProvider.settings.api_secret=${OPNSENSE_API_SECRET}' \
  --set dnsProvider.existingSecret=dns-provider-credentials
```

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

Define which IPs hostnames should resolve to:

```yaml
# configs/domain-map.yaml
example.com: 10.0.0.1
"*.homelab.local": 10.0.0.2
"special.homelab.local": 10.0.0.3   # exact match wins over wildcard
```

Matching priority: **exact** > **wildcard** > **parent domain walk**.

## Configuration

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `DOMAIN_MAP_PATH` | `configs/domain-map.yaml` | Path to the domain map YAML file |
| `DNS_PROVIDER_PATH` | `configs/dns-provider.yaml` | Path to the DNS provider config YAML file |

### Command-Line Flags

| Flag | Env Var Override | Default | Description |
|---|---|---|---|
| `--domain-map-path` | `DOMAIN_MAP_PATH` | — (required) | Path to the domain map file |
| `--zap-log-level` | `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

### DNS Provider Config

```yaml
# configs/dns-provider.yaml
provider: opnsense
upsert: false
settings:
  base_url: "https://opnsense.example.com/api"
  skip_tls_verify: "false"
  api_key: "${OPNSENSE_API_KEY}"
  api_secret: "${OPNSENSE_API_SECRET}"
  default_ttl: "300"
```

- `provider` selects the backend (see [Supported Providers](#supported-providers)).
- `upsert: true` updates existing records on every reconcile; when `false`, only creates missing records.
- Settings support `${ENV_VAR}` expansion for credential injection.

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
| `dnsProvider.provider` | DNS provider name |
| `dnsProvider.existingSecret` | Secret with provider credentials |
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
