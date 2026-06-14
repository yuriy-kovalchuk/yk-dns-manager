# yk-dns-manager

## Overview
A Kubernetes controller built with `controller-runtime` that watches Gateway API HTTPRoute resources and automatically manages DNS A records on pluggable backends. When an HTTPRoute with a domain-map-matched hostname is created/updated, the controller creates or updates the corresponding DNS record via the configured provider API. On deletion, a finalizer (`dns.yk/cleanup`) triggers cleanup of stale DNS records.

## Architecture
Single-reconciler design: one `HTTPRouteReconciler` handles all HTTPRoutes. Feature-based package layout — no handler/service/repository split since the reconciler is thin (parsing → delegation → JSON). Domain separation exists at two boundaries: (1) the `Provider` interface in `internal/dns/` abstracts all DNS-specific logic, and (2) config packages (`config.DomainMap`, `config.ProviderConfig`) are pure structs with no business logic. The controller uses `APIReader` (cache-bypassing) for reads inside conflict-retry loops to ensure read-after-write consistency on finalizer updates.

## Design Decisions
- **Provider factory list over `init()` registry:** `newProviders()` in `main.go` iterates configured providers at startup. More explicit than blank-import registration, easier to test and debug. Restore the aggregation pattern only if the provider count grows substantially.
- **Feature folders, not layer-first:** One reconciler + one domain means layered separation adds indirection without benefit. The `Provider` interface is already a clean boundary.
- **No hot-reload for config files:** Config is read once at startup. Helm chart uses checksum annotations on ConfigMaps to trigger rolling restarts when content changes. A file watcher could be added later if needed.
- **Non-upsert mode with drift warning:** When `upsert: false`, the controller skips existing records but logs a clear warning that IPs may drift if the domain map changes. This is intentional — some users want exactly-once semantics for cost reasons.

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
| `make kind-deploy` | Build image, load into kind, helm install |
| `make kind-down` | Remove kind cluster |
| `make lint` | golangci-lint (`continue-on-error: true` in CI) |
| `make docker-build` | Multi-platform build (no push) |
| `make docker-push` | Multi-platform build + push to GHCR |

## Key Patterns
- **Provider interface is the only DNS boundary.** Controllers never import provider packages directly. Adding a new backend: implement `Provider`, register in `newProviders()`, set config `provider` field.
- **Domain map walk-up matching:** exact → wildcard at each parent level → nil. One controller serves multiple domains/ILBs via one domain map file.
- **Finalizer cleanup is best-effort.** Individual DNS deletion failures are logged but never block finalizer removal — the HTTPRoute must always be deletable.
- **Config values use `${ENV_VAR}` expansion** in YAML for credential injection from Kubernetes Secrets or local `.env` files.
