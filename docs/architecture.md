# Architecture

## Component Overview

```
┌──────────────────────┐       ┌──────────────────┐       ┌───────────────────────┐
│   Kubernetes Cluster │       │                  │       │     DNS Providers     │
│                      │       │  yk-dns-manager  │       │                       │
│  HTTPRoute created ──┼──────>│                  │──────>│  instance 1 (OPNsense)│
│  HTTPRoute deleted ──┼──────>│                  │──────>│  instance 2 (anything)│
│                      │       │                  │       │  ... (broadcast)      │
└──────────────────────┘       └──────────────────┘       └───────────────────────┘
                               ┌──────────┴──────────┐
                               │                     │
                          config.yaml (domainMap + providers)
```

**yk-dns-manager** is a Kubernetes controller built with `controller-runtime`. It watches all HTTPRoute resources in the cluster and manages DNS records on one or more configured provider backends. The architecture follows a feature-based layout with a pluggable provider pattern: no DNS-specific logic exists inside the reconciler, and adding a new backend requires only implementing the `Provider` interface.

### Package Layout

```
cmd/yk-dns-manager/main.go          # Entrypoint only: flag parse, config load, manager bootstrap
internal/app/                       # app.Build: provider factory registry, credential Secrets, health checks
internal/controller/                # HTTPRoute reconciler + RouteState (all K8s mutations)
internal/dns/                       # Provider interface, Manager (fan-out), helpers
  opnsense/opnsense.go              # OPNsense Unbound implementation
internal/config/                    # Config (domain map + providers) loader, DomainMap
internal/version/version.go         # Version, Commit, BuildDate (injected via ldflags)
charts/yk-dns-manager/              # Helm chart for deployment
```

`main.go` is deliberately thin: it parses flags, loads the config, assembles dependencies (`app.Build`), and starts the manager. Everything else is in the internal packages.

The controller is a **single-reconciler** design: one `HTTPRouteReconciler` handles all HTTPRoute resources. Domain separation between handler/service/repository layers is unnecessary — the reconciler is thin orchestration (filtering, delegation) that delegates to two well-defined components:

- **`RouteState`** (`internal/controller/routestate.go`) — the *only* code that mutates Kubernetes: adding/removing the `dns.yk/cleanup` finalizer and writing the `dns.yk/managed-hostnames` annotation. Because there is exactly one instance of it, every K8s mutation happens exactly once per reconcile, no matter how many DNS provider instances exist.
- **`dns.Manager`** (`internal/dns/manager.go`) — fans every record operation out to all configured provider instances, applying each instance's own upsert policy and joining per-instance errors.

### Key Interfaces

**Provider interface** (in `internal/dns/provider.go`):
```go
type Provider interface {
    Exists(ctx context.Context, hostname, recordType string) (bool, error)
    Create(ctx context.Context, record Record) error
    Update(ctx context.Context, record Record) error
    Delete(ctx context.Context, hostname, recordType string) error
    Upsert(ctx context.Context, record Record) error
    HealthCheck(ctx context.Context) error
}
```

Each provider implements all six methods. Providers are **pure HTTP backends** — they never touch Kubernetes. The controller never talks to a provider directly; it delegates to the `dns.Manager`.

**Manager** (in `internal/dns/manager.go`):
```go
type Manager struct { /* unexported instances []instance */ }

func (m *Manager) Add(name string, upsert bool, p Provider)                 // register one configured instance
func (m *Manager) EnsureRecord(ctx context.Context, record Record) error    // fan-out, per-instance upsert policy
func (m *Manager) DeleteRecord(ctx context.Context, hostname, recordType string) error
func (m *Manager) HealthCheck(ctx context.Context) error                    // all instances must pass
func (m *Manager) Len() int
```

**RouteState** (in `internal/controller/routestate.go`) — owns all Kubernetes writes for managed routes:
```go
func (s *RouteState) Get(ctx context.Context, nn types.NamespacedName) (*gatewayv1.HTTPRoute, error)
func (s *RouteState) EnsureFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute) error
func (s *RouteState) RemoveFinalizer(ctx context.Context, route *gatewayv1.HTTPRoute) error
func (s *RouteState) SetManagedHostnames(ctx context.Context, route *gatewayv1.HTTPRoute, hostnames []string) error
func (s *RouteState) ManagedHostnames(route *gatewayv1.HTTPRoute) []string
```

## Data Flow

### Startup

