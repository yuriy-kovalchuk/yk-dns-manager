# Testing

All tests run via `make test`; a plain `go test ./...` skips envtest unless `KUBEBUILDER_ASSETS` is set. CI runs `make vet build test lint` and `make vuln` on every push and pull request, and again on every release tag before anything is published; both are blocking.

## Overview

| Layer | Location | Count | What it covers |
|---|---|---|---|
| Unit | `cmd/`, `internal/` | 60 | Config parsing and validation, provider construction and TLS, controller reconcile logic, log-level handling, the fake appliance's contract |
| Integration | `test/integration/` | 11 + conformance | The provider conformance suite and OPNsense-specific checks, against an in-process fake appliance over HTTP |
| Envtest | `test/envtest/` | 17 | The real manager, predicates and provider against a real API server and the fake appliance |
| E2E | manual | — | Full flow in a kind cluster with the fake appliance; see [docs/local-testing.md](local-testing.md) |

There are no mocking frameworks. Tests use plain structs implementing `dns.Provider` and `sigs.k8s.io/controller-runtime/pkg/client/fake`.

## Unit tests

### Entrypoint — `cmd/yk-dns-manager/main_test.go`

| Test | Description |
|---|---|
| `TestParseLogLevel` | Named levels, numeric logr verbosity (`-3` means `V(3)`), and rejects garbage |
| `TestDebugLevelEnablesVerbosity` | `debug` actually turns `V(1)` output on |
| `TestDebugLevelKeepsProductionEncoder` | Debug does not switch zap to development mode |
| `TestSelectProviderPicksByName` | Only the provider the config names is initialized |
| `TestSelectProviderRejectsUnknownName` | A misspelled name fails and lists the available providers |
| `TestSelectProviderReturnsInitError` | The named provider's own error is the startup error |
| `TestSelectProviderRejectsUnknownSettings` | A settings key the provider does not list fails startup (D11) |
| `TestSelectProviderDoesNotKeepTypedNilProvider` | A factory returning `(nil, err)` must not leave a typed-nil provider behind — this is what used to panic on startup |

### Config — `internal/config/`

| Test | Description |
|---|---|
| `TestLoadDomainMap` | Parses a YAML domain map |
| `TestLoadDomainMapValidation` | Rejects non-IPv4 values, empty maps, empty or invalid domain keys, and keys duplicated before or after normalization |
| `TestLoadDomainMapNormalizesValues` | Values are canonicalised through `netip` |
| `TestLoadDomainMapNormalizesKeys` | Keys are lowercased and lose a trailing dot (B12) |
| `TestLookupIP` | An exact key matches only itself, case-insensitively and with a trailing dot (D2) |
| `TestLookupIPWildcard` | Wildcard entries match subdomains |
| `TestLookupIPWildcardWithBaseDomain` | Walk-up matching falls back from wildcard to base domain |
| `TestLoadProviderConfig` | Parses a valid provider config |
| `TestLoadProviderConfig_UpsertTrue` | `upsert: true` is a top-level bool |
| `TestLoadProviderConfig_UpsertDefault` | `upsert` defaults to `false` |
| `TestLoadProviderConfig_MissingProvider` | Errors when `provider` is absent |
| `TestLoadProviderConfig_EnvVarExpansion` | `${ENV_VAR}` in settings resolves from the environment |
| `TestLoadProviderConfig_EnvVarUnset` | A reference to an unset variable fails, naming the setting and the variable (S6) |
| `TestLoadProviderConfig_LiteralDollar` | Only `${NAME}` is expanded; a literal `$` is kept (S6) |
| `TestLoadProviderConfig_UnknownField` | An unknown top-level field fails (D11) |
| `TestLoadProviderConfig_MissingFile` | Errors for a non-existent file |

### OPNsense provider — `internal/dns/opnsense/`

