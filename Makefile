# ── Variables ──────────────────────────────────────────────────────────────────
APP_NAME   := yk-dns-manager
REGISTRY   ?= ghcr.io/yuriy-kovalchuk
IMAGE      ?= $(REGISTRY)/$(APP_NAME)
PLATFORMS  ?= linux/amd64,linux/arm64

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
IMG        ?= $(IMAGE):$(VERSION)

CHART_DIR  := charts/$(APP_NAME)

KIND_CLUSTER        ?= yk-dns-manager-dev
KIND_CONFIG         := hack/kind-config.yaml
# Every kind target pins this context. Without it a target run before kind-up
# acts on whatever the kubeconfig currently points at, which in practice is a
# real cluster.
KIND_CONTEXT        := kind-$(KIND_CLUSTER)
KIND_NAMESPACE      ?= yk-dns-manager-system
# Must match the sigs.k8s.io/gateway-api version in go.mod: the controller is
# compiled against these types, and CRDs two minor versions behind silently
# prune fields the reconciler reads.
GATEWAY_API_VERSION ?= v1.6.3

LOCALBIN              ?= $(shell pwd)/bin
# golangci-lint refuses to load a module whose Go minor is newer than the Go it
# was built with (v2.11.4, built with go1.26, rejects a go1.27 module). CI uses
# this pin too, through `make lint`.
GOLANGCI_LINT_VERSION ?= v2.15.0
# envtest control plane. The Kubernetes minor matches the k8s.io/* modules in
# go.mod; setup-envtest matches the controller-runtime release.
ENVTEST_K8S_VERSION   ?= 1.37.0
SETUP_ENVTEST_VERSION ?= v0.25.2
SETUP_ENVTEST         := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

VERSION_PKG := github.com/yuriy-kovalchuk/yk-dns-manager/internal/version

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION)

# ── Default ────────────────────────────────────────────────────────────────────
.DEFAULT_GOAL := all

.PHONY: all tidy deps-check fmt vet lint build run test test-unit test-envtest envtest test-cover vuln clean docker-build docker-push docker-build-local kind-demo kind-crds kind-status kind-reset helm-lint helm-template helm-package helm-push kind-up kind-down kind-load kind-secret kind-deploy kind-undeploy kind-reload kind-logs install-hooks help

## all: tidy, fmt, vet, lint, build
all: tidy fmt vet lint build

# ── Development ────────────────────────────────────────────────────────────────
## tidy: tidy and verify dependencies
tidy:
	go mod tidy && go mod verify

## deps-check: list outdated direct dependencies
deps-check:
	@go list -u -m -f '{{if and (not .Indirect) .Update}}{{.Path}}  {{.Version}} → {{.Update.Version}}{{end}}' all \
	  | grep -v "^$$" \
	  || echo "All direct dependencies are up to date."

## fmt: format Go source files
fmt:
	go fmt ./...

## vet: check for suspicious constructs
vet:
	go vet ./...

## lint: run golangci-lint
lint: $(LOCALBIN)/golangci-lint
	$(LOCALBIN)/golangci-lint run --timeout=5m ./...

## build: compile the primary binary to bin/
build:
	mkdir -p $(LOCALBIN)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/$(APP_NAME) ./cmd/$(APP_NAME)

# make run falls back to it when .env sets no CONFIG_PATH
LOCAL_CONFIG := hack/local/config.yaml

## run: build and run the primary binary locally
run:
	@echo "reconciling against kubecontext: $$(kubectl config current-context 2>/dev/null || echo unknown)"
	@set -a && { [ ! -f .env ] || . ./.env; } && set +a && export CONFIG_PATH=$${CONFIG_PATH:-$(LOCAL_CONFIG)} && echo "config: $$CONFIG_PATH" && go run -ldflags "$(LDFLAGS)" ./cmd/$(APP_NAME) --zap-log-level=debug --leader-elect=false

# ── Testing ────────────────────────────────────────────────────────────────────
## test: run all tests (unit + envtest)
test: test-unit test-envtest

## test-unit: run unit tests with coverage profile
test-unit:
	go test -race -timeout 120s -v -count=1 -coverprofile=coverage.txt ./cmd/... ./internal/...

## test-cover: run tests, print function summary, generate HTML report
test-cover:
	go test -v -race -count=1 -coverprofile=coverage.txt ./cmd/... ./internal/...
	go tool cover -html=coverage.txt -o coverage.html


## envtest: download the envtest control-plane binaries into bin/k8s
envtest:
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)/k8s -p path

## test-envtest: run the controller against a real API server and the memory provider
test-envtest:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)/k8s -p path)" \
		go test -race -timeout 300s -v -count=1 ./test/envtest/

## vuln: check for known vulnerabilities (also runs in CI)
# `go run` resolves through this module's toolchain, so govulncheck is built
# with the go.mod go version. A globally installed govulncheck is pinned to
# whatever Go built it and refuses to load a newer module.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# ── Docker ─────────────────────────────────────────────────────────────────────
## docker-build: multi-platform build via buildx (no push)
# No --load: buildx cannot export a multi-platform image into the local docker
# image store. The result lands in the builder cache, which is what a
# build-only check needs. For a locally loadable image use docker-build-local.
docker-build:
	$(MAKE) buildx-setup
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMG) .

## docker-push: multi-platform build and push to registry
# One buildx step: `docker push` cannot publish the multi-arch manifest that
# a multi-platform build produces.
docker-push:
	$(MAKE) buildx-setup
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		-t $(IMG) \
		--push .

# ── Helm ───────────────────────────────────────────────────────────────────────
## helm-lint: lint the chart (strict)
helm-lint:
	helm lint --strict $(CHART_DIR) -f $(CHART_DIR)/ci/values.yaml

