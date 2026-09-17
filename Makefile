SHELL := /bin/sh

VERSION ?= dev
BINARY ?= bin/fixik-next-mobile-gateway
AGENT_SERVICE_BINARY ?= bin/agent-service
DOCKER_ADAPTER_BINARY ?= bin/homelab-docker-adapter
IMAGE ?= homelab-telegram-panel:local
GO_LDFLAGS := -buildid= -s -w -X github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo.Version=$(VERSION)
AGENT_SERVICE_LDFLAGS := -buildid= -s -w -X main.version=$(VERSION)
DOCKER_ADAPTER_LDFLAGS := -buildid= -s -w -X main.version=$(VERSION)

.PHONY: all fmt vet test harness-quality agentservice-quality agentservice-integration agentservice-build dockeradapter-build sdk-test release-test web-install web-quality web-check build quality image
all: quality

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

vet:
	go vet ./...

test:
	go test -race ./...

harness-quality:
	@test -z "$$(gofmt -l harness)" || { gofmt -l harness; exit 1; }
	cd harness && go vet ./...
	cd harness && go test -race ./...

agentservice-quality:
	@test -z "$$(gofmt -l agentservice)" || { gofmt -l agentservice; exit 1; }
	cd agentservice && GOWORK=off go vet ./...
	cd agentservice && GOWORK=off go test -race ./...

agentservice-integration:
	@test -n "$${AGENT_SERVICE_TEST_DATABASE_URL:-}" || { echo 'AGENT_SERVICE_TEST_DATABASE_URL is required'; exit 2; }
	cd agentservice && GOWORK=off go test ./... -count=1

agentservice-build:
	cd agentservice && GOWORK=off CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='$(AGENT_SERVICE_LDFLAGS)' -o ../$(AGENT_SERVICE_BINARY) ./cmd/agent-service

dockeradapter-build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='$(DOCKER_ADAPTER_LDFLAGS)' -o $(DOCKER_ADAPTER_BINARY) ./cmd/homelab-docker-adapter

web-install:
	cd web/mobile-workspace && npm ci --no-audit --no-fund

web-check:
	bash scripts/check-mobile-workspace-dist.sh

web-quality: web-install
	node api/check-harness-v1-schema.mjs
	node api/check-history-replica-v1.mjs
	node api/generate-transcript-view-v1.mjs --check
	node api/check-transcript-view-v1.mjs
	node api/check-agent-contracts.mjs
	node web/mobile-workspace/scripts/generate-harness-types.mjs --check
	node web/mobile-workspace/scripts/generate-transcript-types.mjs --check
	cd web/mobile-workspace && npm run lint
	cd web/mobile-workspace && npm run typecheck
	cd web/mobile-workspace && npm test
	$(MAKE) web-check

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '$(GO_LDFLAGS)' -o $(BINARY) ./cmd/fixik-next-mobile-gateway

sdk-test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s backend -v

release-test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_*.py' -v

quality: fmt vet test harness-quality agentservice-quality sdk-test release-test web-quality build agentservice-build dockeradapter-build

image:
	docker build --build-arg VERSION='$(VERSION)' -t '$(IMAGE)' .
