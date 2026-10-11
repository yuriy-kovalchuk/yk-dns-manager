# Testing

All tests run via `go test ./...` (or `make test`). CI runs `make vet build test lint` and `make vuln` on every push and pull request, and both are blocking.

## Overview

| Layer | Location | Count | What it covers |
|---|---|---|---|
| Unit | `cmd/`, `internal/` | 45 | Config parsing and validation, provider construction and TLS, controller reconcile logic, log-level handling, the fake appliance's contract |
| Integration | `test/integration/` | 14 | The real OPNsense provider against an in-process fake appliance API over HTTP |
| Envtest | `test/envtest/` | 5 + 8 pending | The real manager, predicates and provider against a real API server and the fake appliance |
| E2E | manual | — | Full flow in a kind cluster with the fake appliance; see [docs/local-testing.md](local-testing.md) |

There are no mocking frameworks. Tests use plain structs implementing `dns.Provider` and `sigs.k8s.io/controller-runtime/pkg/client/fake`.

## Unit tests

### Entrypoint — `cmd/yk-dns-manager/main_test.go`

| Test | Description |
|---|---|
| `TestParseLogLevel` | Named levels, numeric logr verbosity (`-3` means `V(3)`), and rejects garbage |
| `TestDebugLevelEnablesVerbosity` | `debug` actually turns `V(1)` output on |
| `TestDebugLevelKeepsProductionEncoder` | Debug does not switch zap to development mode |
| `TestSelectProviderSkipsFailedFactories` | A failing factory does not stop a later one from being chosen |
| `TestSelectProviderFailsWhenNoneInitialize` | No provider selected returns an error and a nil interface |
| `TestSelectProviderDoesNotKeepTypedNilProvider` | A factory returning `(nil, err)` must not leave a typed-nil provider behind — this is what used to panic on startup |

### Config — `internal/config/`

| Test | Description |
|---|---|
| `TestLoadDomainMap` | Parses a YAML domain map |
| `TestLoadDomainMapValidation` | Rejects non-IPv4 values, empty maps, empty domain keys and duplicate YAML keys |
| `TestLoadDomainMapNormalizesValues` | Values are canonicalised through `netip` |
| `TestLookupIP` | Exact and nested subdomain lookup, trailing dots |
| `TestLookupIPWildcard` | Wildcard entries match subdomains |
| `TestLookupIPWildcardWithBaseDomain` | Walk-up matching falls back from wildcard to base domain |
| `TestLoadProviderConfig` | Parses a valid provider config |
| `TestLoadProviderConfig_UpsertTrue` | `upsert: true` is a top-level bool |
| `TestLoadProviderConfig_UpsertDefault` | `upsert` defaults to `false` |
| `TestLoadProviderConfig_MissingProvider` | Errors when `provider` is absent |
| `TestLoadProviderConfig_EnvVarExpansion` | `${ENV_VAR}` in settings resolves from the environment |
| `TestLoadProviderConfig_EnvVarUnset` | Unset variables expand to the empty string |
| `TestLoadProviderConfig_MissingFile` | Errors for a non-existent file |

### OPNsense provider — `internal/dns/opnsense/`

| Test | Description |
|---|---|
| `TestNew_ValidSettings` | Builds a client from valid settings |
| `TestNew_CustomTTL` | `default_ttl` is parsed |
| `TestNew_InvalidTTL` | Non-numeric TTL is an error |
| `TestNew_MissingBaseURL` / `_MissingAPIKey` / `_MissingAPISecret` | Required settings are enforced |
| `TestNew_SkipTLSVerify` | `skip_tls_verify` produces a client with verification off and the TLS floor set |
| `TestNew_SkipTLSVerifyWarns` | Enabling it logs a warning naming `ca_file` as the fix; a default config must not warn |
| `TestNew_TLSVerification` | Stands up HTTPS on a throwaway CA: system roots reject it, `ca_file` accepts it, `skip_tls_verify` accepts it |
| `TestNew_TLSConfigErrors` | Missing `ca_file`, non-PEM file, and `ca_file` together with `skip_tls_verify` |
| `TestResponseBodyLimit` | A body over the cap is reported as a limit error, not as truncated JSON |
| `TestErrorBodyTruncated` | Error bodies are capped rather than dumped whole into the log |

