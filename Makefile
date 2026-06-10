BINARY_NAME     := kubexa-agent
MODULE          := github.com/kubexa/kubexa-agent
PROTO_DIR       := proto
GEN_DIR         := proto/gen/go
PROTO_FILE      := $(PROTO_DIR)/agent/v1/agent.proto
PROTO_FILES     := $(PROTO_DIR)/agent/v1/*.proto $(PROTO_DIR)/common/v1/*.proto

VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT          ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME      ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS         := -s -w \
	-X github.com/kubexa/kubexa-agent/pkg/buildinfo.Version=$(VERSION) \
	-X github.com/kubexa/kubexa-agent/pkg/buildinfo.Commit=$(COMMIT) \
	-X github.com/kubexa/kubexa-agent/pkg/buildinfo.BuildTime=$(BUILD_TIME)
GO_BUILD_FLAGS  := -ldflags="$(LDFLAGS)"
DOCKER_IMAGE    := kubexa/kubexa-agent
DOCKER_TAG      := $(VERSION)

.PHONY: all build test lint proto gen clean docker-build helm-lint helm-template helm-package helm-push-oci run run-local run-dev run-dev-grpc help

all: proto build

## ─────────────────────────────────────────
## Proto
## ─────────────────────────────────────────

proto: ## generate protobuf files using protoc
	@echo "→ generating proto..."
	@rm -rf gen
	@mkdir -p $(GEN_DIR)/agent/v1 $(GEN_DIR)/common/v1
	protoc \
		--proto_path=. \
		--go_out=. \
		--go_opt=module=$(MODULE) \
		--go-grpc_out=. \
		--go-grpc_opt=module=$(MODULE) \
		$(PROTO_FILES)
	@echo "✓ proto generated"

## ─────────────────────────────────────────
## Build
## ─────────────────────────────────────────

build: ## compile binary
	@echo "→ building $(BINARY_NAME)..."
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -o bin/$(BINARY_NAME) ./cmd/agent
	@echo "✓ bin/$(BINARY_NAME)"

run: ## run locally (uses ./config/config.yaml or /etc/kubexa/config.yaml unless you pass flags)
	go run ./cmd/agent

run-local: ## run agent with config/example-local.yaml
	go run ./cmd/agent --config=config/example-local.yaml

run-dev: ## run agent in dev mode (debug logs, localhost bind, kubeconfig)
	go run -ldflags="$(LDFLAGS)" ./cmd/agent --dev --config=config/example-local.yaml

run-dev-grpc: ## run local AgentService mock (insecure gRPC on 127.0.0.1:50051)
	go run ./cmd/dev-grpc-server

## ─────────────────────────────────────────
## Test & Lint
## ─────────────────────────────────────────

test: ## run tests
	go test ./... -v -race -count=1

lint: ## run golangci-lint
	golangci-lint run ./...

## ─────────────────────────────────────────
## Docker
## ─────────────────────────────────────────

docker-build: ## build docker image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(DOCKER_IMAGE):$(DOCKER_TAG) \
		-t $(DOCKER_IMAGE):latest \
		.
	@echo "✓ $(DOCKER_IMAGE):$(DOCKER_TAG)"

docker-push: ## push docker image
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)
	docker push $(DOCKER_IMAGE):latest

## ─────────────────────────────────────────
## Helm
## ─────────────────────────────────────────

helm-lint: ## run helm chart lint
	helm lint helm/kubexa-agent

helm-template: ## render chart manifests (debug)
	helm template kubexa-agent helm/kubexa-agent \
		--namespace kubexa \
		--set secret.tenantToken=dev-token \
		--set gateway.address=127.0.0.1:50051 \
		--set gateway.tls=false \
		--set persistence.enabled=false

helm-package: ## package helm chart to dist/
	@mkdir -p dist
	helm package helm/kubexa-agent -d dist \
		--version $(shell grep '^version:' helm/kubexa-agent/Chart.yaml | awk '{print $$2}')
	@echo "✓ chart packaged to dist/"

helm-push-oci: helm-package ## push chart to GHCR OCI (requires helm registry login)
	helm push dist/kubexa-agent-*.tgz oci://ghcr.io/kubexa/charts

## ─────────────────────────────────────────
## Clean
## ─────────────────────────────────────────

clean: ## clean build artifacts
	rm -rf bin/ dist/ gen/ $(GEN_DIR)/
	@echo "✓ cleaned"

## ─────────────────────────────────────────
## Help
## ─────────────────────────────────────────

help: ## show help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'