# yk-dns-manager

## Overview
A Kubernetes controller built with `controller-runtime` that watches Gateway API HTTPRoute resources and automatically manages DNS A records on pluggable backends. When an HTTPRoute with a domain-map-matched hostname is created/updated, the controller creates or updates the corresponding DNS record via the configured provider API. On deletion, a finalizer (`dns.yk/cleanup`) triggers cleanup of stale DNS records.

## Architecture
Single-reconciler design: `cmd/yk-dns-manager/main.go` only handles signals and calls `app.Run`, which wires flags, logging, config, provider and manager in `internal/app`. one `HTTPRouteReconciler` handles all HTTPRoutes. Feature-based package layout — no handler/service/repository split since the reconciler is thin (parsing → delegation → JSON). Domain separation exists at two boundaries: (1) the `Provider` interface in `internal/dns/` abstracts all DNS-specific logic, and (2) the config package (`config.Config`, `config.DomainMap`) holds pure structs with no business logic. The controller reads routes from the informer cache, re-reads through `APIReader` only inside the finalizer conflict-retry loops, and writes its annotation as a merge patch.

## Design Decisions
- **Provider factory list over `init()` registry:** `newProviders()` in `internal/dns/providers` iterates configured providers at startup. More explicit than blank-import registration, easier to test and debug. Restore the aggregation pattern only if the provider count grows substantially.
- **Feature folders, not layer-first:** One reconciler + one domain means layered separation adds indirection without benefit. The `Provider` interface is already a clean boundary.
- **One config file, no hot-reload:** provider, settings and domain map live in one YAML file (`--config` / `CONFIG_PATH`), read once at startup. The Helm chart renders it as one ConfigMap with a checksum annotation to trigger rolling restarts when content changes. A file watcher could be added later if needed.
- **Non-upsert mode with drift warning:** When `upsert: false`, the controller skips existing records but logs a clear warning that IPs may drift if the domain map changes. This is intentional — some users want exactly-once semantics for cost reasons.

## Development Commands

| Command | Description |
|---------|-------------|
| `make build` | Compile binary to `bin/yk-dns-manager` with ldflags |
| `make test` | All tests (unit + integration + envtest) |
| `make test-unit` | Unit tests only, coverage profile |
| `make test-integration` | Integration tests with fake HTTP server |
| `make test-envtest` | Real manager against an envtest API server and the fake appliance (downloads binaries to `bin/k8s`) |
| `make test-cover` | Coverage report (HTML at `coverage.html`) |
| `make run` | Run the binary locally against your current kubecontext (sources `.env`, `--leader-elect=false`) |
| `make kind-up` | Start local kind cluster |
| `make kind-deploy` | kind cluster + Gateway API CRDs + image + in-cluster fake appliance + helm install |
| `make kind-down` | Remove kind cluster |
| `make lint` | golangci-lint (blocking in CI) |
| `make docker-build` | Multi-platform build (no push) |
| `make docker-push` | Multi-platform build + push to GHCR |

## Key Patterns
- **Provider interface is the only DNS boundary.** Controllers never import provider packages directly. Adding a new backend: implement `Provider`, register in `newProviders()` in `internal/dns/providers`, set config `provider` field.
- **Domain map matching:** an exact key matches only itself; otherwise the most specific `*.parent` wildcard covering the hostname wins, at any depth. One controller serves multiple domains/ILBs via the `domains` section of the config file.
- **Ownership lives on the DNS record, not the route.** Providers mark every record they write and only change or delete marked records; anything else is a `Conflict`. The mark names the routes that wrote the record (`dns.Record.Owner`), and `Remove` carries the owner, so a route can only release records it wrote. The `dns.yk/managed-hostnames` annotation is writable by any route editor, so it only says which names to release, never what may be deleted. A record shared by several routes is deleted only when none claims it.
- **Every provider runs the conformance suite** (`internal/dns/dnstest`). The contract is pinned there, not in provider-specific tests.
- **The HTTPRoute must always be deletable.** A failing DNS cleanup holds the finalizer and is retried until `--cleanup-grace-period` after the deletion request, then the finalizer is removed anyway. The cleanup is published before the finalizer goes.
- **Only the leader reconciles.** Leader election is on by default (Lease `yk-dns-manager.dns.yk` in the pod's namespace); the chart refuses `replicaCount` > 1 without it. `make run` passes `--leader-elect=false`.
- **Report outcomes, not just logs.** New DNS outcomes get an Event on the HTTPRoute and a `yk_dns_manager_*` counter.
- **Config values use `${ENV_VAR}` expansion** in YAML for credential injection from Kubernetes Secrets or local `.env` files. Only `${NAME}` is expanded, and an unset variable fails startup. Config is strict: unknown fields and unknown provider settings keys (listed per factory in `newProviders()`) fail startup.