| Test | Description |
|---|---|
| `TestNew_ValidSettings` | Builds a client from valid settings |
| `TestNew_DefaultTTLIgnored` | `default_ttl` is accepted for compatibility but warns that it has no effect |
| `TestNew_MissingBaseURL` / `_MissingAPIKey` / `_MissingAPISecret` | Required settings are enforced |
| `TestNew_SkipTLSVerify` | `skip_tls_verify` produces a client with verification off and the TLS floor set |
| `TestNew_SkipTLSVerifyWarns` | Enabling it logs a warning naming `ca_file` as the fix; a default config must not warn |
| `TestNew_TLSVerification` | Stands up HTTPS on a throwaway CA: system roots reject it, `ca_file` accepts it, `skip_tls_verify` accepts it |
| `TestNew_TLSConfigErrors` | Missing `ca_file`, non-PEM file, and `ca_file` together with `skip_tls_verify` |
| `TestNew_Scheme` | `http` is refused unless `allow_insecure_http` is true (any boolean spelling), warns when allowed, and a non-boolean value or another scheme is refused |
| `TestNew_SkipTLSVerifyIsABoolean` | `skip_tls_verify` accepts `true`, `True`, `1`, `false` and empty, and rejects a non-boolean value (D11) |
| `TestResponseBodyLimit` | A body over the cap is reported as a limit error, not as truncated JSON |
| `TestErrorBodyTruncated` | Error bodies are capped rather than dumped whole into the log |
| `TestCommitChecksReconfigureStatus` | A reconfigure answering a status other than `ok` fails the commit, and the publish stays owed |

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
| `TestHTTPRouteReconciler_UpsertEnabled` | Ensures records with overwrite on when upsert is on |
| `TestHTTPRouteReconciler_CreateSkipsExisting` | Leaves existing records alone when upsert is off |
| `TestHTTPRouteReconciler_Deletion` | Finalizer cleanup deletes the records |
| `TestHTTPRouteReconciler_DeletionWithoutMatchingHostname` | A route whose hostnames no longer match the domain map is still deletable — the orphaned-finalizer bug |
| `TestHTTPRouteReconciler_CleanupOfUnownedHostnames` | Hostnames dropped from the spec or the domain map are released; names outside the map are released without adopting legacy records; corrupt annotations do not wedge the route |
| `TestHTTPRouteReconciler_ConflictIsNotOwned` | A hostname held by someone else's record stays out of the annotation |
| `TestHTTPRouteReconciler_AdoptsLegacyOnlyForOwnedNames` | Legacy records are adopted only for names the route already recorded owning |
| `TestHTTPRouteReconciler_SharedHostnameIsKept` | Deleting one of two routes sharing a hostname keeps the record (B1) |
| `TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod` | A failing cleanup holds the finalizer and retries until the grace period runs out, then releases the route (B9, D10) |
| `TestHTTPRouteReconciler_RequireAccepted` | Off by default; on, only accepted routes get DNS (S3) |
| `TestHTTPRouteReconciler_OneReconcilePublishes` | The reconcile that adds the finalizer also publishes (D7) |
| `TestHTTPRouteReconciler_WantsUpdate` | Which updates trigger a reconcile (D8) |
| `TestHTTPRouteReconciler_EventsAndMetrics` | Outcomes appear as Events and in the counters (D6) |
| `TestHTTPRouteReconciler_FlushFailureRequeues` | A failed commit is returned so controller-runtime requeues instead of silently dropping the change |

## Integration tests

`test/integration/` drives the real provider over real HTTP against `internal/testenv.OPNsense`, the same in-memory appliance `make fake-opnsense` serves.

The fake follows the appliance where the provider depends on it. Writes are staged (`Snapshot`) and only published (`Published`) by reconfigure. Unknown uuids get HTTP 200 with a result body. Invalid rows are rejected with `validations`. In these tests it requires the credentials the provider sends. `SetFailure` makes an endpoint return an error status, and `DuringReconfigure` lets a write race a publish.

```bash
make test-integration
```

### Provider conformance — `internal/dns/dnstest`

The contract every provider must meet, written once against `dns.Provider` and run by each provider through an adapter that seeds and reads records on its test backend (`conformance_test.go` for OPNsense). It covers:

