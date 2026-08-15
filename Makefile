# waldo — teleoperation for coding agents
GO      ?= go
BIN     := waldo
VERSION := $(shell sed -n 's/.*Version = "\(.*\)".*/\1/p' internal/waldo/types.go)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

.PHONY: all build test vet lint e2e mock conformance clean install fmt check

all: check build

build: ## build the waldo binary
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/waldo

install: build ## install to ~/.local/bin
	install -d $(HOME)/.local/bin
	install -m 0755 $(BIN) $(HOME)/.local/bin/$(BIN)
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

clean:
	rm -f $(BIN)
	$(GO) clean -testcache
