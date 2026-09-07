.PHONY: build test lint

BINARY := bin/mcp-resile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	CGO_ENABLED=0 go build -ldflags "-X github.com/cinar/mcp-resile/internal/version.Version=$(VERSION)" -o $(BINARY) ./cmd/mcp-resile

test:
	go test -race ./...

lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping (go vet already ran)"; \
	fi
