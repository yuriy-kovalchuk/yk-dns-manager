# Architecture

## Component Overview

```
┌──────────────────────┐       ┌──────────────────┐       ┌──────────────────┐
│   Kubernetes Cluster │       │                  │       │                  │
│                      │       │  yk-dns-manager  │       │   DNS Provider   │
│  HTTPRoute created ──┼──────>│                  │──────>│  (any backend)   │
│  HTTPRoute deleted ──┼──────>│                  │──────>│                  │
│                      │       │                  │       │                  │
└──────────────────────┘       └──────────────────┘       └──────────────────┘
                               ┌──────────┴──────────┐
                               │                     │
                          domain-map.yaml    dns-provider.yaml
```

**yk-dns-manager** is a Kubernetes controller built with `controller-runtime`. It watches all HTTPRoute resources in the cluster and manages DNS records on a configured backend. The architecture follows a feature-based layout with a pluggable provider pattern: no DNS-specific logic exists inside the reconciler, and adding a new backend requires only implementing the `Provider` interface.

### Package Layout

```
cmd/yk-dns-manager/main.go          # Entrypoint: flag parse, provider selection, manager bootstrap
internal/controller/                # HTTPRoute reconciler (the only reconciler)
internal/dns/                       # Provider interface + helpers
  opnsense/opnsense.go              # OPNsense Unbound implementation
internal/config/                    # DomainMap + ProviderConfig loaders
internal/version/version.go         # Version, Commit, BuildDate (injected via ldflags)
charts/yk-dns-manager/              # Helm chart for deployment
```

The controller is a **single-reconciler** design: one `HTTPRouteReconciler` handles all HTTPRoute resources. Domain separation between handler/service/repository layers is unnecessary — the reconciler itself is thin enough (parsing, delegation to DNS provider) that a monolithic structure with clean boundaries suffices.

### Key Interfaces

**Provider interface** (in `internal/dns/provider.go`):
```go
type Provider interface {
    Exists(ctx context.Context, hostname, recordType string) (bool, error)
    Create(ctx context.Context, record Record) error
    Update(ctx context.Context, record Record) error
    Delete(ctx context.Context, hostname, recordType string) error
    Upsert(ctx context.Context, record Record) error
}
```

Each provider implements all five methods. The controller delegates DNS CRUD entirely to the selected provider — it never knows which backend is in use.

## Data Flow

### Create/Update Reconciliation

1. **HTTPRoute triggers reconcile** — controller-runtime watches HTTPRoutes and calls `Reconcile(ctx, req)`.
2. **Read route** — uses `APIReader` (bypasses cache) for read-after-write consistency during conflict retries.
3. **Filter by domain map** — hostnames that have no entry in the domain map are silently skipped. This lets a single controller serve multiple load balancers via different domain maps.
4. **If deletion timestamp set:** remove DNS records, then remove the finalizer (`dns.yk/cleanup`). If record deletion fails, errors are logged but never block the finalizer — the route must always be removed.
5. **If new or updated:** add finalizer (first reconcile), then create/update DNS records for matching hostnames via `DNS.Create`/`DNS.Upsert`.
6. **Update annotation** (`dns.yk/managed-hostnames`) tracks which hostnames have actual DNS records for comparison on subsequent reconciles.

### Domain Map Lookup

The domain map uses a walk-up strategy with priority ordering:

```
Hostname: sub.app.example.com
  1. Exact match: "sub.app.example.com" → if found, return IP
  2. Parent:    "app.example.com"       → check for wildcard "*.app.example.com"
  3. Parent:    "example.com"           → check for wildcard "*.example.com"
  4. Return nil (no match)
```

Exact matches always win over wildcards. Wildcard entries only match at their own level — `"*.example.com"` matches `anything.example.com` but not `a.b.example.com` directly.

### DNS Provider Selection

At startup in `main()`, the controller builds an ordered list of available providers via `newProviders()`. It iterates through them, attempting to initialize each with the configured settings. The first one whose mandatory fields are present and who passes `HealthCheck()` wins. This sequential fallback pattern means:
- Only one provider can be active at a time (the `provider` field selects *which* one to try first).
- Adding a new provider is as simple as returning a factory from `newProviders()`.

## Boundary Rules

| From → To | What crosses the boundary |
|---|---|
| HTTPRoute → Reconciler | Gateway API resource only — no DNS types leak upstream |
| Reconciler → Provider | `dns.Record{Hostname, Type, Value}` — a simple DTO, not tied to any backend |
| Config files → main() | Parsed structs (`DomainMap`, `ProviderConfig`) — raw YAML is deserialized inside the config package |
| DNS Provider → K8s API | **None.** The provider never touches Kubernetes. It's a pure HTTP client. |

## Design Decisions

### Why feature folders instead of layer-first?

The project has only one domain (HTTPRoute → DNS) and one reconciler. Layer-first (`handler/`, `service/`, `repository/`) would add unnecessary indirection. The reconciler is thin enough to be monolithic, and the provider abstraction already provides clean separation from DNS-specific logic.

### Why `APIReader` instead of `Client` for reads?

The reconciler uses `r.APIReader.Get()` (bypasses the informer cache) inside conflict-retry loops. This ensures it sees the latest state of the HTTPRoute after another controller or process might have modified it, preventing stale-cache conflicts on the finalizer update step.

### Why no `init()` for provider registration anymore?

The original design used `init()` + a registry (standard pluggable Go pattern), but this has maintenance costs: new providers require importing into an aggregation package just to register. The current factory-list approach (`newProviders()` in `main.go`) is more explicit and testable, with zero hidden side effects. A future enhancement could restore registration-based discovery if the provider list grows significantly.

### Why checksum annotations on ConfigMaps?

The Helm chart adds `checksum/` annotations to the pod spec based on ConfigMap content hashes. This triggers a rolling restart whenever domain-map or provider-config changes — essential because the controller reads config only at startup (no hot-reload, see BUG.md M1).

## Adding a New Provider

Three steps, no changes to the controller needed:

**1. Implement the `Provider` interface:**
```go
package myprovider

import (
    "context"
    "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func New(settings map[string]string) (*Provider, error) { /* ... */ }

type Provider struct { /* ... */ }

func (p *Provider) Exists(ctx context.Context, hostname, recordType string) (bool, error) { /* ... */ }
func (p *Provider) Create(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Update(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error          { /* ... */ }
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error                   { /* ... */ }
```

**2. Register it in `main()`:**
```go
func newProviders() []providerFactory {
    return []providerFactory{
        {"opnsense", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
            return opnsense.New(settings)
        }},
        {"myprovider", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
            return myprovider.New(settings)
        }},
    }
}
```

**3. Set `provider: myprovider` in the DNS provider config.**
