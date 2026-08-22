# DNS Providers

How yk-dns-manager connects to DNS backends: the common mechanism, then each supported provider with its configuration and Secret.

## How the mechanism works

- The app contains no provider-specific logic outside the provider packages. Every configured instance is built from the `providers` map in the config file, and each managed record is applied to **all** instances (broadcast).
- Each instance has four knobs:
  | Field | Meaning |
  |---|---|
  | `provider` | backend type (e.g. `opnsense`); optional, defaults to the instance name |
  | `upsert` | `true` — update existing records on every reconcile; `false` — only create missing ones |
  | `secret` | name of the Kubernetes Secret holding this instance's credentials |
  | `settings` | free-form string map of non-secret connection parameters, validated by the provider itself |
- **Credentials never live in the config file.** The app reads each `secret:` via the Kubernetes API at startup and passes the raw data to the provider. The mechanism is **key-agnostic**: each provider declares the keys it expects and validates them in its constructor, failing startup with an error naming any missing key.
- All providers implement one small interface (`Exists`, `Create`, `Update`, `Delete`, `Upsert`, `HealthCheck`), so the controller, config schema, and Helm chart stay provider-agnostic.

## OPNsense

Unbound DNS host overrides via the OPNsense API. Each managed hostname becomes a host override entry; the provider triggers `unbound/service/reconfigure` after every change so the record takes effect.

### Settings

| Key | Required | Description |
|---|---|---|
| `base_url` | yes | OPNsense API base URL; the path must start with `/api` (e.g. `https://opnsense.example.com/api`) |
| `skip_tls_verify` | no | Set `"true"` to disable TLS certificate verification |

### Secret

Keys: `API_KEY`, `API_SECRET` (an OPNsense account with access to the Unbound API; used as HTTP Basic auth).

```bash
kubectl create secret generic opnsense-creds \
  --namespace yk-dns-manager \
  --from-literal=API_KEY=your-key \
  --from-literal=API_SECRET=your-secret
```

### Configuration

Helm values:

```yaml
dnsProviders:
  opnsense:
    provider: opnsense   # optional — defaults to the instance name
    upsert: false
    secret: opnsense-creds
    settings:
      base_url: "https://opnsense.example.com/api"
```

Config file (for running outside Helm):

```yaml
providers:
  opnsense:
    upsert: false
    secret: opnsense-creds
    settings:
      base_url: "https://opnsense.example.com/api"
```

### Behavior notes

- **Disabled overrides count as absent.** An override manually disabled in the OPNsense UI is not resolved by Unbound, so `Exists` returns `false` for it, and `Create`/`Upsert` re-enable it in place instead of adding a duplicate.
- **Reconfigure after each mutation**, retried up to 3 times with 500 ms backoff; if it still fails, the reconcile is retried by the controller.
- **Existence checks fetch the full override table** and filter client-side — the OPNsense API offers no per-hostname filter. Fine at the override counts this controller manages.
- Deletion is idempotent: deleting a record that does not exist is not an error.

## Planned providers

| Provider | Backend |
|---|---|
| Pi-hole | gravity DNS |
| AdGuard Home | AdGuard Home REST API |
| CoreDNS | CoreDNS plugin records |

Each will document its own settings and Secret keys in this file.

## Adding a new provider

1. Create `internal/dns/<name>/` and implement `dns.Provider` — [internal/dns/opnsense](../internal/dns/opnsense/) is the reference implementation.
2. In the constructor, validate the required `settings` keys **and** the Secret keys you expect; fail with an error that names what is missing.
3. Register the type in the `factories` map in [internal/app/app.go](../internal/app/app.go) — one import and one map entry.
4. Keep `Delete` idempotent and `HealthCheck` a cheap authenticated call.
5. Document settings and Secret keys in this file.

Nothing else in the app (controller, config, chart, credentials mechanism) needs changes. See [architecture.md — Adding a new provider](architecture.md#adding-a-new-provider).
