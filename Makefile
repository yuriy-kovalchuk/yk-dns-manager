# ── Variables ──────────────────────────────────────────────────────────────────
APP_NAME   := yk-dns-manager
REGISTRY   ?= ghcr.io/yuriy-kovalchuk
IMAGE      ?= $(REGISTRY)/$(APP_NAME)
PLATFORMS  ?= linux/amd64,linux/arm64

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
IMG        ?= $(IMAGE):$(VERSION)

CHART_DIR  := charts/$(APP_NAME)

KIND_CLUSTER        ?= yk-dns-manager-dev
KIND_CONFIG         := hack/kind-config.yaml
# Every kind target pins this context. Without it a target run before kind-up
# acts on whatever the kubeconfig currently points at, which in practice is a
# real cluster.
KIND_CONTEXT        := kind-$(KIND_CLUSTER)
KIND_NAMESPACE      ?= yk-dns-manager-system
FAKE_IMAGE          ?= fake-opnsense:local
# Must match the sigs.k8s.io/gateway-api version in go.mod: the controller is
# compiled against these types, and CRDs two minor versions behind silently
# prune fields the reconciler reads.
GATEWAY_API_VERSION ?= v1.6.3

LOCALBIN              ?= $(shell pwd)/bin
# golangci-lint refuses to load a module whose Go version is newer than the Go
# it was built with: v2.11.4 (go1.26.1) rejects a go1.27 module outright.
# v2.14.0 is built with go1.27.0. Keep this >= the go.mod go directive.
GOLANGCI_LINT_VERSION ?= v2.14.0

VERSION_PKG := github.com/yuriy-kovalchuk/yk-dns-manager/internal/version

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

# ── Default ────────────────────────────────────────────────────────────────────
.DEFAULT_GOAL := all

.PHONY: all tidy deps-check fmt vet lint build run test test-unit test-integration test-cover vuln clean docker-build docker-push docker-build-local fake-opnsense kind-fake-install kind-fake-delete helm-lint helm-template helm-package helm-push kind-up kind-down kind-load kind-secret kind-deploy kind-undeploy kind-reload kind-logs install-hooks help

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

## run: build and run the primary binary locally
run:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && go run -ldflags "$(LDFLAGS)" ./cmd/$(APP_NAME) --zap-log-level=debug

# ── Testing ────────────────────────────────────────────────────────────────────
## test: run all tests (unit + integration)
test: test-unit test-integration

## test-unit: run unit tests with coverage profile
test-unit:
	go test -race -timeout 120s -v -count=1 -coverprofile=coverage.txt ./internal/...

## test-cover: run tests, print function summary, generate HTML report
test-cover:
	go test -v -race -count=1 -coverprofile=coverage.txt ./internal/...
	go tool cover -html=coverage.txt -o coverage.html

## test-integration: run integration tests
test-integration:
	go test -race -timeout 120s -v -count=1 ./test/integration/

## vuln: check for known vulnerabilities (also runs in CI)
# `go run` resolves through this module's toolchain, so govulncheck is built
# with the go.mod go version. A globally installed govulncheck is pinned to
# whatever Go built it and refuses to load a newer module.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

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
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMG)

## docker-push: multi-platform build and push to registry
# One buildx step: `docker push` cannot publish the multi-arch manifest that
# a multi-platform build produces.
docker-push:
	$(MAKE) buildx-setup
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMG) \
		--push

# ── Helm ───────────────────────────────────────────────────────────────────────
## helm-lint: lint the chart (strict)
helm-lint:
	helm lint --strict $(CHART_DIR)

## helm-template: render the chart with default values as a smoke test
helm-template:
	helm template smoke $(CHART_DIR)

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
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):local .

kind-up:
	kind get clusters | grep -q "^$(KIND_CLUSTER)$$" || \
		kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG)

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build-local
	kind load docker-image $(IMAGE):local --name $(KIND_CLUSTER)

## fake-opnsense: serve the in-memory OPNsense API locally for `make run`
fake-opnsense:
	go run ./hack/fake-opnsense --addr=:8053

## kind-fake-install: run the fake OPNsense inside the kind cluster
kind-fake-install:
	docker build -f Dockerfile.fake -t $(FAKE_IMAGE) .
	kind load docker-image $(FAKE_IMAGE) --name $(KIND_CLUSTER)
	kubectl --context $(KIND_CONTEXT) --namespace $(KIND_NAMESPACE) apply -f hack/fake-opnsense-deployment.yaml

kind-fake-delete:
	kubectl --context $(KIND_CONTEXT) --namespace $(KIND_NAMESPACE) delete -f hack/fake-opnsense-deployment.yaml --ignore-not-found

kind-secret:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && \
		kubectl --context $(KIND_CONTEXT) create namespace $(KIND_NAMESPACE) --dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f - && \
		kubectl --context $(KIND_CONTEXT) create secret generic yk-dns-manager-credentials \
			--namespace $(KIND_NAMESPACE) \
			--from-literal=OPNSENSE_API_KEY=$${OPNSENSE_API_KEY} \
			--from-literal=OPNSENSE_API_SECRET=$${OPNSENSE_API_SECRET} \
			--dry-run=client -o yaml | kubectl --context $(KIND_CONTEXT) apply -f -

kind-deploy: kind-up kind-load kind-secret
	kubectl --context $(KIND_CONTEXT) apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml
	helm --kube-context $(KIND_CONTEXT) upgrade --install $(APP_NAME) $(CHART_DIR) \
		--namespace $(KIND_NAMESPACE) \
		--create-namespace \
		-f hack/local-values.yaml \
		--wait

kind-undeploy:
	helm --kube-context $(KIND_CONTEXT) uninstall $(APP_NAME) --namespace $(KIND_NAMESPACE) --ignore-not-found

kind-reload: kind-load
	kubectl --context $(KIND_CONTEXT) rollout restart deployment/$(APP_NAME) --namespace $(KIND_NAMESPACE)

kind-logs:
	kubectl --context $(KIND_CONTEXT) logs --namespace $(KIND_NAMESPACE) \
		-l app.kubernetes.io/name=$(APP_NAME) \
		--follow

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
