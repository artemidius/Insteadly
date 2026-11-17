BIN      := bin/insteadly
VERSION  ?= dev

.PHONY: build run test live-test live-test-mistral live-test-m0 live-test-m0-phone lint fmt check-deps verify cover clean clean-data

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/insteadly

run:
	go run ./cmd/insteadly

test:
	go test -race -count=1 ./...

live-test:
	go test -tags live -count=1 -run TestLiveCall -v ./internal/app

live-test-mistral:
	go test -tags live -count=1 -run '^TestLiveMistral$$' -v ./internal/app

live-test-m0:
	go test -tags live -count=1 -run '^TestLiveM0NoPhone$$' -v ./internal/app

live-test-m0-phone:
	go test -tags live -count=1 -run '^TestLiveM0Phone$$' -v ./internal/app

cover:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

verify: lint test check-deps build

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
