FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG LDFLAGS

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "$LDFLAGS" -o /yk-dns-manager ./cmd/yk-dns-manager

FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.source="https://github.com/yuriy-kovalchuk/yk-dns-manager"

COPY --from=builder /yk-dns-manager /yk-dns-manager

USER 65532:65532

EXPOSE 9090

ENTRYPOINT ["/yk-dns-manager"]
