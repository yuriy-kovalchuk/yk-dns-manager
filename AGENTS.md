# yk-dns-manager

## Overview
A Kubernetes controller built with `controller-runtime` that watches Gateway API HTTPRoute resources and automatically manages DNS A records on one or more pluggable backends. When an HTTPRoute with a domain-map-matched hostname is created/updated, the controller creates or updates the corresponding DNS record on **every** configured provider instance (broadcast). On deletion, a finalizer (`dns.yk/cleanup`) triggers cleanup of stale DNS records.

## Architecture
Single-reconciler design: one `HTTPRouteReconciler` handles all HTTPRoutes and is pure orchestration — no K8s writes or HTTP calls of its own. It delegates to two components:
- **`RouteState`** (`internal/controller/routestate.go`) — the *only* code that mutates Kubernetes (finalizer add/remove + `dns.yk/managed-hostnames` annotation). Exactly-once per reconcile; uses `APIReader` (cache-bypassing) for reads inside conflict-retry loops.
- **`dns.Manager`** (`internal/dns/manager.go`) — fans record operations out to all provider instances, applying each instance's upsert policy and joining per-instance errors.

Feature-based package layout — no handler/service/repository split. Domain separation exists at three boundaries: (1) the `Provider` interface in `internal/dns/` abstracts all DNS-specific logic, (2) `dns.Manager` isolates fan-out/policy from the reconciler, and (3) the config package (`config.DomainMap`, `config.ProviderInstance`) is pure structs with no business logic. `main.go` is bootstrap-only (flags → config → `app.Build` → manager); provider assembly, credential Secret reads, and startup health checks live in `internal/app`. Providers are pure HTTP backends and never touch Kubernetes.

## Design Decisions
- **`RouteState` + `dns.Manager` split for multi-provider:** K8s mutations must happen exactly once even when N provider instances exist (and can each fail/retry independently). Centralizing writes in `RouteState` and fan-out in `dns.Manager` makes that structural, not accidental. No channels/locks needed — controller-runtime guarantees one active reconcile per object key.
- **Broadcast fan-out to all instances:** Every managed record is applied to every configured provider instance (e.g. two OPNsense appliances). Domain-partitioned providers would be a later enhancement if needed.
- **Empty provider list = no-op mode:** Zero configured instances is a valid state, not a startup error. The controller watches HTTPRoutes and maintains finalizers/annotations, but `dns.Manager` operations are no-ops until instances are configured.
- **Provider factory registry over `init()` registry:** the `factories` map in `internal/app` is an explicit registry of provider *types*; each configured instance (under `providers:`) is resolved against it. Unknown types or init failures fail startup with a clear error — no silent skipping, no blank-import side effects.
- **Feature folders, not layer-first:** One reconciler + one domain means layered separation adds indirection without benefit. The `Provider` interface and `Manager` are already clean boundaries.
- **No hot-reload for config files:** Config is read once at startup. Helm chart uses checksum annotations on ConfigMaps to trigger rolling restarts when content changes. A file watcher could be added later if needed.
- **Non-upsert mode with drift warning:** `upsert` is per instance. When an instance has `upsert: false`, the manager skips existing records but logs a clear warning that IPs may drift if the domain map changes. This is intentional — some users want exactly-once semantics for cost reasons.

## Development Commands

| Command | Description |
|---------|-------------|
| `make build` | Compile binary to `bin/yk-dns-manager` with ldflags |
| `make test` | All tests (unit + integration) |
| `make test-unit` | Unit tests only, coverage profile |
| `make test-integration` | Integration tests with fake HTTP server |
| `make test-cover` | Coverage report (HTML at `coverage.html`) |
| `make run` | Build and run locally (sources `.env`) |
| `make kind-up` | Start local kind cluster |
| `make kind-load` | Build image and push it into kind |
| `make kind-deploy` | Helm install/upgrade with chart defaults + local image (`VALUES=my-values.yaml` optional) |
| `make kind-reload` | Push image into kind + rolling restart (fast code-iteration loop) |
| `make kind-down` | Remove kind cluster |
| `make lint` | golangci-lint (`continue-on-error: true` in CI) |
| `make docker-build` | Multi-platform build (no push) |
| `make docker-push` | Multi-platform build + push to GHCR |

## Key Patterns
- **Providers are pure backends behind `dns.Manager`.** Nothing outside `internal/app` imports provider packages directly. Adding a new backend type: implement `Provider`, register the type in the `factories` map in `internal/app`, add an instance under `providers:` in the config (instance name or explicit `provider` field selects the type).
- **All K8s mutations go through `RouteState`.** If you find yourself writing `client.Update`/`Patch` anywhere else for a managed route, move it into `RouteState`.
- **Domain map walk-up matching:** exact → wildcard at each parent level → nil. One controller serves multiple domains/ILBs via one domain map file.
- **Finalizer cleanup is best-effort.** Individual DNS deletion failures are logged but never block finalizer removal — the HTTPRoute must always be deletable.
- **Credentials never live in the config file.** Each provider instance names a Kubernetes `Secret` (`secret:` field); `internal/app` reads it via the API at startup (`loadCredentials`) and passes the raw data to the provider's constructor. Each provider declares and validates the keys it expects itself (OPNsense: `API_KEY`/`API_SECRET`) — the mechanism is key-agnostic, so providers can require different credential shapes. Namespace: `secretsNamespace` config field, else the pod's own namespace. The chart grants `secrets: [get]` restricted via `resourceNames` to exactly the referenced Secret names.
