# ─────────────────────────────────────────
# Stage 1: Builder
# ─────────────────────────────────────────
FROM golang:1.26.0-alpine AS builder

ARG VERSION=dev

WORKDIR /app

# download dependencies (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# copy proto gen output
COPY gen/ gen/

# copy source code
COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /kubexa-agent \
    ./cmd/agent

# ─────────────────────────────────────────
# Stage 2: Runtime
# ─────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /kubexa-agent /kubexa-agent

USER nonroot:nonroot

ENTRYPOINT ["/kubexa-agent"]