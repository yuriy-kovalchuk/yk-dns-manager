# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com),
and this project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- `networkPolicy.apiServer` and `networkPolicy.provider` set the egress ports, and optionally CIDRs, for the API server and the DNS provider.
- `allow_insecure_http` provider setting, the opt-in for a plain-HTTP appliance URL.
- envtest suite (`make test-envtest`), part of `make test` and CI.

### Changed

- The provider works in one batch per reconcile: it lists the appliance's overrides once, writes only what differs, and reconfigures Unbound only when something changed. A controller restart no longer reloads Unbound once per route.
- `default_ttl` is ignored with a warning and removed from the chart and examples; Unbound host overrides have no per-record TTL, so it never had an effect.
- The startup health check has its own 10s deadline.
- **Breaking:** a `base_url` using `http` is refused unless `allow_insecure_http: "true"` is set; plain HTTP sends the API credentials in cleartext.
- **Breaking:** the provider is chosen by the config's `provider` name. An unknown name fails startup instead of falling back to the first provider that initializes, and startup reports that provider's own error.
- Hostnames recorded in the `dns.yk/managed-hostnames` annotation are only deleted if the domain map covers them. When a domain is dropped from the map, its existing records are logged and left in place instead of being deleted.
- Releases run the full CI checks first and are refused unless the tagged commit is on master.

### Fixed

- In create-only mode a disabled override counted as an existing record, so the name never resolved again; it is now re-enabled.
- A failed reconfigure left changes staged but unpublished once the retry found nothing left to write; a failed publish now stays owed until a later one succeeds, and a new process publishes once in case an earlier one died with changes staged.
- A reconfigure answering a status other than `ok` was treated as success.
- Any user able to edit an HTTPRoute could delete arbitrary DNS host overrides by writing their names into the ownership annotation.
- Enabling the NetworkPolicy blocked the API server on clusters that serve it on 6443.
- A ServiceMonitor placed in another namespace selected nothing.
- `make kind-deploy` CrashLooped because the kind values did not point at the fake appliance.
- `cmd/` tests did not run under `make test` or in CI.
