.PHONY: build test lint bench loadtest cover doc generate clean fmt vet check

# Force bash as shell (required on Windows for || true, rm -f, etc.)
SHELL := bash

# Variables
GO ?= go
GOLANGCI_LINT ?= golangci-lint
GOFLAGS ?=
COVERPROFILE ?= coverage.out
PKG := ./...

## build: Build all binaries
build:
	$(GO) build $(GOFLAGS) $(PKG)

## test: Run all tests
test:
	$(GO) test $(GOFLAGS) -race -count=1 $(PKG)

## lint: Run golangci-lint
lint:
	$(GOLANGCI_LINT) run $(PKG)

## fmt: Format all Go files
fmt:
	$(GO) fmt $(PKG)

## vet: Run go vet
vet:
	$(GO) vet $(PKG)

## bench: Run benchmarks
bench:
	$(GO) test $(GOFLAGS) -bench=. -benchmem $(PKG)

## loadtest: Run HTTP load tests against examples (requires hey)
loadtest:
	bash scripts/loadtest.sh

## cover: Generate coverage report
cover:
	$(GO) test $(GOFLAGS) -race -coverprofile=$(COVERPROFILE) -covermode=atomic $(PKG)
	$(GO) tool cover -html=$(COVERPROFILE) -o coverage.html
	@echo "Coverage report: coverage.html"

## doc: Serve godoc locally on port 6060
doc:
	@echo "Documentation at http://localhost:6060/pkg/github.com/credo-go/credo/"
	godoc -http=:6060

## generate: Run go generate
generate:
	$(GO) generate $(PKG)

## check: Run all quality gates (vet + lint + test + bench)
check:
	@echo "=== vet ==="
	$(GO) vet $(PKG)
	@echo "=== lint ==="
	-$(GOLANGCI_LINT) run $(PKG)
	@echo "=== test ==="
	$(GO) test $(GOFLAGS) -race -count=1 $(PKG)
	@echo "=== bench ==="
	$(GO) test $(GOFLAGS) -bench=. -benchmem -run='^$$' $(PKG)
	@echo "=== all checks passed ==="

## clean: Remove build artifacts
clean:
	$(GO) clean
	rm -f $(COVERPROFILE) coverage.html

## help: Show this help
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
