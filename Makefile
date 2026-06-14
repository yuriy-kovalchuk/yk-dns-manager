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
GATEWAY_API_VERSION ?= v1.4.1

LOCALBIN              ?= $(shell pwd)/bin
GOLANGCI_LINT_VERSION ?= v2.11.4

VERSION_PKG := github.com/yuriy-kovalchuk/yk-dns-manager/internal/version

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

# ── Default ────────────────────────────────────────────────────────────────────
.DEFAULT_GOAL := all

.PHONY: all tidy deps-check fmt vet lint build run test test-unit test-integration test-cover vuln clean docker-build docker-push helm-package helm-push kind-up kind-down kind-load kind-secret kind-deploy kind-undeploy kind-reload kind-logs install-hooks help

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

## vuln: check for known vulnerabilities (manual/CI only)
vuln:
	govulncheck ./...

# ── Docker ─────────────────────────────────────────────────────────────────────
## docker-build: multi-platform build via buildx (no push)
docker-build:
	$(MAKE) buildx-setup
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMG) \
		--load

## docker-push: multi-platform build and push to registry
docker-push:
	$(MAKE) docker-build
	docker push $(IMG)

# ── Helm ───────────────────────────────────────────────────────────────────────
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

kind-secret:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && \
		kubectl create namespace yk-dns-manager-system --dry-run=client -o yaml | kubectl apply -f - && \
		kubectl create secret generic yk-dns-manager-credentials \
			--namespace yk-dns-manager-system \
			--from-literal=OPNSENSE_API_KEY=$${OPNSENSE_API_KEY} \
			--from-literal=OPNSENSE_API_SECRET=$${OPNSENSE_API_SECRET} \
			--dry-run=client -o yaml | kubectl apply -f -

kind-deploy: kind-up kind-load kind-secret
	kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/$(GATEWAY_API_VERSION)/standard-install.yaml
	helm upgrade --install yk-dns-manager $(CHART_DIR) \
		--namespace yk-dns-manager-system \
		--create-namespace \
		-f hack/local-values.yaml \
		--wait

kind-undeploy:
	helm uninstall yk-dns-manager --namespace yk-dns-manager-system --ignore-not-found

kind-reload: kind-load
	kubectl rollout restart deployment/yk-dns-manager --namespace yk-dns-manager-system

kind-logs:
	kubectl logs --namespace yk-dns-manager-system \
		-l app.kubernetes.io/name=yk-dns-manager \
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
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | \
		sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)

## help: print this help text
help:
	@grep -E '^## ' Makefile | sed 's/^## //' | column -t -s ':'
