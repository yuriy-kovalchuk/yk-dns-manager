# Local Testing

## Prerequisites

- Go 1.27.2 (pinned in `go.mod`; with `GOTOOLCHAIN=auto` the go command fetches it on demand)
- Docker (for buildx and kind)
- `kubectl`, and `helm` v3.18 or newer (CI uses 3.18; v4 works too, see the strategy note in the CHANGELOG)
- For kind-based testing: [`kind`](https://kind.sigs.k8s.io/) **v0.33.0 or newer**.
  `hack/kind-config.yaml` pins the node image to
  `kindest/node:v1.37.0@sha256:a1ed56cf…`, so the dev cluster is always Kubernetes
  1.37.0 regardless of which kind you have — but older kind releases cannot load
  newer node images, so upgrade the tool alongside the pin.

No OPNsense appliance and no real credentials are needed. The local profiles
(`hack/local/config.yaml` for `make run`, `hack/local-values.yaml` for kind)
select `provider: memory`, which keeps records in the controller's memory and
logs every change.

Every `kind-*` target pins `--context kind-yk-dns-manager-dev`, so it acts on the
dev cluster rather than on whatever your kubeconfig currently points at.

## Pick the loop that matches the change

Most changes never need a cluster. Reach for one only when the thing you are
testing is the cluster.

| You changed | Use | Cost |
|---|---|---|
| reconcile logic, config parsing, provider calls, dedup, finalizers | `make test-unit` | ~2s, no cluster, no network |
| the OPNsense provider against a real appliance | `go test -run TestConformanceAgainstAppliance ./internal/dns/opnsense/` with `OPNSENSE_TEST_*` set, see [testing.md](testing.md#against-a-real-appliance) | ~10s, needs a test appliance |
| the controller's behaviour against a real API server: event filter, finalizers, ownership | `make test-envtest` | ~1 min, no cluster needed |
| wiring you can only see end to end — manager setup, RBAC, chart, probes, real informer behaviour | Path A below | ~40s bring-up, ~10s per iteration |
| the binary's own startup, flags, config loading, logging | Path B below | ~2s per restart |

The unit tests cover the reconciler against a mock provider and a fake Kubernetes
client, and the OPNsense provider against stubbed API answers; envtest runs the
real manager against a real API server and the memory provider. That is most of
the surface area.

## When something is not doing what you expect

`make kind-status` prints the local dev state in one place:

```
kubecontext:  kind-yk-dns-manager-dev
cluster:      yk-dns-manager-dev (v1.37.0)
gateway CRDs: installed
controller:   1/1 ready
config:       hack/local/config.yaml (default)
```

Every line names the command that fixes it. Run this before debugging; most local
failures are a missing prerequisite rather than a bug.

Use the make targets rather than assembling `docker build` / `kind load` / `helm
upgrade` by hand. They are ordered and idempotent, and `kind-deploy` prints pod
status and recent events when the rollout does not become ready. A hand-assembled
sequence will happily wait out helm's timeout and tell you nothing.

## Path A — everything in kind

One command brings up a cluster, installs the CRDs, builds and loads the image
and deploys the controller with the memory provider:

```bash
make kind-deploy
```

Then drive it:

```bash
make kind-demo         # applies hack/local/httproute.yaml (app.example.com)
make kind-logs         # watch the reconcile
```

The controller writes `app.example.com -> 10.0.0.1`, and the log shows it:

```
{"logger":"dns-memory","msg":"memory: record created","fqdn":"app.example.com","value":"10.0.0.1","owners":["httproute/default/local-demo"]}
```

Iterate on a code change with `make kind-reload` — rebuild the image, load it into
kind, restart the deployment. `make kind-reset` tears the cluster down and
rebuilds it; use it when the kind pin changes or when a stale cluster is
suspected, because `kind-up` reuses an existing cluster and says so.
`make kind-down` tears it down.

## Path B — standalone binary

The fast inner loop: no image build, restart in about two seconds. The controller
runs as a plain binary and connects through your kubeconfig, so it still needs a
cluster with Gateway API CRDs. The DNS records live in the process.

**Check the context first.** `make run` uses your current kubeconfig context, and
`make kind-up` switches it to the dev cluster. If your current context is a real
cluster, the controller will reconcile against it and write real DNS records.
`make run` prints the context it is about to use.

```bash
make kind-up           # or use any cluster you already have a context for
make kind-crds         # the controller watches HTTPRoutes; without the CRDs it starts and then fails to sync
make run               # prints the kubecontext it is about to reconcile against
```

`make run` needs no `.env`: without one it uses `hack/local/config.yaml`. Every
record the controller writes, updates or deletes is logged as a `memory: record …`
line; they are gone when the process stops.

## Against a real appliance

Start from the example config and change the backend:

```bash
cp examples/config.yaml .developer/config.yaml
# set settings.base_url to https://<appliance>/api
cp .env.example .env
# set CONFIG_PATH=.developer/config.yaml and the real OPNSENSE_API_KEY / OPNSENSE_API_SECRET
```

An `.env` from before the single config file still sets `DOMAIN_MAP_PATH` and
`DNS_PROVIDER_PATH`; replace both with `CONFIG_PATH`. For kind, follow the
comments in `hack/local-values.yaml` and run `make kind-secret` before
`make kind-deploy`.

If the appliance uses a self-signed certificate, put its CA in a ConfigMap under
the key `ca.pem`, name that ConfigMap in `dnsProvider.caConfigMap`, and set
`ca_file: "/etc/yk-dns-manager/ca/ca.pem"` in settings. Do not set
`skip_tls_verify: true`: the API credentials go out as basic auth on every
request.

## Running tests

```bash
make test              # unit + envtest
make test-envtest      # the real manager against an envtest API server (~1 min)
make test-unit         # unit only — no network, no external processes
make test-cover        # HTML report at coverage.html + function summary
go test ./internal/config/ -v
go test -run TestNewDomainMap ./internal/config/ -v
```

See [testing.md](testing.md) for the full inventory, and for running the
conformance suite against a real appliance.

## External dependencies

| Dependency | How to provide locally |
|---|---|
| Kubernetes cluster | kind (`make kind-up`), minikube, or any remote cluster with kubeconfig access |
| Gateway API CRDs | `make kind-crds` for a kind cluster; `kind-deploy` installs them as a prerequisite. Anywhere else: `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml` |
| DNS backend | `provider: memory` (the local default), or a real appliance as above |
