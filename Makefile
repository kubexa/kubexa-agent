BINARY_NAME     := kubexa-agent
MODULE          := github.com/kubexa/kubexa-agent
PROTO_DIR       := proto
GEN_DIR         := gen
PROTO_FILE      := $(PROTO_DIR)/agent/v1/agent.proto

GO_BUILD_FLAGS  := -ldflags="-s -w"
DOCKER_IMAGE    := kubexa/kubexa-agent
DOCKER_TAG      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

.PHONY: all build test lint proto gen clean docker-build helm-lint run run-local run-dev-grpc help

all: proto build

## ─────────────────────────────────────────
## Proto
## ─────────────────────────────────────────

proto: ## generate protobuf files using protoc
	@echo "→ generating proto..."
	@mkdir -p $(GEN_DIR)/agent/v1
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(GEN_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=$(GEN_DIR) \
		--go-grpc_opt=paths=source_relative \
		agent/v1/agent.proto
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
		--build-arg VERSION=$(DOCKER_TAG) \
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

helm-template: ## run helm template (debug)
	helm template kubexa-agent helm/kubexa-agent \
		--set agent.clusterId=local-dev \
		--set agent.backend.host=localhost \
		--set agent.backend.port=50051 \
		--set agent.backend.tls=false

helm-package: ## package helm chart
	@mkdir -p dist
	helm package helm/kubexa-agent -d dist
	@echo "✓ chart packaged to dist/"

## ─────────────────────────────────────────
## Clean
## ─────────────────────────────────────────

clean: ## clean build artifacts
	rm -rf bin/ dist/
	@echo "✓ cleaned"

## ─────────────────────────────────────────
## Help
## ─────────────────────────────────────────

help: ## show help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'