# Local Testing

## Prerequisites

- Go 1.27.2 (pinned in `go.mod`; with `GOTOOLCHAIN=auto` the go command fetches it on demand)
- Docker (for buildx and kind)
- `kubectl` and `helm` v3.x
- For kind-based testing: [`kind`](https://kind.sigs.k8s.io/)

No OPNsense appliance and no real credentials are needed. `make fake-opnsense`
serves an in-memory stand-in for the appliance API, and the local profile under
`hack/local/` is wired to it.

Every `kind-*` target pins `--context kind-yk-dns-manager-dev`, so it acts on the
dev cluster rather than on whatever your kubeconfig currently points at.

## 1. Everything in kind

One command brings up a cluster, builds and loads the image, installs the fake
appliance and deploys the controller:

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

Tear it down with `make kind-down`.

## 2. Standalone binary against the fake

The controller runs as a plain binary and connects through your kubeconfig, so it
still needs a cluster with Gateway API CRDs — but the DNS backend stays local.

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

## 3. Against a real appliance

Copy the local profile and change only the backend:

```bash
cp hack/local/dns-provider.yaml .developer/dns-provider.yaml
# set settings.base_url to https://<appliance>/api
```

If the appliance uses a self-signed certificate, put its CA in a ConfigMap under
the key `ca.pem`, name that ConfigMap in `dnsProvider.caConfigMap`, and set
`ca_file: "/etc/yk-dns-manager/ca/ca.pem"` in settings. Do not set
`skip_tls_verify: true`: the API credentials go out as basic auth on every
request.

## Running Tests

### Unit tests

Fast, no network, no external processes. Runs in milliseconds:

```bash
make test-unit
```

Tests live alongside the code they test (`*_test.go` files in the same package). No mocking frameworks — plain structs implementing interfaces. Table-driven tests for multiple input cases.

### Integration tests

Spin up a real HTTP server with in-memory OPNsense-like handlers and exercise the provider over actual HTTP:

```bash
make test-integration
```

These verify wire-format correctness: JSON marshaling, URL construction, HTTP status handling, and response parsing against the fake OPNsense API in `internal/testenv` — the same fake `make fake-opnsense` serves.

### All tests

```bash
make test          # runs unit + integration
```

### Code coverage

```bash
make test-cover    # HTML report at coverage.html + function summary to stdout
```

### Specific package or file

```bash
go test ./internal/config/ -v
go test -run TestLoadDomainMap ./internal/config/ -v
```

## External Dependencies

The controller needs:

| Dependency | How to provide locally |
|---|---|
| Kubernetes cluster | kind (`make kind-up`), minikube, or any remote cluster with kubeconfig access |
| Gateway API CRDs | `make kind-crds` for a kind cluster; `make kind-deploy` installs them as a prerequisite. Anywhere else: `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml` |
| DNS provider (OPNsense) | `make fake-opnsense` for local work, `make kind-fake-install` inside the cluster, or a real appliance as in step 3 |
