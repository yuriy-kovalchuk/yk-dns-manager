# yk-dns-manager

A Kubernetes controller that watches Gateway API HTTPRoutes and automatically manages DNS records on pluggable DNS backends.

## How it works

When an HTTPRoute with a hostname that matches the **domain map** is created or updated, the controller creates the corresponding DNS A record on **every** configured provider backend (e.g. an OPNsense appliance). On route deletion, a finalizer triggers cleanup of its records.

## Quickstart

### 1. Create the credentials Secret

The OPNsense provider expects the keys `API_KEY` and `API_SECRET` (each provider declares the keys it needs):

```bash
kubectl create secret generic opnsense-creds \
  --namespace yk-dns-manager \
  --from-literal=API_KEY=your-key \
  --from-literal=API_SECRET=your-secret
```

### 2. Install the chart

```bash
helm install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager --create-namespace \
  --set 'dnsProviders.opnsense.settings.base_url=https://opnsense.example.com/api' \
  --set dnsProviders.opnsense.secret=opnsense-creds
```

The app reads the Secret from the Kubernetes API at startup — credentials never appear in the config. The chart grants the pod `get` access to exactly the referenced Secret names (a namespace-scoped `Role` restricted via `resourceNames`).

See [docs/deployment.md](docs/deployment.md) for OCI registry deployment.

### 3. Deploy an HTTPRoute

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example-route
spec:
  hostnames:
    - "example.com"
  rules:
    - backendRefs:
        - name: my-service
          port: 8080
```

The controller resolves `example.com` to `10.0.0.1` (from the chart's default `domainMap`) and creates the DNS A record.

### Domain map

The `domainMap` section of the config defines which IPs hostnames should resolve to:

```yaml
example.com: 10.0.0.1
"*.homelab.local": 10.0.0.2
"special.homelab.local": 10.0.0.3   # exact match wins over wildcard
```

Lookup walks up the hostname labels; at each level an exact entry wins, then a `*.parent` wildcard.

## Configuration

One config file holds the domain map and the provider instances (the chart renders it into a ConfigMap):

```yaml
domainMap:
  example.com: 10.0.0.1

providers:
  opnsense:
    provider: opnsense   # optional; defaults to the instance name
    upsert: false
    secret: opnsense-creds   # K8s Secret holding the credentials
    settings:
      base_url: "https://opnsense.example.com/api"
      skip_tls_verify: "false"
```

- An **empty `providers` map is valid** — no-op mode: routes are watched, no records are managed.
- Every managed record is applied to **all** configured instances (broadcast).
- `upsert` is per instance: `true` updates existing records on every reconcile; `false` only creates missing ones.
- Optional top-level `secretsNamespace`: where credential Secrets live (default: the app's own namespace; set it when running locally against a dev cluster).

Credentials never live in the config file: the app reads each `secret:` via the API at startup, and **each provider declares and validates the keys it expects** (OPNsense: `API_KEY`, `API_SECRET`), failing startup with an error naming any missing key. The mechanism is key-agnostic, so other providers can use tokens or username/password.

### Flags and environment

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--config-path` | `CONFIG_PATH` | — (required) | Path to the config file |
| `--zap-log-level` | `LOG_LEVEL` | `info` | Log level (`debug`, `info`, `warn`, `error`) |

## Supported providers

| Provider | Backend |
|---|---|
| OPNsense | Unbound DNS host overrides via OPNsense API |
| Pi-hole, AdGuard Home, CoreDNS | Planned |

Per-provider configuration and credential Secrets: [docs/providers.md](docs/providers.md). Adding a provider: implement the `Provider` interface — see [Architecture](docs/architecture.md#adding-a-new-provider).

## Helm values

| Value | Description |
|---|---|
| `domainMap` | Domain-to-IP mapping (rendered into the config ConfigMap) |
| `dnsProviders` | Map of provider instances: `provider`, `upsert`, `secret`, `settings` |
| `secretsNamespace` | Namespace to read credential Secrets from (default: release namespace) |
| `serviceMonitor.enabled` | Create a Prometheus ServiceMonitor |

All values: [`charts/yk-dns-manager/values.yaml`](charts/yk-dns-manager/values.yaml).

## Local development

`make kind-up && make kind-load && make kind-deploy` runs the whole thing in a local kind cluster — see [docs/local-testing.md](docs/local-testing.md).

## Testing

```bash
make test              # all tests
make test-unit         # unit tests only
make test-integration  # integration tests
```

See [docs/testing.md](docs/testing.md) for details.

## License

[Apache-2.0](LICENSE)