### Fake appliance — `internal/testenv/`

The fake is only useful while it behaves like OPNsense, so its contract is pinned too.

| Test | Description |
|---|---|
| `TestFakeRequiresCredentialsWhenConfigured` | With `RequireCredentials`, missing or wrong basic auth gets 401 |
| `TestFakeUnknownUUIDAnswers200WithResult` | Unknown uuids get HTTP 200 with `result: failed` (set) or `not found` (del), not a 404 |
| `TestFakeValidatesRows` | Rows the appliance would reject get `result: failed` with `validations` |
| `TestFakeWritesRequirePOST` | Write endpoints ignore other methods |
| `TestFakePublishesOnlyOnReconfigure` | Writes are staged; only a successful reconfigure publishes them |
| `TestFakeSeedIsPublished` | Seeded rows count as configuration that was already live |

### HTTPRoute controller — `internal/controller/httproute_controller_test.go`

Uses a mock `dns.Provider` and a fake Kubernetes client, so there is no cluster and no DNS traffic.

| Test | Description |
|---|---|
| `TestHTTPRouteReconciler_Reconcile` | Adds the finalizer, then creates a record for a matching hostname |
| `TestHTTPRouteReconciler_ReconcileUnknownDomain` | Hostnames with no domain map entry are skipped |
| `TestHTTPRouteReconciler_UpsertEnabled` | Calls `Upsert` instead of `Create` when upsert is on |
| `TestHTTPRouteReconciler_CreateSkipsExisting` | Skips existing records when upsert is off |
| `TestHTTPRouteReconciler_Deletion` | Finalizer cleanup deletes the records |
| `TestHTTPRouteReconciler_DeletionWithoutMatchingHostname` | A route whose hostnames no longer match the domain map is still deletable — the orphaned-finalizer bug |
| `TestHTTPRouteReconciler_CleanupOfUnownedHostnames` | Hostnames dropped from the spec or from the domain map are cleaned up from the ownership annotation; corrupt annotations do not wedge the route |
| `TestHTTPRouteReconciler_FlushFailureRequeues` | A failed `Flush` is returned so controller-runtime requeues instead of silently dropping the change |

## Integration tests

`test/integration/opnsense_test.go` drives the real provider over real HTTP against `internal/testenv.OPNsense`, the same in-memory appliance `make fake-opnsense` serves. This is where wire format is pinned: JSON shapes, URL construction, status handling and response parsing.

The fake follows the appliance where the provider depends on it. Writes are staged (`Snapshot`) and only published (`Published`) by reconfigure. Unknown uuids get HTTP 200 with a result body. Invalid rows are rejected with `validations`. In these tests it requires the credentials the provider sends. `SetFailure` makes an endpoint return an error status, to test failure paths.

```bash
make test-integration
```

| Test | Description |
|---|---|
| `TestCreateAndExists` | Create, then `Exists`, then inspect every stored field |
| `TestUpdateExistingRecord` | Update changes the IP in place |
| `TestUpdateNonExistent` | Updating a missing record is an error |
| `TestDeleteExistingRecord` | Delete removes it and `Exists` goes false |
| `TestDeleteNonExistent` | Deleting a missing record is **not** an error |
| `TestUpsertCreatesAndUpdates` | First upsert creates, second updates the same row |
| `TestFullLifecycle` | Exists → Create → Exists → Update → verify → Delete → Exists |
| `TestMultipleRecords` | Independent records stay independent through a delete |
| `TestDeleteRemovesAllDuplicates` | Every duplicate override for a hostname is deleted, not just the first |
| `TestUpsertConvergesDuplicates` | Writing to a hostname with duplicates leaves exactly one row, holding the new value |
| `TestUpsertUpdatePathCallCount` | An update-path `Upsert` costs one list fetch plus one write, and `Flush` issues exactly one `reconfigure`; an idle `Flush` issues none |
| `TestHealthCheckRejectsWrongCredentials` | A 401 from the appliance surfaces as an authentication failure |
| `TestCreateSurfacesValidationFailure` | A row the appliance rejects (HTTP 200, `result: failed`) is an error, not a success |
| `TestFlushPublishes` | Writes go live only on `Flush`; a failed `Flush` stays pending and a retry publishes |