- creating, updating, keeping (create-only mode) and re-enabling managed records, and writing nothing when a record already matches;
- never touching someone else's record: a live one is a `Conflict`, a disabled one is ignored, and `Remove` leaves both;
- legacy records: foreign unless adopted, rewritten with the marker when adopted, removed only when adopting;
- converging duplicate managed records, removing a missing record, and a batch seeing its own writes.

### OPNsense-specific — `opnsense_test.go`

| Test | Description |
|---|---|
| `TestEnsureWritesTheOverrideFields` | The FQDN is split into hostname and domain, and the description carries the ownership marker |
| `TestEnsureUnchangedWritesNothing` | A matching record costs one list, no write and no reconfigure (B2) |
| `TestEnsureSurfacesValidationFailure` | A row the appliance rejects (HTTP 200, `result: failed`) is an error, not a success |
| `TestRemoveOfMissingRecordIssuesNoDelete` | Removing a record that does not exist issues no delete |
| `TestRemoveToleratesVanishedRecord` | A row deleted by someone else since the batch listed is not an error |
| `TestBatchListsOnce` | A batch lists once however many records it touches, and commits with one reconfigure (D4) |
| `TestUnusedBatchCommitsNothing` | A batch that was never used makes no calls on commit |
| `TestFirstCommitPublishes` | A new provider's first used batch publishes, in case an earlier process left changes staged (B10) |
| `TestFailedPublishStaysOwed` | After a failed reconfigure, a later batch that writes nothing still publishes (B10) |
| `TestWriteDuringPublishStaysOwed` | A write made during another batch's reconfigure is still published by its own commit (D3) |
| `TestHealthCheckRejectsWrongCredentials` | A 401 from the appliance surfaces as an authentication failure |

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
| `TestRouteIsDeletableWhileApplianceFails` | With every appliance call failing, the route is released once the cleanup grace period runs out |
| `TestRecordPublishedAfterReconfigureRecovers` | A record staged while reconfigure fails is published once it recovers |
| `TestAnnotationCannotDeleteForeignRecords` | Names written into the ownership annotation outside the domain map are never deleted (S1) |
| `TestUnchangedRouteCausesNoWrites` | Reconciling an unchanged route makes no writes and no reconfigure (B2) |
| `TestDisabledManagedRecordIsReenabled` | A disabled managed override is re-enabled (B8) |
| `TestUnmanagedRecordIsNotOverwritten` | A hand-made record for a route's hostname is never repointed (S2) |
| `TestUnmanagedRecordSurvivesRouteDeletion` | Deleting the route leaves a hand-made record in place (S2) |
| `TestSharedHostnameSurvivesOneRouteDeletion` | Deleting one of two routes sharing a hostname keeps the record published (B1) |
| `TestLegacyRecordIsAdoptedByItsRoute` | A record from before ownership marking is adopted and re-marked by the route that recorded owning it |
| `TestLegacyRecordIsNotAdoptedByANewRoute` | A route that never owned a name cannot adopt or delete its legacy record |
| `TestAcceptanceControlsRecord` | With `--require-accepted-routes`, a route gets its record once accepted and loses it when acceptance goes |
| `TestDeleteIsPublishedAfterReconfigureRecovers` | A route is held while its deletion cannot be published, then goes with its record (B9) |
| `TestUnacceptedRouteGetsNoRecord` | With `--require-accepted-routes`, an unaccepted route gets no finalizer and no record (S3) |

`test/envtest/findings_test.go` holds regression tests for the 2026-10-12 audit findings (IDs such as S1 and B2); each failed on the code the audit reviewed.

## End-to-end

Not automated in CI. The manual path is documented in [docs/local-testing.md](local-testing.md):

```bash
make kind-deploy   # cluster + image + fake appliance + controller
make kind-demo     # apply hack/local/httproute.yaml
```

The controller logs `app.example.com -> 10.0.0.1` with reason `DNSRecordCreated`
(also an Event on the route, see `kubectl describe httproute local-demo`) followed
by one `reconfigure completed`, and `GET /records` on the fake shows the row. Automating this as `make test-e2e` is open work.

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
