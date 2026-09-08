SHELL := /bin/sh

VERSION ?= dev
BINARY ?= bin/fixik-next-mobile-gateway
IMAGE ?= homelab-telegram-panel:local
GO_LDFLAGS := -buildid= -s -w -X github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo.Version=$(VERSION)

.PHONY: all fmt vet test web-install web-quality web-check build quality image
all: quality

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; exit 1; }

vet:
	go vet ./...

test:
	go test -race ./...

web-install:
	cd web/mobile-workspace && npm ci --no-audit --no-fund

web-check:
	bash scripts/check-mobile-workspace-dist.sh

web-quality: web-install
	cd web/mobile-workspace && npm run lint
	cd web/mobile-workspace && npm run typecheck
	cd web/mobile-workspace && npm test
	$(MAKE) web-check

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '$(GO_LDFLAGS)' -o $(BINARY) ./cmd/fixik-next-mobile-gateway

quality: fmt vet test web-quality build

image:
	docker build --build-arg VERSION='$(VERSION)' -t '$(IMAGE)' .
