# syntax=docker/dockerfile:1
# Multi-stage build: the Go toolchain runs on the build host's platform and
# cross-compiles for the target (no QEMU); the result is a static binary on
# distroless (no shell, runs as uid 65532). One Dockerfile builds every service:
# --build-arg CMD=api (default, demo-api) or CMD=payments (payments-svc).

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
WORKDIR /src
# Override when proxy.golang.org is unreachable, e.g. --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
# Optional extra CA for networks that intercept TLS (pass with --secret id=extra_ca,src=<pem>).
# It is used only for this download step and never lands in an image layer.
RUN --mount=type=secret,id=extra_ca \
    --mount=type=cache,target=/go/pkg/mod \
    if [ -s /run/secrets/extra_ca ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; \
      export SSL_CERT_FILE=/tmp/ca.pem; \
    fi; \
    go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG CMD=api
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
