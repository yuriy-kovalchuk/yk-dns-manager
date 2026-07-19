# syntax=docker/dockerfile:1

# ─── Build ────────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/yuriy-kovalchuk/yk-dns-manager/internal/version.Version=${VERSION} \
      -X github.com/yuriy-kovalchuk/yk-dns-manager/internal/version.Commit=${COMMIT} \
      -X github.com/yuriy-kovalchuk/yk-dns-manager/internal/version.BuildDate=${BUILD_DATE}" \
    -o /yk-dns-manager ./cmd/yk-dns-manager

# ─── Runtime ─────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static@sha256:9197324ba51d9cd071af8505989365c006adf9d6d2067eada25aef00abbb5278

LABEL org.opencontainers.image.title="yk-dns-manager" \
      org.opencontainers.image.description="Kubernetes controller that manages DNS records for Gateway API HTTPRoutes" \
      org.opencontainers.image.source="https://github.com/yuriy-kovalchuk/yk-dns-manager" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /yk-dns-manager /yk-dns-manager

USER 65532:65532

EXPOSE 9090

ENTRYPOINT ["/yk-dns-manager"]
