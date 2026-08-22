# TODO — Assessment & Improvement Backlog

App assessment from the multi-provider readiness audit (2026-08-22). Work on these in the future.

## Multi-provider readiness — verdict

**Structurally ready.** Verified by audit: zero provider-specific references in the
controller, `dns.Manager`, `dns.Provider` interface, `dns.Credentials`, config schema,
Helm chart, and `main.go`. Adding a provider = new package implementing `dns.Provider`
+ one import and one `factories` entry in `internal/app/app.go`.

Known operational weaknesses (all structural-adjacent, not seam leaks):

- Startup health check is **fail-fast across all instances**: any one instance unhealthy
  → the whole app refuses to start.
- **No continuous health**: the startup check is one-shot; a backend that dies later is
  invisible until a reconcile fails. Liveness probe is pod-level only.
- **Sequential fan-out**: one slow provider (30s HTTP timeout) delays every record; one
  error re-reconciles all instances (safe — idempotent — but noisy).
- **`Upsert` boilerplate**: providers without a native upsert re-implement the same
  Exists→Create/Update dance.

## Overall rating (out of 10)

| Criterion | Score | Notes |
|---|---|---|
| Functionality | 8.5 | Core loop E2E-verified on kind; broadcast, no-op mode, finalizer cleanup solid. Gaps: A records only, no drift correction beyond upsert. |
| Readability | 9 | ~1.2k LOC production code, clean boundaries (orchestrator → RouteState + Manager → provider), short functional comments, no dead code. |
| Features | 6 | Correctness features good; operational features thin: no metrics, no continuous health, no per-provider status, no AAAA/CNAME. |
| Bugs | 9 | 13 known bugs found, 12 fixed with regression tests, 1 deferred with documented justification (OPNsense API has no per-hostname filter). |
| Documentation | 8.5 | README/architecture/testing/deployment/local-testing/providers in sync. Missing: `CONTRIBUTING.md`. |
| Ease of use | 8 | One config file, one Secret per provider, no-op default, `kind-load`/`kind-deploy` loop. Friction: per-provider Secret keys, `secretsNamespace` concept for local dev. |
| Extensibility | 8.5 | Cleanest seam in the codebase; nits: registry edit in `app.go`, no default-upsert helper. |
| Security | 8.5 | Credentials never in config/logs, least-privilege RBAC (`get` on named secrets only), strict decode. `skip_tls_verify` explicit per-instance opt-in. |
| Testing | 7.5 | 59 unit + 11 integration (fake HTTP), regression coverage for every fixed bug. Gaps: no kind E2E, opnsense package line-coverage 11% (HTTP plumbing — behavior covered by integration). |
| Operability | 7 | Structured logs, startup health, graceful shutdown, ServiceMonitor plumbing. Missing actual counters and per-provider visibility. |

**Overall: ~8/10** — production-ready for a small set of trusted backends. The ceiling
is operability (observability + health), not correctness or design.

## Improvements (prioritized)

### P1 — makes multi-provider operationally safe

- [ ] **Startup health policy**: fail-fast is too blunt for N providers. Per-instance
      retry window at startup, or start degraded + surface per-instance state. At minimum
      document the trade-off and make it configurable.
- [ ] **Per-instance health visibility**: periodic cheap health check with structured
      logs (or a `/healthz` endpoint listing each instance) — turns "silently down" into
      "visible and alertable".
- [ ] **Metrics**: `records_total{op,provider}`, `reconcile_errors{provider}`,
      `last_success_timestamp`. The chart's ServiceMonitor already exists — only the
      counters are missing.

### P2 — completeness

- [ ] **AAAA/CNAME support**: `Record.Type` is already in the interface; only the
      controller hardcodes `"A"`.
- [x] **Default `Upsert` helper** in `internal/dns` for providers without native upsert (done: `dns.Upsert` + opnsense single-fetch `setOverride`).
- [ ] **Kind E2E test**: the kind workflow (`kind-load`/`kind-deploy`) makes this cheap
      to script.

### P3 — nice-to-have

- [ ] **Kubernetes Events** for record create/update/delete — `kubectl describe
      httproute` would show what happened; cheap, high debug value, no API surface added.
- [ ] **CONTRIBUTING.md**.
- [ ] **Retry on OPNsense data endpoints** (`searchHostOverride` et al.) — only
      reconfigure retries today.

## Deliberately not doing

- Per-route status annotations (API surface bloat for a rarely-needed feature).
- Hot config reload (documented design decision: checksum + rolling restart is the right
  call at this scale).
- OPNsense per-hostname existence filter (B12 — the API has no filter parameter; full
  table fetch is the only reliable mechanism, acceptable at this scale).