## helm-template: render the chart with the CI values as a smoke test
helm-template:
	helm template smoke $(CHART_DIR) -f $(CHART_DIR)/ci/values.yaml

## helm-package: package the Helm chart
helm-package:
	helm package $(CHART_DIR)

## helm-push: package and push the Helm chart to OCI registry
helm-push: helm-package
	helm push $(APP_NAME)-*.tgz oci://$(REGISTRY)/charts

# ── Local Kind cluster ─────────────────────────────────────────────────────────
docker-build-local:
	docker build \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):local .

## kind-up: create the dev cluster, or say so when reusing an existing one
kind-up:
	@if kind get clusters | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "reusing cluster $(KIND_CLUSTER) ($$(kubectl --context $(KIND_CONTEXT) get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}' 2>/dev/null || echo 'not responding')) — run 'make kind-down' or 'make kind-reset' to recreate it"; \
	else \
		kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG); \
	fi

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build-local
	kind load docker-image $(IMAGE):local --name $(KIND_CLUSTER)

## kind-secret: put the OPNsense credentials from .env in a Secret, for deploying against a real appliance
kind-secret:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && \
		kubectl --context $(KIND_CONTEXT) create namespace $(KIND_NAMESPACE) --dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f - && \
		kubectl --context $(KIND_CONTEXT) create secret generic yk-dns-manager-credentials \
			--namespace $(KIND_NAMESPACE) \
			--from-literal=OPNSENSE_API_KEY=$${OPNSENSE_API_KEY} \
			--from-literal=OPNSENSE_API_SECRET=$${OPNSENSE_API_SECRET} \
			--dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -

## kind-crds: install the Gateway API CRDs the controller watches
kind-crds:
	kubectl --context $(KIND_CONTEXT) apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml

# hack/local-values.yaml selects the memory provider, so no appliance is needed.
# To deploy against a real one, see the comments there.
## kind-deploy: cluster + CRDs + image + controller
kind-deploy: kind-up kind-crds kind-load
	@if ! helm --kube-context $(KIND_CONTEXT) upgrade --install $(APP_NAME) $(CHART_DIR) \
		--namespace $(KIND_NAMESPACE) \
		--create-namespace \
		-f hack/local-values.yaml \
		--wait --timeout 2m; then \
		echo "--- deploy did not become ready; pod status and recent events ---"; \
		kubectl --context $(KIND_CONTEXT) -n $(KIND_NAMESPACE) get pods -o wide; \
		kubectl --context $(KIND_CONTEXT) -n $(KIND_NAMESPACE) get events --sort-by=.lastTimestamp | tail -10; \
		exit 1; \
	fi

kind-undeploy:
	helm --kube-context $(KIND_CONTEXT) uninstall $(APP_NAME) --namespace $(KIND_NAMESPACE) --ignore-not-found

## kind-demo: apply the sample HTTPRoute that the local domain map matches
kind-demo:
	kubectl --context $(KIND_CONTEXT) apply -f hack/local/httproute.yaml

kind-reload: kind-load
	kubectl --context $(KIND_CONTEXT) rollout restart deployment/$(APP_NAME) --namespace $(KIND_NAMESPACE)

kind-logs:
	kubectl --context $(KIND_CONTEXT) logs --namespace $(KIND_NAMESPACE) \
		-l app.kubernetes.io/name=$(APP_NAME) \
		--follow

## kind-status: print the local dev state in one place
kind-status:
	@echo "kubecontext:  $$(kubectl config current-context 2>/dev/null || echo none)"
	@if kind get clusters | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "cluster:      $(KIND_CLUSTER) ($$(kubectl --context $(KIND_CONTEXT) get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}' 2>/dev/null || echo 'not responding'))"; \
	else \
		echo "cluster:      absent — make kind-up"; \
	fi
	@echo "gateway CRDs: $$(kubectl --context $(KIND_CONTEXT) get crd httproutes.gateway.networking.k8s.io >/dev/null 2>&1 && echo installed || echo 'missing — make kind-crds')"
	@echo "controller:   $$(kubectl --context $(KIND_CONTEXT) -n $(KIND_NAMESPACE) get deploy $(APP_NAME) -o jsonpath='{.status.readyReplicas}/{.spec.replicas} ready' 2>/dev/null || echo 'not deployed — make kind-deploy')"
	@echo "config:       $$(grep -sE '^CONFIG_PATH=.' .env | sed 's/^CONFIG_PATH=//' | grep . || echo '$(LOCAL_CONFIG) (default)')"

## kind-reset: rebuild the dev cluster from scratch (use it after the kind pin changes)
kind-reset: kind-down kind-deploy

# ── Git hooks ──────────────────────────────────────────────────────────────────
## install-hooks: install git hooks from .githooks/
install-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed — tests and commit messages will be checked before every push."

# ── Tools ──────────────────────────────────────────────────────────────────────
## buildx-setup: create or start the multi-platform buildx builder
buildx-setup:
	docker buildx create --name multiplatform --driver docker-container --bootstrap --use 2>/dev/null || \
	  docker buildx inspect --bootstrap multiplatform

## clean: remove build artifacts (bin/, coverage files, helm packages)
clean:
	rm -rf $(LOCALBIN) coverage.* *.tgz

# ── Internal ───────────────────────────────────────────────────────────────────
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

$(LOCALBIN)/golangci-lint: $(LOCALBIN)
	# Install script pinned to the same tag as the binary. `master/install.sh`
	# is both unpinned (supply chain) and broken for v2.14.0: its checksum grep
	# also matches the `.sbom.json` asset and then compares the wrong hash.
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | \
		sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)

## help: print this help text
help:
	@grep -E '^## ' Makefile | sed 's/^## //' | column -t -s ':'
