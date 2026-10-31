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

Point the chart at your appliance and your load balancer in a values file:

```yaml
# my-values.yaml
domainMap:
  "*.example.com": "10.0.0.100"                    # your load balancer IP
dnsProvider:
  settings:
    base_url: "https://opnsense.example.com/api"   # your appliance
  existingSecret: dns-provider-credentials
```

The chart's defaults already reference the credentials as `${OPNSENSE_API_KEY}` and `${OPNSENSE_API_SECRET}`, which `existingSecret` supplies. Install from the `charts/` directory:

```bash
helm install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager --create-namespace \
  -f my-values.yaml
```

For a self-signed appliance certificate, pinning its CA, and installing from the OCI registry, see [docs/deployment.md](docs/deployment.md).

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

The `domains` section of the config file defines which IPs hostnames should resolve to:

```yaml
domains:
  "*.homelab.local": 10.0.0.2          # every name below homelab.local, at any depth
  "special.homelab.local": 10.0.0.3    # exactly this name; wins over the wildcard
  homelab.local: 10.0.0.2              # the bare domain needs its own entry
```

- An exact key such as `homelab.local` matches only that one name.
- A wildcard key such as `*.homelab.local` matches every name below it (`app.homelab.local`, `a.b.homelab.local`), but not `homelab.local` itself.
- An exact key wins, then the most specific wildcard.
- Keys are case-insensitive, may end in a dot, and must be valid domain names; two keys that differ only in case or a trailing dot are rejected.
- A route hostname that is itself a wildcard (`*.example.com`) gets no record: it would answer for every name below the domain, including names other routes serve. The controller leaves it alone and records a warning Event on the route.

Earlier versions let an exact key cover every name below it too. The controller now logs each exact key that has no matching wildcard at startup; see the CHANGELOG before upgrading.

## Configuration

### Environment Variables

| Variable | Required | Description |
|---|---|---|
| `CONFIG_PATH` | yes, unless `--config` is given | Path to the config YAML file |

### Command-Line Flags

| Flag | Env Var Override | Default | Description |
|---|---|---|---|
| `--config` | `CONFIG_PATH` | — (required) | Path to the config file: DNS provider, settings and domain map |
| `--zap-log-level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`, or a numeric logr verbosity such as `-3` for `V(3)` |
| `--leader-elect` | — | `true` | Elect a leader through a Lease so only one replica manages DNS; turn off only for a single instance outside a cluster (`make run` does) |
| `--require-accepted-routes` | — | `false` | Only publish DNS for HTTPRoutes that at least one parent Gateway has accepted |
| `--cleanup-grace-period` | — | `5m` | How long a failing DNS cleanup of a deleted HTTPRoute is retried before the route is released anyway |
| `--resync-period` | — | `1h` | How often every HTTPRoute is reconciled again even when nothing changed in the cluster; this republishes a record someone changed or deleted on the DNS backend. `0` turns it off |

### Config File

One YAML file holds everything the controller needs:

```yaml
# examples/config.yaml
provider: opnsense
upsert: false
domains:
  "*.example.com": 10.0.0.1
settings:
  base_url: "https://opnsense.example.com/api"
  ca_file: "/etc/yk-dns-manager/ca/ca.pem"
  api_key: "${OPNSENSE_API_KEY}"
  api_secret: "${OPNSENSE_API_SECRET}"
  # Reconfiguring Unbound restarts the resolver, so a burst of route changes
  # costs one reconfigure instead of one per change. Empty or "0" publishes on
  # every change.
  publish_interval: "10s"
```

