# ─────────────────────────────────────────
# Stage 1: Builder
# ─────────────────────────────────────────
FROM golang:1.26.0-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY proto/gen/go/ proto/gen/go/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.Version=${VERSION} \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.Commit=${COMMIT} \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /kubexa-agent \
    ./cmd/agent

# ─────────────────────────────────────────
# Stage 2: Runtime
# ─────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /kubexa-agent /kubexa-agent

USER nonroot:nonroot

ENTRYPOINT ["/kubexa-agent"]
