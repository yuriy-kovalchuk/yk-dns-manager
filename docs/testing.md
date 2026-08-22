# Testing

This document describes the test suite for yk-dns-manager. All tests run via `go test ./...` (or `make test`).

## Overview

| Layer | Location | Count | What it covers |
|---|---|---|---|
| Unit | `internal/*/` | 59 (incl. subtests) | Config parsing, app assembly + secret loading, manager fan-out, controller logic, provider init |
| Integration | `test/integration/` | 11 | OPNsense provider against an in-process fake HTTP server |
| E2E | _(not yet implemented)_ | — | Full flow: K8s cluster + real/fake OPNsense appliance |

## Unit Tests

Unit tests live alongside the code they test. They use no network, no external processes, and run in milliseconds.

### Config — `internal/config/`

**`config_test.go`**

| Test | Description |
|---|---|
| `TestLoadConfig` | Loads the unified config file (`domainMap` + `providers`) and checks both sections |
| `TestLoadConfig_EmptyFile` | An empty file is a valid no-op config |
| `TestLookupIP` (+ `Wildcard`, `WildcardWithBaseDomain`) | Verifies IP lookup: exact match, wildcard, and exact-beats-wildcard |

**`provider_test.go`**

| Test | Description |
|---|---|
| `TestLoadConfig_Providers` | Loads a multi-instance config (two types) and checks all fields |
| `TestLoadConfig_ProvidersUpsertDefault` | Verifies per-instance `upsert` defaults to `false` when omitted |
| `TestLoadConfig_EmptyProviders` | Empty/missing `providers` map is valid (no-op mode) |
| `TestLoadConfig_OldFormatRejected` | The legacy two-file format (unknown top-level keys) is rejected by strict decode |
| `TestLoadConfig_MissingFile` | Expects error for non-existent config file |

Note: credential handling is tested in the provider package — the provider reads its declared keys from the `dns.Credentials` set and fails with an error naming any missing key (see `TestNew_MissingCredentialKeys` below).

### App Assembly — `internal/app/`

**`app_test.go`**

Uses a fake Kubernetes clientset to test `app.Build` (provider construction, credential Secret resolution, namespace resolution) without a real cluster.

| Test | Description |
|---|---|
| `TestLoadCredentials` | No `secret` field → nil; existing Secret → raw data passed through; missing Secret → error naming the Secret |
| `TestPodNamespaceFromEnv` | `POD_NAMESPACE` env var is used for in-cluster namespace resolution |
| `TestResolveSecretNamespace` | Explicit `secretsNamespace` wins; empty when no secret is referenced |
| `TestBuild_NoProviders` | Zero instances → empty manager (no-op mode), no error |
| `TestBuild_UnknownProviderType` | Unknown type → startup error |
| `TestBuild_MissingSecret` | Referenced Secret absent from the cluster → startup error |
| `TestBuild_ProviderNeedsSecret` | OPNsense instance without a `secret` field → startup error (provider validates its keys) |

### DNS Manager — `internal/dns/`

**`manager_test.go`**

Uses mock `Provider` implementations to verify fan-out without any HTTP.

| Test | Description |
|---|---|
| `TestManager_EnsureRecord_Fanout` | Record is applied to all instances (non-upsert → Create, upsert → Upsert) |
| `TestManager_EnsureRecord_SkipsExistingNonUpsert` | Existing record + `upsert: false` → no op on that instance |
| `TestManager_EnsureRecord_JoinsErrors` | One failing instance → joined error names it; other instances still processed |
| `TestManager_DeleteRecord_FanoutAndJoinErrors` | Delete fans out to all instances; errors joined and named |
| `TestManager_HealthCheck_AllMustPass` | Any failing instance fails the health check; all healthy → nil |
| `TestManager_Len` | Instance count reporting |

### OPNsense Provider — `internal/dns/opnsense/`

**`opnsense_test.go`**

