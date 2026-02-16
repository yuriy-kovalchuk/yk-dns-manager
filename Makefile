APP_NAME := yk-dns-manager
BUILD_DIR := bin
REGISTRY ?= ghcr.io/yuriy-kovalchuk

# Extract versions from Chart.yaml
CHART_VERSION := $(shell grep '^version:' charts/$(APP_NAME)/Chart.yaml | awk '{print $$2}')
APP_VERSION   := $(shell grep '^appVersion:' charts/$(APP_NAME)/Chart.yaml | awk '{print $$2}' | tr -d '"')

# Image uses appVersion, Helm push uses chart version
IMG ?= $(REGISTRY)/$(APP_NAME):$(APP_VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build run test test-unit test-integration clean fmt vet generate docker-build docker-push docker-buildx helm-package helm-push

LDFLAGS := -X main.Version=$(APP_VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/$(APP_NAME)

run:
	@test -f .env || (echo "Missing .env file — copy from .env.example" && exit 1)
	@set -a && . ./.env && set +a && go run -ldflags "$(LDFLAGS)" ./cmd/$(APP_NAME) --zap-log-level=debug

test:
	go test ./...

test-unit:
	go test ./internal/...

test-integration:
	go test ./test/integration/ -v

clean:
	rm -rf $(BUILD_DIR) *.tgz

fmt:
	go fmt ./...

vet:
	go vet ./...

generate:
	go generate ./...

docker-build:
	docker build --build-arg LDFLAGS="$(LDFLAGS)" -t $(IMG) .

docker-push:
	docker push $(IMG)

docker-buildx:
	docker buildx build --platform $(PLATFORMS) --build-arg LDFLAGS="$(LDFLAGS)" -t $(IMG) --push .

helm-package:
	helm package charts/$(APP_NAME)

helm-push: helm-package
	helm push $(APP_NAME)-$(CHART_VERSION).tgz oci://$(REGISTRY)/$(APP_NAME)-chart
