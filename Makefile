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

.PHONY: all build build-agent test vet lint e2e mock conformance integration bench clean install fmt check

all: check build

build: ## build the waldo binary
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/waldo

# The tier-3 helper for this platform. waldo finds it beside its own binary, so
# installing both means the agent tier works against a same-platform target
# without a cross-compile. Other platforms are cross-built on demand.
build-agent: ## build the optional tier-3 helper for this platform
	$(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o $(BIN)-agent ./cmd/waldo-agent

install: build build-agent ## install to ~/.local/bin
	install -d $(HOME)/.local/bin
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(BIN)
	install -m 0755 $(BIN)-agent $(HOME)/.local/bin/$(BIN)-agent
	@echo "installed $(HOME)/.local/bin/$(BIN)"

test: ## unit and integration tests (no model tokens spent)
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

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
	rm -f $(BIN) $(BIN)-agent
	$(GO) clean -testcache
