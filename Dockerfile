# syntax=docker/dockerfile:1

# ─── Build ────────────────────────────────────────────────────────────────────
# Go patch pin must stay equal to the go.mod `go` directive and the CI setup-go
# `go-version`: govulncheck judges the stdlib of the toolchain that builds the
# image, so a floating minor silently reintroduces fixed CVEs. Keep the
# golangci-lint pin in the Makefile built with at least this Go — it refuses
# modules newer than its own build.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/yuriy-kovalchuk/yk-dns-manager/internal/version.Version=${VERSION}" \
    -o /yk-dns-manager ./cmd/yk-dns-manager

# ─── Runtime ─────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static@sha256:448810357cc8e67b7208988abdba0f5ec3d3aebee0a5b13eb624979792cffc0f

LABEL org.opencontainers.image.title="yk-dns-manager" \
      org.opencontainers.image.description="Kubernetes controller that manages DNS records for Gateway API HTTPRoutes" \
      org.opencontainers.image.source="https://github.com/yuriy-kovalchuk/yk-dns-manager" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /yk-dns-manager /yk-dns-manager

USER 65532:65532

EXPOSE 9090

ENTRYPOINT ["/yk-dns-manager"]
