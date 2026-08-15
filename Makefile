# waldo — teleoperation for coding agents
GO      ?= go
BIN     := waldo
VERSION := $(shell sed -n 's/.*Version = "\(.*\)".*/\1/p' internal/waldo/types.go)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# The commit's own date rather than the wall clock, so two builds of the same
# commit produce identical binaries.
DATE    := $(shell git log -1 --format=%cd --date=format:%Y-%m-%d 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X main.buildVersion=$(VERSION) \
	-X main.buildCommit=$(COMMIT) \
	-X main.buildDate=$(DATE)

.PHONY: all build build-helper test vet lint e2e mock conformance integration bench clean install fmt check

all: check build

build: ## build the waldo binary
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/waldo

# The helper binary for this platform. waldo finds it beside its own binary, so
# installing both means the agent tier works against a same-platform target
# without a cross-compile. Other platforms are cross-built on demand.
build-helper: ## build the optional helper binary for this platform
	$(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o $(BIN)-helper ./cmd/waldo-helper

install: build build-helper ## install to ~/.local/bin
	install -d $(HOME)/.local/bin
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(BIN)
	install -m 0755 $(BIN)-helper $(HOME)/.local/bin/$(BIN)-helper
	@echo "installed $(HOME)/.local/bin/$(BIN)"

test: ## unit and integration tests (no model tokens spent)
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

lint: ## golangci-lint, the same configuration CI uses
	@command -v golangci-lint >/dev/null || { \
		echo "golangci-lint is not installed:"; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2"; \
		exit 1; }
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

check: vet test ## everything that runs without a network or an API key

e2e: build ## end-to-end tests against real agents (SPENDS MODEL TOKENS)
	./test/e2e/transparency_test.sh

mock: ## verify the mock model server (no API key, no tokens)
	./test/e2e/mockmodel_test.sh

conformance: build ## verify harness seams still have the shape waldo expects
	./test/e2e/conformance_test.sh

integration: ## file-operation tiers against a real sshd (no network, no API key)
	$(GO) test -race -count=1 -tags integration ./test/integration/...

bench: ## measure what each file-operation tier costs (docs/TRANSPORTS.md)
	./test/bench/tiers_bench.sh

clean:
	rm -f $(BIN) $(BIN)-helper
	$(GO) clean -testcache
