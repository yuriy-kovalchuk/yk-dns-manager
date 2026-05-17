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

LDFLAGS := -s -w -X main.Version=$(VERSION)

# ── Default ────────────────────────────────────────────────────────────────────
.DEFAULT_GOAL := all

.PHONY: all tidy deps-check fmt vet lint build run \
        test test-unit test-integration \
        clean \
        docker-build docker-push docker-buildx docker-build-local \
        helm-package helm-push \
        kind-up kind-down kind-load kind-secret kind-deploy kind-undeploy kind-reload kind-logs \
        install-hooks

all: tidy fmt vet build

# ── Development ────────────────────────────────────────────────────────────────
tidy:
	go mod tidy

deps-check:
	@go list -u -m -f '{{if and (not .Indirect) .Update}}{{.Path}}  {{.Version}} → {{.Update.Version}}{{end}}' all | grep -v "^$$" || echo "All direct dependencies are up to date."

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: $(LOCALBIN)/golangci-lint
	$(LOCALBIN)/golangci-lint run --timeout=5m

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/$(APP_NAME) ./cmd/$(APP_NAME)

run:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && go run -ldflags "$(LDFLAGS)" ./cmd/$(APP_NAME) --zap-log-level=debug

# ── Testing ────────────────────────────────────────────────────────────────────
test: test-unit test-integration

test-unit:
	go test -v -race -count=1 -coverprofile=coverage.txt ./internal/...

test-integration:
	go test -v -race -count=1 ./test/integration/

# ── Cleanup ────────────────────────────────────────────────────────────────────
clean:
	rm -rf $(LOCALBIN) *.tgz coverage.txt

# ── Docker ─────────────────────────────────────────────────────────────────────
docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMG) .

docker-push: docker-build
	docker push $(IMG)

docker-buildx:
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMG) \
		--push .

# ── Helm ───────────────────────────────────────────────────────────────────────
helm-package:
	helm package $(CHART_DIR)

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
install-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed — tests and commit messages will be checked before every push."

# ── Tools ──────────────────────────────────────────────────────────────────────
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

$(LOCALBIN)/golangci-lint: $(LOCALBIN)
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | \
		sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)
