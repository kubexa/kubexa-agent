# ─────────────────────────────────────────
# Stage 0: a target-architecture reference
# ─────────────────────────────────────────
# Exists only to give the guard below something whose architecture is GROUND
# TRUTH rather than an ARG. No --platform, so BuildKit resolves it to
# TARGETPLATFORM and the registry serves the child manifest for the
# architecture this build will publish. Nothing is RUN here, so no emulation.
FROM alpine:3.22 AS archref

# ─────────────────────────────────────────
# Stage 1: Builder
# ─────────────────────────────────────────
# Pinned to go.mod's `go 1.26.0`. kubexa-backend's Dockerfile is on 1.26.3;
# the two are deliberately different and neither number should be copied over.
#
# --platform=$BUILDPLATFORM: the compiler runs on the BUILD machine's
# architecture and cross-compiles, instead of running under QEMU in the
# target's. Added 2026-10-13 together with arm64 publishing -- without it a
# two-platform build runs the whole Go toolchain emulated, which is minutes
# rather than seconds. The binary is still the target's (GOARCH below).
FROM --platform=$BUILDPLATFORM golang:1.26.0-alpine AS builder

# buildx fills these and NO DEFAULT MAY BE GIVEN: a default shadows BuildKit's
# real value, so GOARCH below silently falls back to it and the arm64 child
# manifest ends up holding an amd64 binary. That is not hypothetical -- it was
# measured on kubexa-consumer on 2026-10-13, which carried exactly such
# defaults. See the guard at the end of this stage.
ARG TARGETOS
ARG TARGETARCH

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

# GOOS/GOARCH came from the hardcoded `linux`/`amd64` until 2026-10-13. That
# is why this image published a single amd64 manifest: the agent runs in the
# CUSTOMER's cluster, and an amd64-only agent cannot be installed on an arm64
# node pool (Graviton, Ampere) at all.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.Version=${VERSION} \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.Commit=${COMMIT} \
      -X github.com/kubexa/kubexa-agent/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /kubexa-agent \
    ./cmd/agent

# The built binary's architecture must match the architecture this image's
# manifest will claim. No runtime check can see a mismatch: `docker image
# inspect` reports the manifest rather than the contents, and `docker run` on
# an arm64 Mac executes an x86-64 binary through Rosetta without complaint --
# both measured 2026-10-13 on kubexa-consumer.
#
# The reference is deliberately NOT $TARGETARCH. Comparing against it was
# tried and proven useless: if a default shadows BuildKit's value, the build
# and the check read the same wrong value, so the check passes on a
# mismatched image. /archref-bin has no ARG in its provenance. Offset 18 is
# the ELF header's little-endian e_machine; the check never names a value, so
# a third architecture needs no edit here.
COPY --from=archref /bin/busybox /archref-bin
RUN set -eu; \
    want=$(od -An -tx1 -j18 -N2 /archref-bin | tr -d ' \n'); \
    got=$(od -An -tx1 -j18 -N2 /kubexa-agent | tr -d ' \n'); \
    if [ "$got" != "$want" ]; then \
      echo "/kubexa-agent has ELF e_machine $got; this image's target architecture is $want." >&2; \
      echo "Most likely cause: a default on ARG TARGETARCH, or a hardcoded GOARCH." >&2; \
      exit 1; \
    fi; \
    rm -f /archref-bin

# ─────────────────────────────────────────
# Stage 2: Runtime
# ─────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /kubexa-agent /kubexa-agent

USER nonroot:nonroot

ENTRYPOINT ["/kubexa-agent"]
