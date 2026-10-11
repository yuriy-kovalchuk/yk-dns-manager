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
# examples/domain-map.yaml
example.com: 10.0.0.1
"*.homelab.local": 10.0.0.2
"special.homelab.local": 10.0.0.3   # exact match wins over wildcard
```

Matching walks up the hostname one label at a time and returns the first hit. At each level an exact entry beats a wildcard, so the most specific entry wins. Two consequences: an entry such as `example.com` also covers every name under it, and `*.example.com` covers names at any depth (`a.b.example.com` too), unless something more specific matches first.

## Configuration

### Environment Variables

| Variable | Required | Description |
|---|---|---|
| `DOMAIN_MAP_PATH` | yes, unless `--domain-map-path` is given | Path to the domain map YAML file |
| `DNS_PROVIDER_PATH` | yes — env only, there is no flag for it | Path to the DNS provider config YAML file |

### Command-Line Flags

| Flag | Env Var Override | Default | Description |
|---|---|---|---|
| `--domain-map-path` | `DOMAIN_MAP_PATH` | — (required) | Path to the domain map file |
| `--zap-log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`, or a numeric logr verbosity such as `-3` for `V(3)` |

### DNS Provider Config

```yaml
# examples/dns-provider.yaml
provider: opnsense
upsert: false
settings:
  base_url: "https://opnsense.example.com/api"
  ca_file: "/etc/yk-dns-manager/ca/ca.pem"
  api_key: "${OPNSENSE_API_KEY}"
  api_secret: "${OPNSENSE_API_SECRET}"
```

- `provider` selects the backend (see [Supported Providers](#supported-providers)).
- `upsert: true` updates existing records on every reconcile; when `false`, only creates missing records.
- Settings support `${ENV_VAR}` expansion for credential injection. Credentials must be referenced that way; the chart fails the render if a literal key or secret appears in `values.yaml`.
- `base_url` must be `https`. Plain `http` sends the API credentials in cleartext and is refused unless `allow_insecure_http: "true"` is set, which is meant only for the local fake.
- `ca_file` pins the CA that signed the appliance certificate. `skip_tls_verify: "true"` turns verification off entirely and is a last resort — the API credentials go out as basic auth on every request — and it is rejected together with `ca_file`.

Full Helm chart values are in [`charts/yk-dns-manager/values.yaml`](charts/yk-dns-manager/values.yaml).

## Ownership

The controller only changes or deletes DNS records it created. It marks each one; for OPNsense the override description is `yk-dns-manager: managed record, do not edit`, so do not edit that description by hand. A record someone else created for the same name is left alone and logged as a conflict, whatever `upsert` says. Records created by earlier versions are adopted automatically by the route that created them.

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

## Development

No appliance and no real credentials are needed locally; an in-memory fake stands
in for OPNsense.

```bash
cp .env.example .env
make kind-deploy   # cluster + CRDs + image + fake appliance + controller
make kind-demo     # apply a sample HTTPRoute and watch the record appear
make kind-status   # what is running, and what is missing
```

Most changes need neither of those — `make test` covers the reconciler and the
provider without a cluster. See
[docs/local-testing.md](docs/local-testing.md) for which loop to use, the
standalone binary path, and pointing at a real appliance.

## License

[Apache-2.0](LICENSE)
