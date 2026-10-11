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
    HealthCheck(ctx context.Context) error
    Batch() Batch
}

type Batch interface {
    Ensure(ctx context.Context, record Record, overwrite bool) (Result, error)
    Remove(ctx context.Context, hostname, recordType string) error
    Commit(ctx context.Context) error
}
```

The controller starts one `Batch` per reconcile. A batch reads the backend's records once, on first use, and works from that view, so a route with any number of hostnames costs one read. `Ensure` makes a hostname resolve to a value. It returns `Created`, `Updated`, `Unchanged` (nothing written) or `Kept` (a live record with another value, left alone because `overwrite` is off), and it writes nothing when the record already matches. `Commit` publishes whatever is staged and not yet published, including changes from earlier batches whose publish failed. The controller delegates DNS entirely to the selected provider — it never knows which backend is in use.

## Data Flow

### Create/Update Reconciliation

1. **HTTPRoute triggers reconcile** — controller-runtime watches HTTPRoutes and calls `Reconcile(ctx, req)`.
2. **Read route** — uses `APIReader` (bypasses cache) for read-after-write consistency during conflict retries.
3. **Filter by domain map** — hostnames that have no entry in the domain map are silently skipped. This lets a single controller serve multiple load balancers via different domain maps.
4. **If deletion timestamp set:** remove DNS records, then remove the finalizer (`dns.yk/cleanup`). If record deletion fails, errors are logged but never block the finalizer — the route must always be removed.
5. **If new or updated:** add finalizer (first reconcile), then `Ensure` a record for each matching hostname, with overwrite set from `upsert`, and `Commit` the batch at the end of the reconcile.
6. **Update annotation** (`dns.yk/managed-hostnames`) tracks which hostnames have actual DNS records for comparison on subsequent reconciles.

Anyone who can edit a route can write that annotation, so on its own it never authorizes a delete. Both cleanup paths only delete annotated hostnames the domain map covers. As a result, when a domain is dropped from the map, records already created under it are left in place and logged, to be removed by hand.

### Domain Map Lookup

The domain map walks up the hostname one label at a time. At each level it checks for an exact entry, then for a wildcard covering that level, and returns the first hit:

```
Hostname: sub.app.example.com
  1. "sub.app.example.com", then "*.app.example.com"
  2. "app.example.com",     then "*.example.com"
  3. "example.com",         then "*.com"
  4. "com"
  5. no match
```

The most specific entry wins, and an exact entry beats a wildcard at the same level. Because the walk continues upward, an exact entry such as `example.com` also covers every name under it, and `*.example.com` covers `a.b.example.com` as well as `anything.example.com`.

### DNS Provider Selection

At startup in `main()`, the controller looks up the factory named by the provider config's `provider` field in `newProviders()` and initializes it with the configured settings. There is no fallback: an unknown name, or settings the provider rejects, fails startup with that provider's own error. This means:
- Only one provider is active at a time.
- Adding a new provider is as simple as returning a factory from `newProviders()`.

After initialization the controller runs `HealthCheck()` once. A failure is logged but does not stop startup: every reconcile reports the same failure and retries with backoff, so a provider that comes back recovers without a restart.

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

The Helm chart adds `checksum/` annotations to the pod spec based on ConfigMap content hashes. This triggers a rolling restart whenever domain-map or provider-config changes — essential because the controller reads config only at startup (no hot-reload).

## Adding a New Provider

Three steps, no changes to the controller needed:

**1. Implement the `Provider` and `Batch` interfaces:**
```go
package myprovider

import (
    "context"

    "github.com/go-logr/logr"
    "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func New(log logr.Logger, settings map[string]string) (*Provider, error) { /* ... */ }

type Provider struct { /* ... */ }

func (p *Provider) HealthCheck(ctx context.Context) error { /* ... */ }
func (p *Provider) Batch() dns.Batch                      { /* ... */ }

type batch struct { /* ... */ }

func (b *batch) Ensure(ctx context.Context, record dns.Record, overwrite bool) (dns.Result, error) { /* ... */ }
func (b *batch) Remove(ctx context.Context, hostname, recordType string) error                    { /* ... */ }
func (b *batch) Commit(ctx context.Context) error                                                 { /* ... */ }
```

A backend that applies every write immediately can make `Commit` a no-op. `test/integration` shows the behaviour a provider is expected to have.

**2. Register it in `main()`:**
```go
func newProviders() []providerFactory {
    return []providerFactory{
        {"opnsense", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
            return opnsense.New(log, settings)
        }},
        {"myprovider", func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
            return myprovider.New(log, settings)
        }},
    }
}
```

**3. Set `provider: myprovider` in the DNS provider config.**
