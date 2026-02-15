# yk-dns-manager

A Kubernetes controller that watches Gateway API HTTPRoutes and automatically manages DNS records on custom DNS providers.

## The Problem

You run services on Kubernetes behind a gateway or load balancer. Every time you add a new HTTPRoute, you have to manually create a DNS record in whatever DNS server your homelab uses. Delete a route? Better remember to clean up the record too.

This is tedious, easy to forget, and gets worse as your cluster grows.

**yk-dns-manager** closes the loop: deploy an HTTPRoute, get a DNS record. Delete the HTTPRoute, the record is cleaned up automatically. It works with any DNS backend through a pluggable provider interface you just swap the config.

## How It Works

```
┌──────────────────────┐       ┌──────────────────┐       ┌──────────────────┐
│   Kubernetes Cluster │       │                  │       │                  │
│                      │       │  yk-dns-manager  │       │   DNS Provider   │
│  HTTPRoute created ──┼──────>│                  │──────>│  (any backend)   │
│  HTTPRoute deleted ──┼──────>│  domain map      │──────>│                  │
│                      │       │  lookup + match  │       │                  │
└──────────────────────┘       └──────────────────┘       └──────────────────┘
```

1. The controller watches all HTTPRoute resources in the cluster.
2. When an HTTPRoute is created or updated, it extracts the hostnames (e.g. `app.example.com`).
3. Each hostname is matched against a **domain map** to resolve the target IP address.
4. The configured DNS provider API is called to create or update the record.
5. A **finalizer** (`dns.yk/cleanup`) is added to each HTTPRoute. When the route is deleted, the controller removes the corresponding DNS records before allowing Kubernetes to finalize the resource.

The domain map supports wildcards with per-subdomain overrides:

```yaml
"*.example.com": "10.0.0.1"       # catch-all
"app2.example.com": "10.0.0.2"    # exact match takes priority
```

## Supported Providers

| Provider | Status | Backend |
|---|---|---|
| OPNsense | Available | Unbound DNS host overrides via OPNsense API |
| Pi-hole  | Planned   | — |
| AdGuard Home | Planned | — |
| CoreDNS | Planned | — |

Adding a new provider is straightforward — see [Adding a New Provider](#adding-a-new-provider) below.

## Provider Architecture

The core of the project is a pluggable provider interface. The controller doesn't know or care which DNS backend you use. Adding support for a new one requires no changes to the controller, main entrypoint, or any core code.

### The Interface

Every provider implements five methods:

```go
type Provider interface {
    Exists(ctx context.Context, hostname, recordType string) (bool, error)
    Create(ctx context.Context, record Record) error
    Update(ctx context.Context, record Record) error
    Delete(ctx context.Context, hostname, recordType string) error
    Upsert(ctx context.Context, record Record) error
}
```

### Self-Registration

Providers register themselves at import time using an `init()` function and a central registry:

```go
func init() {
    dns.Register("myprovider", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
        return New(log, settings)
    })
}
```

All providers are pulled in via a single aggregation package with blank imports:

```go
// internal/dns/providers/all.go
package providers

import (
    _ "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
    // add new providers here
)
```

The controller selects the active provider based on the `provider` field in your config file. No code changes needed to switch backends.

## Quick Start

### Prerequisites

- Go 1.26+
- A Kubernetes cluster with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/) installed
- A supported DNS provider (currently OPNsense)

### Local Development

```bash
cp .env.example .env
# Fill in your DNS provider credentials and config paths
make run
```

### Docker

```bash
# Single-platform build
make docker-build

# Multi-platform build and push (linux/amd64, linux/arm64)
make docker-buildx
```

### Helm

Create a secret with your provider credentials (example for OPNsense):

```bash
kubectl create secret generic dns-provider-credentials \
  --namespace yk-dns-manager \
  --from-literal=OPNSENSE_API_KEY=your-key \
  --from-literal=OPNSENSE_API_SECRET=your-secret
```

Install the chart:

```bash
helm install yk-dns-manager charts/yk-dns-manager \
  --namespace yk-dns-manager --create-namespace \
  --set dnsProvider.provider=opnsense \
  --set dnsProvider.settings.base_url=https://opnsense.example.com/api \
  --set 'dnsProvider.settings.api_key=${OPNSENSE_API_KEY}' \
  --set 'dnsProvider.settings.api_secret=${OPNSENSE_API_SECRET}' \
  --set dnsProvider.existingSecret=dns-provider-credentials
```

## Configuration

### Domain Map

Maps base domains (or wildcards) to load balancer IPs. The controller uses this to resolve which IP a DNS record should point to.

```yaml
# configs/domain-map.yaml
example.com: 10.0.0.1
"*.homelab.local": 10.0.0.2
"special.homelab.local": 10.0.0.3   # exact match wins over wildcard
```

Matching priority: exact match > wildcard > parent domain walk.

### DNS Provider

Configures which provider to use and how to connect to it. Values in `settings` support `${ENV_VAR}` expansion.

```yaml
# configs/dns-provider.yaml
provider: opnsense
upsert: false
settings:
  base_url: "https://opnsense.example.com/api"
  skip_tls_verify: "true"
  api_key: "${OPNSENSE_API_KEY}"
  api_secret: "${OPNSENSE_API_SECRET}"
  default_ttl: "300"
```

Set `upsert: true` to update existing records on every reconcile. When `false`, the controller only creates records that don't already exist.

The `provider` field selects the backend. The `settings` map is passed directly to the provider, each provider defines its own keys.

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `DOMAIN_MAP_PATH` | `configs/domain-map.yaml` | Path to the domain map file |
| `DNS_PROVIDER_PATH` | `configs/dns-provider.yaml` | Path to the DNS provider config file |

Provider-specific credentials are referenced in `dns-provider.yaml` via `${ENV_VAR}` syntax. For OPNsense:

| Variable | Description |
|---|---|
| `OPNSENSE_API_KEY` | OPNsense API key |
| `OPNSENSE_API_SECRET` | OPNsense API secret |

## Helm Chart

Key values for the Helm chart:

| Value | Description |
|---|---|
| `image.repository` | Container image registry and name |
| `image.tag` | Image tag (default: `0.1.0`) |
| `domainMap` | Domain-to-IP mapping (rendered as ConfigMap) |
| `dnsProvider.provider` | DNS provider name (e.g. `opnsense`) |
| `dnsProvider.upsert` | Enable upsert mode |
| `dnsProvider.settings` | Provider-specific connection settings |
| `dnsProvider.existingSecret` | Name of an existing Secret with provider credentials |
| `metrics.service.port` | Metrics endpoint port (default: `9090`) |
| `serviceMonitor.enabled` | Create a Prometheus ServiceMonitor |

See `charts/yk-dns-manager/values.yaml` for the full reference.

## Adding a New Provider

Three steps, no changes to the controller or `main.go` needed.

**1. Implement the `Provider` interface:**

```go
// internal/dns/myprovider/myprovider.go
package myprovider

import (
    "context"
    "github.com/go-logr/logr"
    "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func init() {
    dns.Register("myprovider", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
        return &Provider{log: log}, nil
    })
}

type Provider struct {
    log logr.Logger
}

func (p *Provider) Exists(ctx context.Context, hostname, recordType string) (bool, error) { /* ... */ }
func (p *Provider) Create(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Update(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error          { /* ... */ }
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error                   { /* ... */ }
```

**2. Add a blank import to the aggregation package:**

```go
// internal/dns/providers/all.go
import (
    _ "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/opnsense"
    _ "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns/myprovider"
)
```

**3. Reference it in your config:**

```yaml
provider: myprovider
settings:
  # your provider's settings
```

## Testing

```bash
make test              # all tests
make test-unit         # unit tests only
make test-integration  # integration tests
```

See [docs/testing.md](docs/testing.md) for details on test coverage and structure.

## License

[Apache-2.0](LICENSE)