```
main()
 ├─ config.LoadConfigFromPath          config.yaml: domainMap + providers + secretsNamespace
 ├─ app.Build
 │   ├─ resolve secret namespace       secretsNamespace → POD_NAMESPACE (in-cluster)
 │   ├─ per instance: kube Get(secret) → dns.Credentials (raw, key-agnostic)
 │   ├─ factory registry: type → New(log, settings, creds)   each provider validates its own keys
 │   └─ HealthCheck every instance     fail-fast; zero instances → no-op mode
 └─ ctrl.NewManager + RouteState + HTTPRouteReconciler → mgr.Start
```

### Reconciliation (create/update/delete)

1. **HTTPRoute triggers reconcile** — controller-runtime watches HTTPRoutes and calls `Reconcile(ctx, req)`. The route is read via `APIReader` (bypasses cache) for read-after-write consistency during conflict retries.
2. **Deletion is handled first, independently of the domain map** — if `DeletionTimestamp` is set: delete DNS records for **annotation ∪ mapped spec hostnames** (best-effort — failures are logged, never block), then remove the finalizer (`dns.yk/cleanup`). The route must always be deletable, even when its hostnames no longer map.
3. **Live path — filter by domain map** — `mappedSpecHostnames` = spec hostnames present in the domain map, deduplicated. Unmapped hostnames are skipped, letting one controller serve multiple load balancers via different domain maps.
4. **Ownership** — if the route has mapped hostnames (or a non-empty annotation) but no finalizer yet, add it via `RouteState.EnsureFinalizer` and return; the finalizer-change event triggers the next reconcile. Routes with nothing mapped and no annotation are never touched.
5. **Record cleanup** — for every hostname in the `managed-hostnames` annotation that is no longer in the mapped spec, delete its DNS record. This is what makes a route that loses its last mapped hostname clean up instead of leaking records.
6. **Record ensure** — `Manager.EnsureRecord` for each mapped spec hostname — applies each provider instance's policy (upsert, or exists-check + create) and fans out to all instances.
7. **Annotation sync** — `RouteState.SetManagedHostnames` writes the current managed list (removing the annotation when it becomes empty). The annotation is the **source of truth** for what the controller manages.

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

### DNS Provider Instances

At startup, `app.Build` (in `internal/app`) resolves a factory per configured instance via the `factories` registry (provider *types*) and initializes every instance configured under `providers:` in the config file. **All** instances are active simultaneously; every record is applied to each of them (broadcast). This means:
- Multiple backends of the same or different types can run side by side (e.g. two OPNsense appliances in different datacenters).
- A misconfigured instance fails startup with a clear error (`provider instance "name": unknown provider type "..."`) instead of being silently skipped.
- `Manager.HealthCheck` requires every instance to be reachable before the controller starts serving.
- `upsert` is a per-instance policy, decided inside `Manager.EnsureRecord`; the reconciler does not branch on it.
- **Credentials come from Kubernetes Secrets, read at startup.** Each instance's `secret:` field names a Secret; `app.Build` reads it via the API (`loadCredentials`) and hands the raw data to the provider constructor as `dns.Credentials`. **Each provider declares and validates its own keys** — the mechanism is key-agnostic, so providers can require different credential shapes (key+secret, a single token, username+password). The Secret namespace is `secretsNamespace` from the config, or the pod's own namespace in-cluster.
- **Zero instances is valid (no-op mode).** An empty `providers` map starts the controller without failure: HTTPRoutes are still watched and finalizers/annotations are maintained, but every `Manager` operation is a no-op. Configure instances later (rolling restart via the ConfigMap checksum) to start managing records.

## Boundary Rules

| From → To | What crosses the boundary |
|---|---|
| HTTPRoute → Reconciler | Gateway API resource only — no DNS types leak upstream |
| Reconciler → RouteState | `*gatewayv1.HTTPRoute` — all K8s writes go through this one component |
| Reconciler → dns.Manager | `dns.Record{Hostname, Type, Value}` — a simple DTO, not tied to any backend |
| dns.Manager → Provider | `dns.Record` + context — the manager knows instance names and upsert policies, nothing else |
| Config file → main() → app.Build | Parsed `config.Config{DomainMap, Providers, SecretsNamespace}` — raw YAML is deserialized inside the config package |
| K8s Secret → app.Build | Raw `map[string][]byte` data at startup only (`loadCredentials`) — no hot reload, no caching beyond process lifetime |
| app.Build → Provider | `dns.Credentials{SecretName, Data}` — key-agnostic; the provider picks out the keys it declared |
| DNS Provider → K8s API | **None.** The provider never touches Kubernetes. It's a pure HTTP client. |

