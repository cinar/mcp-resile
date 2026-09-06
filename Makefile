.PHONY: build test lint

BINARY := bin/mcp-resile

build:
	CGO_ENABLED=0 go build -o $(BINARY) ./cmd/mcp-resile

test:
	go test -race ./...

lint:
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping (go vet already ran)"; \
	fi
