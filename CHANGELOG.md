# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com),
and this project adheres to [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Leader election (`--leader-elect`, chart value `leaderElection.enabled`, on by default) through a Lease in the release namespace, so only one replica manages DNS. More replicas and a `RollingUpdate` strategy are now safe; the chart refuses `replicaCount` > 1 with leader election off. The chart adds a Role for Leases and leader-election Events.
- `--require-accepted-routes` (chart value `requireAcceptedRoutes`, off by default) limits DNS to HTTPRoutes that a parent Gateway has accepted.
- `--cleanup-grace-period` (default 5m): how long a failing DNS cleanup of a deleted HTTPRoute is retried.
- Events on HTTPRoutes for created, updated, released, conflicting and failing DNS records, and the `yk_dns_manager_records_total` and `yk_dns_manager_dns_errors_total` metrics. The ClusterRole gains create/patch on `events.k8s.io` events.
- Ownership marking: every DNS record the controller writes is marked (the OPNsense override description `yk-dns-manager: managed record, do not edit`), and only marked records are ever changed or deleted.
- Provider conformance suite (`internal/dns/dnstest`) that every provider runs.
- `networkPolicy.apiServer` and `networkPolicy.provider` set the egress ports, and optionally CIDRs, for the API server and the DNS provider.
- `allow_insecure_http` provider setting, the opt-in for a plain-HTTP appliance URL.
- envtest suite (`make test-envtest`), part of `make test` and CI.

### Changed

- **Breaking:** an exact domain-map key matches only that one name. It used to cover every name below it as well, so `example.com: 10.0.0.1` also served `app.example.com`. Wildcards are unchanged: `*.example.com` covers every name below example.com at any depth. **Before upgrading**, replace each exact key that is meant to cover subdomains with its `*.` form (keep the exact key too if the bare name needs a record); otherwise the controller releases the records of those subdomains. At startup the controller logs every exact key without a matching wildcard. The chart default is now `"*.example.com"`.
- **Breaking:** a `${NAME}` reference to an unset environment variable in the provider settings fails startup instead of expanding to an empty string, and only `${NAME}` is expanded, so a literal `$` in a value is kept.
- **Breaking:** unknown fields in the provider config and unknown `settings` keys fail startup instead of being ignored.
- **Breaking:** `skip_tls_verify` and `allow_insecure_http` are parsed as booleans. `"True"` or `"1"` now turn them on (only the exact string `"true"` did before, so `"True"` silently meant off), and a value that is not a boolean, such as `"yes"`, fails startup.
- A failing DNS cleanup of a deleted HTTPRoute holds the finalizer and is retried for `--cleanup-grace-period` instead of being given up after one attempt; the route is still released after that.
- The reconcile that adds the finalizer also publishes the records, routes are read from the informer cache, and the ownership annotation is written as a merge patch. Another controller's finalizer changes no longer trigger a reconcile.
- **Breaking:** a record for a route's hostname that the controller did not create is no longer overwritten or deleted; it is logged as a conflict. Records created by earlier versions are adopted automatically by the route whose `dns.yk/managed-hostnames` annotation lists them, and re-marked once.
- A hostname shared by several routes keeps its record until the last of them stops claiming it.
- The provider works in one batch per reconcile: it lists the appliance's overrides once, writes only what differs, and reconfigures Unbound only when something changed. A controller restart no longer reloads Unbound once per route.
- `default_ttl` is ignored with a warning and removed from the chart and examples; Unbound host overrides have no per-record TTL, so it never had an effect.
- The startup health check has its own 10s deadline.
- **Breaking:** a `base_url` using `http` is refused unless `allow_insecure_http: "true"` is set; plain HTTP sends the API credentials in cleartext.
- **Breaking:** the provider is chosen by the config's `provider` name. An unknown name fails startup instead of falling back to the first provider that initializes, and startup reports that provider's own error.
- The `dns.yk/managed-hostnames` annotation no longer authorizes deletes; only records carrying the ownership marker can be deleted. When a domain is dropped from the domain map, the records the controller created under it are cleaned up; legacy records from earlier versions under it are left in place.
- Releases run the full CI checks first and are refused unless the tagged commit is on master.

### Fixed

- Domain-map keys written with uppercase letters or a trailing dot never matched; keys are now normalized, validated as domain names, and duplicates after normalization are rejected.
- `strategy.type: RollingUpdate` without a `strategy.rollingUpdate` block rendered `rollingUpdate: null`. The chart now renders `maxSurge` and `maxUnavailable` in full (25% unless set), so switching back to `Recreate` works with both client-side and server-side apply (Helm 4's default). A release first deployed with `RollingUpdate` by an earlier chart version may need one `helm upgrade --server-side=false` to switch back.
- A deleted HTTPRoute's DNS cleanup could stay unpublished when the reconfigure failed, because the finalizer was already gone; the cleanup is now published before the finalizer is removed.
- In create-only mode a disabled override counted as an existing record, so the name never resolved again; it is now re-enabled.
- A failed reconfigure left changes staged but unpublished once the retry found nothing left to write; a failed publish now stays owed until a later one succeeds, and a new process publishes once in case an earlier one died with changes staged.
- A reconfigure answering a status other than `ok` was treated as success.
- Any user able to edit an HTTPRoute could delete arbitrary DNS host overrides by writing their names into the ownership annotation.
- An HTTPRoute with a wildcard hostname (`*.example.com`) matched the domain map's wildcard key and created a wildcard override, so that one route answered for every name below the domain. Wildcard route hostnames are now ignored, with a warning Event on the route.
- In upsert mode a route could repoint a hand-made record for its hostname, and deleting the route deleted it.
- Deleting one of two routes that share a hostname deleted the record the other still needed.
- Enabling the NetworkPolicy blocked the API server on clusters that serve it on 6443.
- A ServiceMonitor placed in another namespace selected nothing.
- `make kind-deploy` CrashLooped because the kind values did not point at the fake appliance.
- `cmd/` tests did not run under `make test` or in CI.