## Envtest

`test/envtest/` starts a real API server and etcd (controller-runtime envtest) with the Gateway API CRDs from the module go.mod pins. Each test starts its own manager, running the real reconciler and OPNsense provider against its own fake appliance and watching its own namespace. This covers what the controller unit tests cannot, because they call `Reconcile` directly on a fake client: the event predicates, the finalizer → reconcile chain, and the API server's generation and finalizer semantics.

```bash
make test-envtest   # downloads pinned control-plane binaries into bin/k8s, then runs the suite
```

Without `KUBEBUILDER_ASSETS` the suite skips, so a plain `go test ./...` (and the pre-push hook) still works without the binaries. `make test`, and so CI, always runs it.

| Test | Description |
|---|---|
| `TestCreatePublishesRecord` | In both modes, the finalizer is added, the record is published, and the route records ownership |
| `TestHostnameChangeMovesRecord` | Changing a hostname publishes the new record and removes the old one |
| `TestDeleteRemovesRecordAndReleasesRoute` | Deleting the route unpublishes the record and releases the finalizer |
| `TestUnmatchedRouteIsIgnored` | A route the domain map does not cover gets no finalizer and no appliance writes |
| `TestRouteIsDeletableWhileApplianceFails` | A route is still deleted while every appliance call fails |

### Pending findings

`test/envtest/findings_test.go` pins findings from the 2026-10-12 audit (finding IDs such as S1 and B2) that are not fixed yet. Each test describes the correct behaviour, fails on the current code, and is skipped through `pending()`. The change that fixes a finding deletes its `pending` call. To run them all:

```bash
ENVTEST_RUN_PENDING=1 make test-envtest
```

| Test | Finding | Roadmap phase |
|---|---|---|
| `TestAnnotationCannotDeleteForeignRecords` | S1 | 0 |
| `TestUnchangedRouteCausesNoWrites` | B2 | 2 |
| `TestDisabledManagedRecordIsReenabled` | B8 | 2 |
| `TestUnmanagedRecordIsNotOverwritten` | S2 | 3 |
| `TestUnmanagedRecordSurvivesRouteDeletion` | S2 | 3 |
| `TestSharedHostnameSurvivesOneRouteDeletion` | B1 | 3 |
| `TestDeleteIsPublishedAfterReconfigureRecovers` | B9 | 4 |
| `TestUnacceptedRouteGetsNoRecord` | S3 | 4 |

## End-to-end

Not automated in CI. The manual path is documented in [docs/local-testing.md](local-testing.md):

```bash
make kind-deploy   # cluster + image + fake appliance + controller
make kind-demo     # apply hack/local/httproute.yaml
```

The controller logs `upserted DNS record hostname=app.example.com ip=10.0.0.1`
followed by one `reconfigure completed`, and `GET /records` on the fake shows the
row. Automating this as `make test-e2e` is open work.

## Running tests

```bash
make test              # unit + integration + envtest
make test-envtest      # envtest only
make test-unit         # unit only, with a coverage profile
make test-cover        # function summary + coverage.html
make test-integration  # integration only
make vuln              # govulncheck, same as CI

go test -race ./...    # race detector
go test ./internal/config/ -v
go test -run TestLoadDomainMapValidation ./internal/config/ -v
```