RBAC for the Secret read is deliberately minimal: the chart renders a namespace-scoped `Role` with `secrets: [get]` restricted via `resourceNames` to exactly the Secret names referenced in values (rendered only when at least one instance sets `secret`).

## Design Decisions

### Why feature folders instead of layer-first?

The project has only one domain (HTTPRoute → DNS) and one reconciler. Layer-first (`handler/`, `service/`, `repository/`) would add unnecessary indirection. The reconciler is thin enough to be monolithic, and the provider abstraction already provides clean separation from DNS-specific logic.

### Why `RouteState` and `dns.Manager` as separate components?

With multiple provider instances, the old "reconciler does everything" shape risked tangling K8s writes with per-provider HTTP calls, and a per-provider retry would re-mutate the route N times. Splitting the two concerns fixes that structurally:

- **Exactly-once K8s mutations.** `RouteState` is the single owner of finalizer/annotation writes. The reconciler calls each of its methods at most once per concern per reconcile, and controller-runtime guarantees a single active reconcile per object key — so no channels or locks are needed.
- **Broadcast DNS fan-out.** `dns.Manager` iterates all instances, applies each instance's upsert policy, and joins errors — the reconciler stays backend-agnostic and policy-agnostic.
- **Testability.** The manager is testable with mock providers (no HTTP), and `RouteState` is testable with a fake client (no DNS).

### Why `APIReader` instead of `Client` for reads?

`RouteState` uses the cache-bypassing `APIReader` for reads inside its conflict-retry loops. This ensures it sees the latest state of the HTTPRoute after another controller or process might have modified it, preventing stale-cache conflicts on the finalizer/annotation update steps.

### Why no `init()` for provider registration?

The original design used `init()` + a registry (standard pluggable Go pattern), but this has maintenance costs: new providers require importing into an aggregation package just to register. The current approach — an explicit factory registry (`factories` in `internal/app`) keyed by provider type, resolved against each configured instance — is more explicit and testable, with zero hidden side effects. `main.go` stays free of provider logic: it only starts the app.

### Why checksum annotations on ConfigMaps?

The Helm chart adds a `checksum/config` annotation to the pod spec based on the config ConfigMap's content hash. This triggers a rolling restart whenever the config changes — essential because the controller reads config only at startup (no hot-reload).

## Adding a New Provider

Three steps, no changes to the controller needed:

**1. Implement the `Provider` interface:**
```go
package myprovider

import (
    "context"
    "github.com/yuriy-kovalchuk/yk-dns-manager/internal/dns"
)

func New(log logr.Logger, settings map[string]string, creds *dns.Credentials) (*Provider, error) {
    // settings: non-secret connection parameters from the config file.
    // creds: raw data of the Secret named by the instance's `secret` field;
    // pick out the keys this provider expects and fail construction with an
    // error naming any missing key. Different providers need different keys:
    //   token:    creds.SecretKey("API_TOKEN")
    //   basic:    creds.SecretKey("USERNAME"), creds.SecretKey("PASSWORD")
    //   opnsense: creds.SecretKey("API_KEY"), creds.SecretKey("API_SECRET")
    // nil when the instance references no secret (providers without credentials).
}

type Provider struct { /* ... */ }

func (p *Provider) Exists(ctx context.Context, hostname, recordType string) (bool, error) { /* ... */ }
func (p *Provider) Create(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Update(ctx context.Context, record dns.Record) error                   { /* ... */ }
func (p *Provider) Delete(ctx context.Context, hostname, recordType string) error          { /* ... */ }
func (p *Provider) Upsert(ctx context.Context, record dns.Record) error                   { /* ... */ }
```

**2. Register the type in `internal/app` (`factories`):**
```go
var factories = map[string]providerFactory{
    "opnsense": func(log logr.Logger, settings map[string]string, creds *dns.Credentials) (dns.Provider, error) {
        return opnsense.New(log, settings, creds)
    },
    "myprovider": func(log logr.Logger, settings map[string]string, creds *dns.Credentials) (dns.Provider, error) {
        return myprovider.New(log, settings, creds)
    },
}
```

**3. Configure instances under `providers:` in the config file** — the instance name (or its explicit `provider` field) selects the type:
```yaml
providers:
  primary:
    provider: myprovider   # optional; defaults to the instance name
    upsert: true
    secret: myprovider-creds   # K8s Secret name; nil for providers without credentials
    settings: { ... }
  standby:
    provider: myprovider
    upsert: false
    secret: myprovider-creds
    settings: { ... }
```
