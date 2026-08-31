SHELL := /bin/bash
.DEFAULT_GOAL := help

BIN       := bin/llm-proxy
PKG       := ./cmd/llm-proxy
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
CONFIG    ?= llm-proxy.yaml

GOLANGCI  := $(shell command -v golangci-lint 2>/dev/null)

## help: list targets
.PHONY: help
help:
	@echo "llm-proxy — OpenAI-compatible wire-level debugging proxy"
	@echo
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort
	@echo

## build: compile the static binary into bin/
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## run: build then run against $(CONFIG)
.PHONY: run
run: build
	./$(BIN) -config $(CONFIG)

## run-debug: run with full request/response tracing
.PHONY: run-debug
run-debug: build
	./$(BIN) -config $(CONFIG) -log-level debug -full-trace

## test: run the ENTIRE test suite (the no-regressions gate)
.PHONY: test
test:
	go test ./...

## test-race: full suite under the race detector, no cache
.PHONY: test-race
test-race:
	go test -race -count=1 ./...

## test-cover: full suite with a coverage summary
.PHONY: test-cover
test-cover:
	go test -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -n 30

## cover-html: open the coverage report in a browser
.PHONY: cover-html
cover-html: test-cover
	go tool cover -html=cover.out

## bench: benchmark the hot parsing paths
.PHONY: bench
bench:
	go test -bench=. -benchmem ./internal/analyze ./internal/ratelimit

## vet: go vet
.PHONY: vet
vet:
	go vet ./...

## lint: golangci-lint when installed, otherwise go vet (an error under CI)
.PHONY: lint
lint:
ifdef GOLANGCI
	golangci-lint run ./...
else ifdef CI
	@echo "golangci-lint is not installed; the go vet fallback would report a" >&2
	@echo "green lint step that linted almost nothing. Install it in the workflow." >&2
	@exit 1
else
	@echo "golangci-lint not installed; falling back to go vet (make tools to install)"
	@go vet ./...
endif

## fmt: gofmt -s -w
.PHONY: fmt
fmt:
	gofmt -s -w .

## fmt-check: fail if anything needs gofmt
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -s -l . 2>/dev/null); \
	if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

## tidy: go mod tidy
.PHONY: tidy
tidy:
	go mod tidy

## deps: download modules (the only step needing network)
.PHONY: deps
deps:
	go mod download

## check: fmt-check + vet + lint + race tests — proves no regressions
.PHONY: check
check: fmt-check vet lint test-race

## hooks: install the pre-commit secret scan (sets core.hooksPath)
.PHONY: hooks
hooks:
	@git config core.hooksPath .githooks
	@echo "core.hooksPath -> .githooks; bypass one commit with git commit --no-verify"
	@command -v gitleaks >/dev/null || \
		echo "note: gitleaks is not installed, so the hook will warn and pass (brew install gitleaks)"

## secrets: scan the whole history for secrets — the command CI runs
.PHONY: secrets
secrets:
	@command -v gitleaks >/dev/null || { echo "gitleaks not installed: brew install gitleaks"; exit 1; }
	gitleaks git --redact -v --no-banner .

## check-config: validate the config and print the route table, then exit
.PHONY: check-config
check-config: build
	./$(BIN) -config $(CONFIG) -check

## demo: run a fake upstream + the proxy, issue a streaming request
.PHONY: demo
demo: build
	go run ./cmd/demo -scenario clean

## demo-scenarios: drive every failure mode so all console blocks print
.PHONY: demo-scenarios
demo-scenarios: build
	go run ./cmd/demo -scenario all

## dist: cross-compile release binaries into dist/
.PHONY: dist
dist:
	@mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "$(LDFLAGS)" -o dist/llm-proxy-$$os-$$arch $(PKG) || exit 1; \
	done

## install: go install into GOBIN
.PHONY: install
install:
	go install -ldflags "$(LDFLAGS)" $(PKG)

## tools: install golangci-lint
.PHONY: tools
tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

## clean: remove build artifacts
.PHONY: clean
clean:
	rm -rf bin dist cover.out
