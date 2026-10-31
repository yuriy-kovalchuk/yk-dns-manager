# Testing

All tests run via `make test`; a plain `go test ./...` skips envtest unless `KUBEBUILDER_ASSETS` is set. CI runs `make vet build test lint` and `make vuln` on every push and pull request, and again on every release tag before anything is published; both are blocking.

## Overview

| Layer | Location | Count | What it covers |
|---|---|---|---|
| Unit | `internal/` | 63 | Config parsing and validation, controller reconcile logic, log-level handling, metrics, the publish gate, the conformance suite against the memory provider, and the OPNsense provider against stubbed API answers |
| Appliance | `internal/dns/opnsense/` | conformance | The conformance suite against a real OPNsense; opt-in, see [Against a real appliance](#against-a-real-appliance) |
| Envtest | `test/envtest/` | 23 | The real manager and predicates against a real API server and the memory provider |
| E2E | manual | — | Full flow in a kind cluster with the memory provider; see [docs/local-testing.md](local-testing.md) |

There are no mocking frameworks. Tests use plain structs implementing `dns.Provider` and `sigs.k8s.io/controller-runtime/pkg/client/fake`.

## Unit tests

### Logging — `internal/logging/`

| Test | Description |
|---|---|
| `TestParseLogLevel` | Named levels, numeric logr verbosity (`-3` means `V(3)`), and rejects garbage |
| `TestDebugLevelEnablesVerbosity` | `debug` actually turns `V(1)` output on |
| `TestDebugLevelKeepsProductionEncoder` | Debug does not switch zap to development mode |

### Provider selection — `internal/dns/providers/`

| Test | Description |
|---|---|
| `TestProvidersAcceptTheirOwnSettings` | Every settings key a real factory declares still builds that provider |
| `TestOPNsenseRequestsAreInstrumented` | The real OPNsense factory wraps the provider's transport, so its requests are counted though the provider has no metrics code |
| `TestSelectProvider` | Only the named provider is initialized; an unknown name lists the available ones; the provider's own error is the startup error; a factory returning `(nil, err)` never leaves a typed-nil provider behind; an unknown settings key fails startup (D11) |

### Config — `internal/config/`

| Test | Description |
|---|---|
| `TestLoad` | Parses provider, domains and settings from one file; `upsert` defaults to `false` |
| `TestLoad_UpsertTrue` | `upsert: true` is a top-level bool |
| `TestLoad_Rejected` | An empty file, a missing `provider` or `domains`, an invalid or duplicate domain entry, an unset `${VAR}` (S6) and an unknown top-level field (D11) fail startup |
| `TestLoad_EnvVarExpansion` | `${ENV_VAR}` in settings resolves from the environment; a literal `$` is kept (S6) |
| `TestLoad_MissingFile` | Errors for a non-existent file |
| `TestNewDomainMap` | Keys are lowercased and lose a trailing dot (B12); values are canonicalised through `netip` |
| `TestNewDomainMapValidation` | Rejects non-IPv4 values, empty maps, empty or invalid domain keys, and keys duplicated after normalization |
| `TestLookupIP` | An exact key matches only itself, case-insensitively and with a trailing dot (D2); a wildcard covers any depth; the most specific key wins |
| `TestLookupIPWildcardSkipsBareDomain` | `*.x` never matches `x` itself |

### Publish gate — `internal/dns/publish_test.go`

`dns.Publisher` decides when a provider with staged writes publishes; OPNsense and the memory provider share it.

| Test | Description |
|---|---|
| `TestPublisherFirstPublishIsOwed` | A new process publishes once even with nothing staged, in case an earlier one left changes staged (B10) |
| `TestPublisherFailedPublishStaysOwed` | After a failed publish, the next one goes out although nothing new was staged (B10) |
| `TestPublisherWriteDuringPublishStaysOwed` | A write made while a publish runs is still owed afterwards (D3) |
| `TestPublisherIntervalCoalesces` | Inside the interval a publish is held back with `*dns.PublishDeferred`, and one later publish covers the burst (M4) |
| `TestPublisherCountsOutcomes` | Publishes are counted as `ok`, `failed` or `deferred`, and only those that ran are timed |

### Metrics — `internal/metrics/`

| Test | Description |
|---|---|
| `TestCountersStartAtZero` | Every known record outcome, error stage and publish result is exported at 0 from startup, so `rate()` and `increase()` see the first increment |
| `TestEndpointOf` | A request is named by its last path segment that is not an object id (UUID, number, long hex) |
| `TestInstrumentTransportCountsRequests` | The transport wrapper counts every request by endpoint and status code, an unanswered one as `error`, and times them all |
| `TestManagedRecordsReplacesARoutesSeries` | A route's managed-record series are replaced as a whole, other routes keep theirs, and a forgotten route exports none |

### Provider conformance — `internal/dns/dnstest`

The contract every provider must meet, written once against `dns.Provider` and run by each provider through an adapter that seeds and reads records on its backend. It covers:

- creating, updating, keeping (create-only mode) and re-enabling managed records, and writing nothing when a record already matches;
- never touching someone else's record: a live one is a `Conflict`, a disabled one is ignored, and `Remove` leaves both;
- owner scoping: only an owner named on the record can delete it, a caller that is not one leaves it alone, `Ensure` adds an owner to a record another owner wrote, and a record whose owners are unknown (written before owners existed) is deletable by any caller;
- legacy records: foreign unless adopted, rewritten with the marker when adopted, removed only when adopting;
- converging duplicate managed records, removing a missing record, and a batch seeing its own writes.

### Memory provider — `internal/dns/memory/`

The `provider: memory` backend for local development, and the backend under envtest. Its `Seed`, `Stored`, `Live`, `Drop`, `FailPublish` and `Counts` methods act on the backend directly, as an operator or a broken backend would.

| Test | Description |
|---|---|
| `TestConformance` | The full conformance suite, so tests built on it run against the same contract as a real backend |
| `TestNew_PublishIntervalMustBeADuration` | A nonsense or negative `publish_interval` fails startup |

### OPNsense provider — `internal/dns/opnsense/`

The API tests run against `httptest` stubs that answer only what each test needs and keep no state; they pin the wire format and how the appliance's answers are read. Whether OPNsense really behaves that way is what the [appliance run](#against-a-real-appliance) checks.

| Test | Description |
|---|---|
| `TestNew_ValidSettings` | Builds a client from valid settings |
| `TestNew_DefaultTTLIgnored` | `default_ttl` is accepted for compatibility but warns that it has no effect |
| `TestNew_MissingRequiredSetting` | `base_url`, `api_key` and `api_secret` are required |
| `TestNew_PublishIntervalMustBeADuration` | A nonsense or negative `publish_interval` fails startup |
| `TestNew_SkipTLSVerifyWarns` | Enabling it logs a warning naming `ca_file` as the fix; a default config must not warn |
| `TestNew_TLSVerification` | Stands up HTTPS on a throwaway CA: system roots reject it, `ca_file` accepts it, `skip_tls_verify` accepts it |
| `TestNew_TLSConfigErrors` | Missing `ca_file`, non-PEM file, and `ca_file` together with `skip_tls_verify` |
| `TestNew_Scheme` | `http` is refused unless `allow_insecure_http` is true (any boolean spelling), warns when allowed, and a non-boolean value or another scheme is refused |
| `TestNew_SkipTLSVerifyIsABoolean` | `skip_tls_verify` accepts `true`, `True`, `1`, `false` and empty, and rejects a non-boolean value (D11); on, verification is off with the TLS floor set; off, the system roots are used |
| `TestResponseBodyLimit` | A body over the cap is reported as a limit error, not as truncated JSON |
| `TestErrorBodyTruncated` | Error bodies are capped rather than dumped whole into the log |
| `TestCommitChecksReconfigureStatus` | A reconfigure answering a status other than `ok` fails the commit, and the publish stays owed |
| `TestEnsureWritesTheOverrideFields` | The FQDN is split into hostname and domain, and the description carries the ownership marker and owner |
| `TestEnsureUnchangedWritesNothing` | A matching record costs one list, no write and no reconfigure (B2) |
| `TestEnsureSurfacesValidationFailure` | A row the appliance rejects (HTTP 200, `result: failed`) is an error carrying the appliance's reason, not a success (M3) |
| `TestRemoveOfMissingRecordIssuesNoDelete` | Removing a record that does not exist issues no delete |
| `TestRemoveToleratesVanishedRecord` | A row deleted by someone else since the batch listed (`not found`) is not an error |
| `TestBatchListsOnce` | A batch lists once however many records it touches, and commits with one reconfigure (D4) |
| `TestUnusedBatchCommitsNothing` | A batch that was never used makes no calls on commit |
| `TestHealthCheckRejectsWrongCredentials` | A 401 from the appliance surfaces as an authentication failure |

### HTTPRoute controller — `internal/controller/httproute_controller_test.go`

Uses a mock `dns.Provider` and a fake Kubernetes client, so there is no cluster and no DNS traffic.

| Test | Description |
|---|---|
| `TestHTTPRouteReconciler_Ensure` | A covered hostname gets one record, created or upserted, and never a second write; an existing record is left alone without upsert; a hostname outside the domain map is skipped |
| `TestHTTPRouteReconciler_Deletion` | Finalizer cleanup releases the spec and annotation hostnames, and a route whose hostnames no longer match the domain map is still deletable — the orphaned-finalizer bug |
| `TestHTTPRouteReconciler_CleanupOfUnownedHostnames` | Hostnames dropped from the spec or the domain map are released; names outside the map are released without adopting legacy records; corrupt annotations do not wedge the route |
| `TestHTTPRouteReconciler_ConflictIsNotOwned` | A hostname held by someone else's record stays out of the annotation |
| `TestHTTPRouteReconciler_AdoptsLegacyOnlyForOwnedNames` | Legacy records are adopted only for names the route already recorded owning |
| `TestHTTPRouteReconciler_SharedHostnameIsKept` | Deleting one of two routes sharing a hostname keeps the record (B1) |
| `TestHTTPRouteReconciler_CleanupRetriesWithinGracePeriod` | A failing cleanup holds the finalizer and retries until the grace period runs out, then releases the route, counts the abandoned cleanup and drops its managed-record series (B9, D10) |
| `TestHTTPRouteReconciler_RequireAccepted` | Off by default; on, only accepted routes get DNS (S3) |
| `TestHTTPRouteReconciler_OneReconcilePublishes` | The reconcile that adds the finalizer also publishes (D7) |
| `TestHTTPRouteReconciler_WantsUpdate` | Which updates trigger a reconcile (D8) |
| `TestHTTPRouteReconciler_EventsAndMetrics` | Outcomes appear as Events, in the counters, and as a managed-record series for each managed hostname but not a conflicting one (D6) |
| `TestHTTPRouteReconciler_FlushFailureRequeues` | A failed commit is returned so controller-runtime requeues instead of silently dropping the change |

## Against a real appliance

No fake stands in for OPNsense. `TestConformanceAgainstAppliance` runs the conformance suite against a real one, and is skipped unless `OPNSENSE_TEST_URL` is set:

```bash
OPNSENSE_TEST_URL=https://<appliance>/api \
OPNSENSE_TEST_KEY=... OPNSENSE_TEST_SECRET=... \
OPNSENSE_TEST_CA_FILE=/path/to/ca.pem \
go test -run TestConformanceAgainstAppliance -v ./internal/dns/opnsense/
```

It deletes every override for `app.example.com` and `other.example.com` before and after each case, so point it at a test appliance. Run it before a release and after changing the provider. The API lists staged overrides only, so on the appliance "live" means stored after a successful commit.

## Envtest

`test/envtest/` starts a real API server and etcd (controller-runtime envtest) with the Gateway API CRDs from the module go.mod pins. Each test starts its own manager, running the real reconciler against its own memory provider and watching its own namespace. This covers what the controller unit tests cannot, because they call `Reconcile` directly on a fake client: the event predicates, the finalizer → reconcile chain, and the API server's generation and finalizer semantics.

```bash
make test-envtest   # downloads pinned control-plane binaries into bin/k8s, then runs the suite
```

Without `KUBEBUILDER_ASSETS` the suite skips, so a plain `go test ./...` (and the pre-push hook) still works without the binaries. `make test`, and so CI, always runs it.

| Test | Description |
|---|---|
| `TestCreatePublishesRecord` | In both modes, the finalizer is added, the record is published, and the route records ownership |
| `TestHostnameChangeMovesRecord` | Changing a hostname publishes the new record and removes the old one |
| `TestDeleteRemovesRecordAndReleasesRoute` | Deleting the route unpublishes the record and releases the finalizer |
| `TestUnmatchedRouteIsIgnored` | A route the domain map does not cover gets no finalizer and no DNS writes |
| `TestRouteIsDeletableWhileBackendFails` | With every publish failing, the route is released once the cleanup grace period runs out |
| `TestRecordPublishedAfterFailedPublish` | A record staged while publishing fails is published once it recovers |
| `TestAnnotationCannotDeleteForeignRecords` | Names written into the ownership annotation outside the domain map are never deleted (S1) |
| `TestUnchangedRouteCausesNoWrites` | Reconciling an unchanged route makes no writes and no publish (B2) |
| `TestDisabledManagedRecordIsReenabled` | A disabled managed record is re-enabled (B8) |
| `TestUnmanagedRecordIsNotOverwritten` | A hand-made record for a route's hostname is never repointed (S2) |
| `TestUnmanagedRecordSurvivesRouteDeletion` | Deleting the route leaves a hand-made record in place (S2) |
| `TestSharedHostnameSurvivesOneRouteDeletion` | Deleting one of two routes sharing a hostname keeps the record published (B1) |
| `TestLegacyRecordIsAdoptedByItsRoute` | A record from before ownership marking is adopted and re-marked by the route that recorded owning it |
| `TestLegacyRecordIsNotAdoptedByANewRoute` | A route that never owned a name cannot adopt or delete its legacy record |
| `TestAcceptanceControlsRecord` | With `--require-accepted-routes`, a route gets its record once accepted and loses it when acceptance goes |
| `TestDeleteIsPublishedAfterFailedPublish` | A route is held while its deletion cannot be published, then goes with its record (B9) |
| `TestUnacceptedRouteGetsNoRecord` | With `--require-accepted-routes`, an unaccepted route gets no finalizer and no record (S3) |
| `TestWildcardRouteHostnameGetsNoRecord` | A route hostname of `*.example.com` creates no wildcard record (H2) |
| `TestDriftOnTheBackendIsHealed` | A record deleted on the DNS backend is republished on the next resync (M1) |
| `TestBurstOfRoutesCostsOnePublish` | Five routes applied at once cost at most two publishes (M4) |
| `TestAnnotationCannotDeleteAnotherRoutesRecord` | A route cannot delete a record another route wrote, whatever its annotation claims (H1) |
| `TestSharedHostnameIsReleasedByTheLastRoute` | The last of two routes sharing a hostname releases the record |
| `TestRecordFromBeforeOwnersIsStillReleased` | A record marked before owners existed is still released when its route goes |

`test/envtest/findings_test.go` holds regression tests for the audit findings (IDs such as S1 and B2 from 2026-10-12, H1 and H2 from the follow-up audit); each failed on the code the audit reviewed.

## End-to-end

Not automated in CI. The manual path is documented in [docs/local-testing.md](local-testing.md):

```bash
make kind-deploy   # cluster + image + controller with the memory provider
make kind-demo     # apply hack/local/httproute.yaml
```

The controller logs `app.example.com -> 10.0.0.1` with reason `DNSRecordCreated`
(also an Event on the route, see `kubectl describe httproute local-demo`), and the
memory provider logs `memory: record created` for it. Automating this as `make test-e2e` is open work.

## Running tests

```bash
make test              # unit + envtest
make test-envtest      # envtest only
make test-unit         # unit only, with a coverage profile
make test-cover        # function summary + coverage.html
make vuln              # govulncheck, same as CI

go test -race ./...    # race detector
go test ./internal/config/ -v
go test -run TestNewDomainMapValidation ./internal/config/ -v
```
