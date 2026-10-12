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
cmd/yk-dns-manager/main.go          # Entrypoint: flags, provider selection, manager bootstrap
internal/controller/                # HTTPRoute reconciler (the only reconciler) and its metrics
internal/dns/provider.go            # Provider and Batch interfaces
  opnsense/opnsense.go              # OPNsense Unbound implementation
  dnstest/                          # Conformance suite every provider runs
internal/config/                    # DomainMap + ProviderConfig loaders
internal/testenv/                   # In-memory OPNsense fake (tests and `make fake-opnsense`)
internal/version/version.go         # Version, Commit, BuildDate (injected via ldflags)
test/integration/                   # Provider against the fake over HTTP, incl. conformance
test/envtest/                       # Real manager against an envtest API server
charts/yk-dns-manager/              # Helm chart for deployment
hack/                               # kind config, local profiles, the fake's entrypoint
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
    Ensure(ctx context.Context, record Record, opts EnsureOptions) (Result, error)
    Remove(ctx context.Context, hostname, recordType string, adoptLegacy bool) error
    Commit(ctx context.Context) error
}
```

The controller starts one `Batch` per reconcile. A batch reads the backend's records once, on first use, and works from that view, so a route with any number of hostnames costs one read. `Ensure` makes a hostname resolve to a value as a managed record. It returns `Created`, `Updated`, `Unchanged` (nothing written), `Kept` (a managed record with another value, left alone because `opts.Overwrite` is off) or `Conflict` (someone else's record holds the name, and nothing was written). `Remove` deletes only managed records. `Commit` publishes whatever is staged and not yet published, including changes from earlier batches whose publish failed. The controller delegates DNS entirely to the selected provider — it never knows which backend is in use.

## Data Flow

### Create/Update Reconciliation

1. **HTTPRoute triggers reconcile** — controller-runtime watches HTTPRoutes and calls `Reconcile(ctx, req)` for a spec change, a deletion request, our finalizer coming or going, and, with `--require-accepted-routes`, a change in acceptance. Status- and annotation-only updates are ignored.
2. **Read route** from the informer cache.
3. **Filter by domain map** — hostnames that have no entry in the domain map are skipped. A hostname that is itself a wildcard (`*.example.com`) is skipped too, with a warning Event: it matches the wildcard key, but managing it would put a wildcard record on the backend that answers for every name below the domain. With `--require-accepted-routes`, a route no parent Gateway reports `Accepted=True` for has no hostnames to manage at all.
4. **If deletion timestamp set:** release the route's records and publish the change, then remove the finalizer (`dns.yk/cleanup`). A failing cleanup holds the finalizer and is retried every 10s until `--cleanup-grace-period` (default 5m) after the deletion request; after that the finalizer is removed anyway, because a route must always be deletable.
5. **If new or updated:** add the finalizer if missing and carry on in the same reconcile, release owned hostnames that left the spec or the domain map, then `Ensure` a record for each matching hostname with `Overwrite` set from `upsert`, and `Commit` the batch at the end of the reconcile.
6. **Update annotation** (`dns.yk/managed-hostnames`): the hostnames this route actually manages. A hostname that hit a conflict is left out.

### Ownership

The provider decides which records it may touch, not the controller:

- **The marker.** Every record the controller writes carries an ownership marker; for OPNsense it is the override description `yk-dns-manager: managed record, do not edit`. The provider only ever changes or deletes records carrying it. A live record without it belongs to someone else, and `Ensure` reports `Conflict` instead of replacing or shadowing it. Route editors cannot write the marker, so nothing in an HTTPRoute can make the controller delete a record it does not manage.
- **The annotation** remembers which hostnames a route manages, so a hostname that leaves the spec or the domain map can be released. It never authorizes a delete on its own.
- **Shared hostnames.** Routes are indexed by hostname. Releasing a hostname deletes its record only when no other live route still claims it.
- **Upgrading.** Versions before the marker wrote `managed by yk-dns-manager`. Such a legacy record is adopted (`EnsureOptions.AdoptLegacy`, `Remove`'s `adoptLegacy`) only for a hostname the route's annotation already lists and the domain map covers, and is rewritten with the marker. The annotation only lists names the route actually manages, so a new route cannot adopt another route's legacy record through it.

### Domain Map Lookup

An exact key matches only that name. Otherwise the lookup walks up the hostname and returns the first wildcard covering it, so the most specific wildcard wins:

```
Hostname: sub.app.example.com
  1. "sub.app.example.com"   exact key
  2. "*.app.example.com"
  3. "*.example.com"
  4. "*.com"
  5. no match
