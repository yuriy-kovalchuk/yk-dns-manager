# Testing

This document describes the test suite for yk-dns-manager. All tests run via `go test ./...` (or `make test`).

## Overview

| Layer | Location | Count | What it covers |
|---|---|---|---|
| Unit | `internal/*/` | 21 | Config parsing, provider init, controller logic |
| Integration | `test/integration/` | 8 | OPNsense provider against an in-process fake HTTP server |
| E2E | _(not yet implemented)_ | — | Full flow: K8s cluster + real/fake OPNsense appliance |

## Unit Tests

Unit tests live alongside the code they test. They use no network, no external processes, and run in milliseconds.

### Config — `internal/config/`

**`config_test.go`**

| Test | Description |
|---|---|
| `TestLoadDomainMap` | Loads a YAML domain map file and verifies entries are parsed correctly |
| `TestLookupIP` | Verifies IP lookup for hostnames, including nested subdomains and trailing dots |

**`provider_test.go`**

| Test | Description |
|---|---|
| `TestLoadProviderConfig` | Loads a valid provider config and checks all fields |
| `TestLoadProviderConfig_UpsertTrue` | Verifies `upsert: true` is parsed as a top-level bool |
| `TestLoadProviderConfig_UpsertDefault` | Verifies upsert defaults to `false` when omitted |
| `TestLoadProviderConfig_MissingProvider` | Expects error when `provider` field is missing |
| `TestLoadProviderConfig_EnvVarExpansion` | Verifies `${ENV_VAR}` in settings is resolved via env |
| `TestLoadProviderConfig_EnvVarUnset` | Verifies unset env vars expand to empty string |
| `TestLoadProviderConfig_MissingFile` | Expects error for non-existent config file |

### OPNsense Provider — `internal/dns/opnsense/`

**`opnsense_test.go`**

| Test | Description |
|---|---|
| `TestNew_ValidSettings` | Creates provider with valid settings, checks defaults |
| `TestNew_CustomTTL` | Verifies custom `default_ttl` is parsed |
| `TestNew_InvalidTTL` | Expects error for non-numeric TTL |
| `TestNew_MissingBaseURL` | Expects error when `base_url` is missing |
| `TestNew_MissingAPIKey` | Expects error when `api_key` is missing |
| `TestNew_MissingAPISecret` | Expects error when `api_secret` is missing |
| `TestNew_SkipTLSVerify` | Verifies TLS skip config creates a valid client |

### HTTPRoute Controller — `internal/controller/`

**`httproute_controller_test.go`**

Uses a mock DNS provider and a fake Kubernetes client to test reconciliation logic without any real cluster or DNS calls.

| Test | Description |
|---|---|
| `TestHTTPRouteReconciler_Reconcile` | Creates a DNS record for a matching hostname (two-pass: finalizer then record) |
| `TestHTTPRouteReconciler_ReconcileUnknownDomain` | Skips hostnames with no domain map entry |
| `TestHTTPRouteReconciler_UpsertEnabled` | Calls `Upsert` instead of `Create` when upsert mode is on |
| `TestHTTPRouteReconciler_CreateSkipsExisting` | Skips creation when record already exists and upsert is off |
| `TestHTTPRouteReconciler_Deletion` | Deletes DNS records when HTTPRoute is deleted (finalizer cleanup) |

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
| `TestDeleteNonExistent` | Expects error when deleting a record that doesn't exist |
| `TestUpsertCreatesAndUpdates` | First upsert creates, second upsert updates the same record |
| `TestFullLifecycle` | End-to-end: Exists(false) -> Create -> Exists(true) -> Update -> verify -> Delete -> Exists(false) |
| `TestMultipleRecords` | Creates 3 records, deletes one, verifies others remain unaffected |

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
