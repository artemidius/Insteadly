BIN      := bin/insteadly
VERSION  ?= dev

.PHONY: build run test lint fmt check-deps clean clean-data

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/insteadly

run:
	go run ./cmd/insteadly

test:
	go test -race -count=1 ./...

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not installed — skipped (optional: brew install golangci-lint)"; fi

check-deps:
	@if go list -deps ./internal/core/... | grep -q 'insteadly/internal/adapters'; then \
		echo "FAIL: internal/core imports an adapter — hexagon violated"; exit 1; \
	else echo "OK: core imports no adapters"; fi

fmt:
	gofmt -w .

clean:
	rm -rf bin

clean-data:
	rm -rf data