```

A wildcard covers names at any depth below it, like a DNS wildcard, but not its bare domain: `*.example.com` matches `a.b.example.com` but not `example.com`. Keys are normalized at load time (lowercase, no trailing dot) and must be valid domain names.

### DNS Provider Selection

At startup in `main()`, the controller looks up the factory named by the provider config's `provider` field in `newProviders()` and initializes it with the configured settings. There is no fallback: an unknown name, or settings the provider rejects, fails startup with that provider's own error. This means:
- Only one provider is active at a time.
- Adding a new provider is as simple as returning a factory from `newProviders()`.

After initialization the controller runs `HealthCheck()` once, with a 10s deadline. A failure is logged but does not stop startup: every reconcile reports the same failure and retries with backoff, so a provider that comes back recovers without a restart.

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

### Reads and writes

The reconciler reads routes from the informer cache. Finalizer changes re-read the route through `APIReader` (bypassing the cache) inside a conflict-retry loop: a merge patch would replace the whole finalizer list and could drop another controller's finalizer. The managed-hostnames annotation is written as a merge patch of that one key, which cannot conflict with anyone else's change.

### Observability

The controller records Events on each HTTPRoute (`DNSRecordCreated`, `DNSRecordUpdated`, `DNSRecordReleased`, and warnings `DNSConflict`, `DNSRecordDrift`, `DNSSyncFailed`, `DNSCleanupFailed`, `DNSCleanupAbandoned`) and exports `yk_dns_manager_records_total{result}` and `yk_dns_manager_dns_errors_total{stage}` next to controller-runtime's own metrics. Readiness does not track the provider, so an unreachable appliance does not make the pod unready.

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

func (b *batch) Ensure(ctx context.Context, record dns.Record, opts dns.EnsureOptions) (dns.Result, error) { /* ... */ }
func (b *batch) Remove(ctx context.Context, hostname, recordType string, adoptLegacy bool) error         { /* ... */ }
func (b *batch) Commit(ctx context.Context) error                                                        { /* ... */ }
```

The provider must be able to tell its own records from everyone else's (see [Ownership](#ownership)): it needs somewhere on the backend to store a marker. A backend that applies every write immediately can make `Commit` a no-op.

Run the conformance suite against it. It pins the whole contract, ownership included, and only needs an adapter that seeds records and reads them back from a test backend; `test/integration/conformance_test.go` is the OPNsense one:

```go
func TestConformance(t *testing.T) {
    dnstest.Run(t, func(t *testing.T) (dns.Provider, dnstest.Backend) {
        // start a fake backend, return the provider and an adapter over it
    })
}
```

**2. Register it in `newProviders()` in `cmd/yk-dns-manager/main.go`**, listing every settings key it understands; any other key fails startup:
```go
{
    name:     "myprovider",
    settings: []string{"base_url", "api_token"},
    fn: func(log logr.Logger, settings map[string]string) (dns.Provider, error) {
        return myprovider.New(log, settings)
    },
},
```

If it needs secrets in its settings, add their key names to the literal-credential check in `charts/yk-dns-manager/templates/configmap-dns-provider.yaml`, so the chart refuses them written in plain text.

**3. Set `provider: myprovider` in the DNS provider config.**