| Test | Description |
|---|---|
| `TestNew_ValidSettings` | Creates provider with valid settings + secret, checks parsed fields |
| `TestNew_MissingBaseURL` | Expects error when `base_url` is missing |
| `TestNew_NoCredentials` | Expects error when no `dns.Credentials` are provided (points at the `secret` config field) |
| `TestNew_MissingCredentialKeys` | Expects an error naming the missing Secret key (e.g. `API_SECRET`) |
| `TestNew_CredentialKeyWithWhitespace` | Trims whitespace from Secret values (file-sourced secrets) |
| `TestNew_SkipTLSVerify` | Verifies TLS skip config creates a valid client |

### HTTPRoute Controller — `internal/controller/`

**`httproute_controller_test.go`**

Uses a mock DNS provider (wrapped in a real `dns.Manager`) and a fake Kubernetes client to test reconciliation logic without any real cluster or DNS calls.

| Test | Description |
|---|---|
| `TestHTTPRouteReconciler_Reconcile` | Creates a DNS record for a matching hostname (two-pass: finalizer then record) |
| `TestHTTPRouteReconciler_ReconcileUnknownDomain` | Unmanaged routes are never touched (no records, no finalizer) |
| `TestHTTPRouteReconciler_UpsertEnabled` | Calls `Upsert` instead of `Create` when the instance's upsert policy is on |
| `TestHTTPRouteReconciler_CreateSkipsExisting` | Skips creation when record already exists and upsert is off |
| `TestHTTPRouteReconciler_Deletion` | Deletes DNS records when HTTPRoute is deleted (finalizer cleanup) |
| `TestHTTPRouteReconciler_LosesAllMappedHostnames` | A managed route whose hostnames no longer map gets its record deleted and annotation cleared |
| `TestHTTPRouteReconciler_DeletionWithUnmappedHostnames` | Deleting a route with unmapped hostnames still removes the finalizer (never stuck in Terminating) |
| `TestHTTPRouteReconciler_DeletionUnion` | Deletion covers annotation ∪ spec, not just the spec |
| `TestHTTPRouteReconciler_DuplicateSpecHostnames` | Duplicate spec hostnames produce a single record |

## Integration Tests

Integration tests live in `test/integration/`. They spin up a real HTTP server (using `httptest.NewServer`) with in-memory OPNsense-like handlers and exercise the real provider code over HTTP.

```
go test ./test/integration/ -v
```

**`opnsense_test.go`**

| Test | Description |
|---|---|
| `TestCreateAndExists` | Creates a record, verifies it exists, inspects stored data fields |
| `TestUpdateExistingRecord` | Creates then updates a record, verifies the IP changed in the store |
| `TestUpdateNonExistent` | Expects error when updating a record that doesn't exist |
| `TestDeleteExistingRecord` | Creates then deletes a record, verifies it's gone |
| `TestDeleteNonExistent` | Deleting a missing record is not an error (idempotent) |
| `TestUpsertCreatesAndUpdates` | First upsert creates, second upsert updates the same record |
| `TestFullLifecycle` | End-to-end: Exists(false) -> Create -> Exists(true) -> Update -> verify -> Delete -> Exists(false) |
| `TestMultipleRecords` | Creates 3 records, deletes one, verifies others remain unaffected |
| `TestDisabledOverrideReenabled` | A manually disabled override counts as absent; `Create` re-enables it in place (no duplicate) |
| `TestReconfigureRetriedOnTransientFailure` | Transient reconfigure failures are retried; `Create` succeeds |
| `TestReconfigureExhausted` | Persistent reconfigure failure → error after the bounded retry count |

## E2E Tests (Planned)

End-to-end tests will validate the full flow in a real Kubernetes environment:

1. Deploy yk-dns-manager to a cluster (kind or real)
2. Point it at a fake OPNsense server (or a test appliance)
3. Create an HTTPRoute resource
4. Verify the DNS record appears on the OPNsense side
5. Update the HTTPRoute hostname
6. Verify the old record is cleaned up and the new one is created
7. Delete the HTTPRoute
8. Verify the DNS record is removed

These will likely use a `kind` cluster and the `fake/` server, run as a separate make target (e.g. `make test-e2e`).

## Running Tests

```bash
# All tests (unit + integration)
make test

# Unit tests only (fast, no network)
make test-unit

# Integration tests only (verbose, with fake HTTP server)
make test-integration

# Specific package
go test ./internal/config/

# With race detector
go test -race ./...
```