- `domains` maps hostnames to load balancer IPs: `"*.example.com"` covers every name below example.com at any depth, an exact key covers only that one name, and an exact key wins over the most specific wildcard.
- `provider` selects the backend (see [Supported Providers](#supported-providers)).
- `upsert: true` also corrects a managed record that holds a different IP; when `false`, existing records are left alone and only missing ones are created. Either way nothing is written when a record is already correct.
- Settings support `${ENV_VAR}` expansion for credential injection; a reference to an unset variable fails startup, and a `$` anywhere else is kept as is. Unknown fields and unknown `settings` keys also fail startup. Credentials must be referenced that way; the chart fails the render if a literal key or secret appears in `values.yaml`.
- `skip_tls_verify` and `allow_insecure_http` are booleans (`true`/`false`, also `True`, `1`, `0`); any other value fails startup.
- `base_url` must be `https`. Plain `http` sends the API credentials in cleartext and is refused unless `allow_insecure_http: "true"` is set, which is meant only for the local fake.
- `ca_file` pins the CA that signed the appliance certificate. `skip_tls_verify: "true"` turns verification off entirely and is a last resort — the API credentials go out as basic auth on every request — and it is rejected together with `ca_file`.
- `publish_interval` is the shortest gap between two publishes of the backend (for OPNsense, an Unbound reconfigure, which restarts the resolver). A burst of route changes then costs one publish instead of one per change: the held-back changes stay owed and go out as soon as the wait is over. Empty or `0` publishes on every change.

The controller reads this file once at startup; restart it after a change. Full Helm chart values are in [`charts/yk-dns-manager/values.yaml`](charts/yk-dns-manager/values.yaml).

## Ownership

The controller only changes or deletes DNS records it created. It marks each one; for OPNsense the override description is `yk-dns-manager: managed record, do not edit`, so do not edit that description by hand. The mark also names the routes that asked for the record (`[owners: httproute/<namespace>/<name>]`), and only a route named there can ever have it deleted — writing another route's hostname into your own `dns.yk/managed-hostnames` annotation does nothing. A route that shares a hostname with another joins that list the next time it is reconciled, so the last route left standing can still release the record. A record someone else created for the same name is left alone and logged as a conflict, whatever `upsert` says. Records created by earlier versions are adopted automatically by the route that created them; until that route reconciles once after the upgrade, its record carries no owners and any route naming it can release it.

## Supported Providers

| Provider | Backend |
|---|---|
| OPNsense | Unbound DNS host overrides via OPNsense API |
| Memory | Records in the controller's memory, lost on restart; for local development and tests (`provider: memory`, optional `publish_interval`) |
| Pi-hole | Planned |
| AdGuard Home | Planned |
| CoreDNS | Planned |

For details on adding a new provider, see the [Architecture doc](docs/architecture.md#adding-a-new-provider).

## Helm Chart

The chart renders `domainMap` and `dnsProvider` into the [config file](#config-file), mounts it at `/etc/yk-dns-manager/config/config.yaml`, and rolls the pod when it changes. Key values:

| Value | Description |
|---|---|
| `domainMap` | Domain-to-IP mapping (rendered as `domains` in the config ConfigMap) |
| `dnsProvider.provider` | DNS provider name |
| `dnsProvider.settings` | Provider settings (`base_url`, `ca_file`, …); see [Config File](#config-file) |
| `dnsProvider.existingSecret` | Secret with provider credentials |
| `dnsProvider.caConfigMap` | ConfigMap with the appliance CA under the key `ca.pem`, mounted at `/etc/yk-dns-manager/ca` |
| `leaderElection.enabled` | Leader election through a Lease in the release namespace (on by default; required for `replicaCount` > 1) |
| `requireAcceptedRoutes` | Only publish DNS for HTTPRoutes a Gateway has accepted (off by default) |
| `serviceMonitor.enabled` | Create Prometheus ServiceMonitor |
| `networkPolicy.enabled` | Restrict the pod's traffic; egress ports and CIDRs under `networkPolicy.apiServer` and `networkPolicy.provider` |

## Observability

- **Events** on each HTTPRoute: `kubectl describe httproute <name>` shows records created, updated and released, and warnings for conflicts, drift in create-only mode, and failures.
- **Metrics** on port 9090: `yk_dns_manager_records_total{result}` and `yk_dns_manager_dns_errors_total{stage}`, alongside controller-runtime's reconcile metrics.

## Testing

```bash
make test              # all tests (unit + envtest)
make test-unit         # unit tests, incl. the provider conformance suite
make test-envtest      # the real controller against an envtest API server
```

See [docs/testing.md](docs/testing.md) for details.

## Development

No appliance and no real credentials are needed locally: the local profiles use
`provider: memory`, which keeps records in the controller's memory and logs every
change.

```bash
make kind-deploy   # cluster + CRDs + image + controller with the memory provider
make kind-demo     # apply a sample HTTPRoute and watch the record appear
make kind-status   # what is running, and what is missing
```

Most changes need neither of those — `make test` covers the reconciler and the
provider without a cluster. See
[docs/local-testing.md](docs/local-testing.md) for which loop to use, the
standalone binary path, and pointing at a real appliance.

## License

[Apache-2.0](LICENSE)
