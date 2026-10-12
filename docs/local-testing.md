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

No OPNsense appliance and no real credentials are needed. `make fake-opnsense`
serves an in-memory stand-in for the appliance API, and the local profile under
`hack/local/` is wired to it.

Every `kind-*` target pins `--context kind-yk-dns-manager-dev`, so it acts on the
dev cluster rather than on whatever your kubeconfig currently points at.

## Pick the loop that matches the change

Most changes never need a cluster. Reach for one only when the thing you are
testing is the cluster.

| You changed | Use | Cost |
|---|---|---|
| reconcile logic, config parsing, provider calls, dedup, finalizers | `make test-unit`, `make test-integration` | ~2s each, no cluster, no network |
| the controller's behaviour against a real API server: event filter, finalizers, ownership | `make test-envtest` | ~1 min, no cluster needed |
| wiring you can only see end to end — manager setup, RBAC, chart, probes, real informer behaviour | Path A below | ~40s bring-up, ~10s per iteration |
| the binary's own startup, flags, config loading, logging | Path B below | ~2s per restart |

The unit tests cover the reconciler against a mock provider and a fake Kubernetes
client; the integration tests drive the real provider over HTTP against the same
in-memory appliance both paths below use; envtest runs the real manager against a
real API server. That is most of the surface area.

## When something is not doing what you expect

`make kind-status` prints the local dev state in one place:

```
kubecontext:  kind-yk-dns-manager-dev
cluster:      yk-dns-manager-dev (v1.37.0)
gateway CRDs: installed
controller:   1/1 ready
fake in-cluster: 1/1 ready
fake on host:    not listening — make fake-opnsense
env:          hack/local/domain-map.yaml hack/local/dns-provider.yaml
```

Every line names the command that fixes it. Run this before debugging; most local
failures are a missing prerequisite rather than a bug.

Use the make targets rather than assembling `docker build` / `kind load` / `helm
upgrade` by hand. They are ordered and idempotent, and `kind-deploy` prints pod
status and recent events when the rollout does not become ready. A hand-assembled
sequence will happily wait out helm's timeout and tell you nothing.

## Path A — everything in kind

One command brings up a cluster, installs the CRDs, builds and loads the image,
installs the fake appliance and deploys the controller:

```bash
cp .env.example .env   # the fake accepts any credentials
make kind-deploy
```

Then drive it:

```bash
make kind-demo         # applies hack/local/httproute.yaml (app.example.com)
make kind-logs         # watch the reconcile
```

The controller writes `app.example.com -> 10.0.0.1`. Read it back from the fake
(the image is distroless, so there is no shell in it):

```bash
kubectl --context kind-yk-dns-manager-dev -n yk-dns-manager-system \
  port-forward svc/fake-opnsense 8053:8053 &
curl -s localhost:8053/records
kill %1
```

Iterate on a code change with `make kind-reload` — rebuild the image, load it into
kind, restart the deployment. `make kind-reset` tears the cluster down and
rebuilds it; use it when the kind pin changes or when a stale cluster is
suspected, because `kind-up` reuses an existing cluster and says so.
`make kind-down` tears it down.

## Path B — standalone binary against the fake

The fast inner loop: no image build, restart in about two seconds. The controller
runs as a plain binary and connects through your kubeconfig, so it still needs a
cluster with Gateway API CRDs — only the DNS backend stays on your machine.

**Check the context first.** `make run` uses your current kubeconfig context, and
`make kind-up` switches it to the dev cluster. If your current context is a real
cluster, the controller will reconcile against it and write real DNS records.
`make run` prints the context it is about to use.

```bash
cp .env.example .env
make fake-opnsense     # foreground; listens on :8053
```

In another shell:

```bash
make kind-up           # or use any cluster you already have a context for
make kind-crds         # the controller watches HTTPRoutes; without the CRDs it starts and then fails to sync
make run               # prints the kubecontext it is about to reconcile against
```

`hack/local/dns-provider.yaml` points at `http://127.0.0.1:8053/api`, which works
for a binary on your machine. It does not work for a pod — see the Service DNS
name in `hack/local-values.yaml`.

Inspect what the controller wrote:

```bash
curl -s localhost:8053/records
```

The fake logs one line per request, including whether the controller presented
credentials. A missing `Authorization` header usually means the provider config
is pointing somewhere it should not be.

## Against a real appliance

Copy the local profile and change only the backend:

```bash
cp hack/local/dns-provider.yaml .developer/dns-provider.yaml
# set settings.base_url to https://<appliance>/api and delete allow_insecure_http
```

If the appliance uses a self-signed certificate, put its CA in a ConfigMap under
the key `ca.pem`, name that ConfigMap in `dnsProvider.caConfigMap`, and set
`ca_file: "/etc/yk-dns-manager/ca/ca.pem"` in settings. Do not set
`skip_tls_verify: true`: the API credentials go out as basic auth on every
request.

## Running tests

```bash
make test              # unit + integration + envtest
make test-envtest      # the real manager against an envtest API server (~1 min)
make test-unit         # unit only — no network, no external processes
make test-integration  # provider over real HTTP against internal/testenv
make test-cover        # HTML report at coverage.html + function summary
go test ./internal/config/ -v
go test -run TestLoadDomainMap ./internal/config/ -v
```

The integration tests verify wire-format correctness: JSON shapes, URL
construction, status handling and response parsing against the fake appliance in
`internal/testenv` — the same one `make fake-opnsense` serves. See
[testing.md](testing.md) for the full inventory.

## External dependencies

| Dependency | How to provide locally |
|---|---|
| Kubernetes cluster | kind (`make kind-up`), minikube, or any remote cluster with kubeconfig access |
| Gateway API CRDs | `make kind-crds` for a kind cluster; `kind-deploy` installs them as a prerequisite. Anywhere else: `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml` |
| DNS provider (OPNsense) | `make fake-opnsense` on the host, `make kind-fake-install` inside the cluster, or a real appliance as above |
