# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com),
and this project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added
- **Single config file.** The app now reads one YAML file (`CONFIG_PATH` / `--config-path`) containing both the `domainMap` and the `providers` sections. Replaces the previous `DOMAIN_MAP_PATH` + `DNS_PROVIDER_PATH` two-file setup.
- **Multiple DNS provider instances.** The `providers:` section is a map; every configured instance (any mix of provider types) is initialized at startup and receives every managed record (broadcast). Per-instance `upsert` policy.
- **No-op mode.** An empty provider list is a valid configuration: the controller starts without failure and watches HTTPRoutes without managing any records (Helm default).
- `internal/controller/routestate.go` — `RouteState` component that owns all Kubernetes mutations (finalizer add/remove, managed-hostnames annotation), guaranteeing exactly-once K8s writes per reconcile regardless of provider count.
- `internal/dns/manager.go` — `dns.Manager` that fans record operations out to all provider instances, applies each instance's upsert policy, and joins per-instance errors (`errors.Join`, instance name prefixed).
- Helm chart: `dnsProviders` values (map of instances), single `-config` ConfigMap (`config.yaml`) mounted at one path, namespace-scoped `Role`/`RoleBinding` granting `secrets: [get]` restricted via `resourceNames` to exactly the Secret names referenced in values (rendered only when an instance sets `secret`). `domainMap` + `dnsProviders` values still render into that one file.
- **Credential Secrets read via the Kubernetes API.** Each provider instance names its credential Secret (`secret:` field); `internal/app` reads it at startup (`loadCredentials`) and passes the raw data to the provider constructor. Each provider declares and validates the keys it expects itself (OPNsense: `API_KEY`/`API_SECRET`) — the mechanism is key-agnostic, so different providers can require different credential shapes (token, username/password, ...). Credentials never appear in the config file and are never injected as env vars.
- `secretsNamespace` config field: namespace to read credential Secrets from; in-cluster the app defaults to its own namespace, locally you point it at the namespace where the Secret lives.
- Unit tests for `dns.Manager` fan-out/error-joining, the multi-instance provider config, and credential Secret resolution (fake clientset).
- `internal/app` package — provider factory registry, credential Secret resolution, namespace resolution, and startup health checks moved out of `main.go`. `cmd/yk-dns-manager/main.go` is now bootstrap-only (flags → config → `app.Build` → manager start).
- Regression tests for every previously known reconciler bug (B1–B3, B13): deletion with unmapped hostnames, deletion covering annotation ∪ spec, live cleanup when a route loses its last mapped hostname, duplicate spec hostnames.
- Integration tests for disabled-override re-enable (B9) and reconfigure retry behaviour (B5).
- Makefile: `kind-load` / `kind-deploy` split (image push and chart deploy are now independent steps) and an optional `VALUES=my-values.yaml` parameter on `kind-deploy`; `kind-reload` as the fast code-iteration loop.
- `docs/providers.md` — per-provider reference: the common mechanism (settings, credential Secrets, key-agnostic validation), OPNsense configuration and behavior notes, and the steps for adding a new provider.

### Changed
- **BREAKING:** `${ENV_VAR}` expansion in config files is removed — settings values are used literally. Credentials are not written in the config file at all; they come from the credential Secret referenced by each instance (see Added).
- **BREAKING:** config file schema replaced: the old `domain-map.yaml` and `dns-provider.yaml` files are gone; use one file with `domainMap:` + `providers: <name>: {provider, upsert, secret, settings}`. The `provider` field selects the type and defaults to the instance name. Old-format content (unknown top-level keys) is rejected at startup.
- **BREAKING:** Helm values: `dnsProvider.*` → `dnsProviders.<name>.*`. The chart default is now an empty map (no-op mode); example instances are provided as comments.
- Provider selection is now fail-fast: an unknown provider type, a failed instance init, a missing/unreadable credential Secret, or an old-format config file (unknown top-level keys are rejected) aborts startup with a clear error instead of silently skipping — unless the provider list is empty, which is valid.
- `HTTPRouteReconciler` slimmed to orchestration: `Client`/`APIReader`/`DNS`/`Upsert` fields replaced by `State *RouteState` and `DNS *dns.Manager`.
- **Reconcile restructured** (`httproute_controller.go`): deletion is now handled first and independently of the domain map, deleting records for the **annotation ∪ mapped spec** set; the live path deletes records for annotated hostnames that left the spec and clears the annotation when the managed set becomes empty. The `managed-hostnames` annotation is the single source of truth for what the controller manages.
- OPNsense provider: disabled host overrides now count as **absent** — `Create` re-enables a disabled override in place (no duplicate), `Update`/`Upsert` re-enable it. `reconfigure` is retried (3 attempts, 500 ms backoff) so a transient failure can't strand a persisted-but-unapplied change in non-upsert mode.
- Code simplification: removed `FormatHTTPRoute` (dead code), the hand-rolled `Contains` helper (replaced by `slices.Contains`), unused `dns.Err*` sentinel variables, and the `default_ttl` setting / `Record.TTL` field (the OPNsense host-override API has no TTL — the setting was misleading). `internal/config` is now a single file. `hack/local-values.yaml` removed — `make kind-deploy` uses chart defaults + `--set` local overrides (+ optional `VALUES=` file).
- Makefile aligned with the yk-update-checker reference: buildx-based `docker-build`/`docker-push` (`--builder multiplatform`, `:latest` tag), `go tool cover -func` in `test-cover`, `.PHONY` completeness, `chmod +x .githooks/*` in `install-hooks`.
- Dependencies: `k8s.io/{api,apimachinery,client-go}` v0.36.1 → v0.36.3, `sigs.k8s.io/gateway-api` v1.5.1 → v1.6.1, `go-logr/logr` v1.4.3 → v1.4.4, `go.yaml.in/yaml/v3` v3.0.4 → v3.0.5.

### Fixed
- **B1 — HTTPRoute stuck in Terminating forever:** deleting a route whose hostnames no longer match the domain map never removed the finalizer (the `len(specHostnames) == 0` early return sat before the deletion handling). Deletion is now handled before any domain-map filtering.
- **B2 — Leaked DNS record + stale annotation:** a managed route that lost its last mapped hostname was skipped silently. The live path now deletes the orphaned records and clears the annotation.
- **B3 — Deletion used the wrong source of truth:** the deletion loop iterated the current spec only; it now deletes **annotation ∪ spec** so records left behind by B2 are always collected.
- **B5 — Transient reconfigure failure = permanent drift in non-upsert mode:** `reconfigure` is now retried with bounded backoff (3 attempts); a still-failing reconfigure returns an error instead of silently losing the apply step.
- **B9 — `findOverride` ignored the `Enabled` field:** a manually disabled override counted as "exists", so non-upsert mode never re-created/re-enabled it. Disabled overrides are now treated as absent and re-enabled in place.
- **B13 — Spec hostnames weren't deduplicated:** duplicates in `spec.hostnames` produced duplicate record operations. `mappedSpecHostnames` now dedupes.
- Malformed structured log call in the OPNsense delete path (positional args instead of key-value pairs).
