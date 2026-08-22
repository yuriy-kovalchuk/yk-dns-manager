# TODO

## Documentation

- [ ] Add `CONTRIBUTING.md` — onboarding path for new contributors covering commit conventions, hooks setup (`make install-hooks`), and local dev workflow (`make kind-deploy`)

## Metrics

- [ ] Port 9090 is exposed and the Helm chart wires up a `Service` and optional `ServiceMonitor`, but no metrics are registered; either add basic counters (records created/updated/deleted/errors) or remove the metrics plumbing until it's real

## Testing

- [ ] Add E2E tests using the kind cluster (`make kind-deploy`) — infrastructure is in place; a basic test would create an HTTPRoute and assert the DNS record appears in OPNsense

## Improvements

- [ ] Add retry logic for transient OPNsense HTTP 5xx errors on the data endpoints (`searchHostOverride`, `addHostOverride`, `setHostOverride`, `delHostOverride`) — `reconfigure` already retries
- [ ] Support additional DNS record types — currently hardcoded to `A`; add at least `AAAA` and `CNAME`
