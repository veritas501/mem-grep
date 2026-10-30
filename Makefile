# mem-grep — the only build entry point.
#
# Version identity has exactly one source, git, and is injected into package-level variables in
# internal/version with -ldflags -X (nothing is generated, no constant is edited). Builds without
# git metadata report "dev"; a plain `go run` / `go build` reports dev / none / unknown.

# mg: mem-grep, rg-style search over agent sessions with no index, model or network.
BINARIES := mg
MODULE   := github.com/veritas501/mem-grep
BIN_DIR  := bin
PREFIX  ?= $(HOME)/.local/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LD_FLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) \
            -X $(MODULE)/internal/version.Commit=$(COMMIT) \
            -X $(MODULE)/internal/version.Built=$(BUILT)
GO_FLAGS := -trimpath -ldflags "-s -w $(LD_FLAGS)"

# Docker image tag is independent of the injected version.
IMAGE     ?= mem-grep
IMAGE_TAG ?= dev

# `make run` / `make dev` run BIN with ARGS, e.g. make run ARGS="docker 网络".
BIN  ?= mg
ARGS ?=

.PHONY: all build install run test test-race vet fmt lint ci version clean docker bump help

all: lint test build

help: ## List targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## Build bin/mg with the version injected
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do echo "go build -o $(BIN_DIR)/$$b ./cmd/$$b"; go build $(GO_FLAGS) -o $(BIN_DIR)/$$b ./cmd/$$b || exit 1; done

install: build ## Install to $(PREFIX) (default ~/.local/bin)
	@mkdir -p $(PREFIX)
	@for b in $(BINARIES); do install -m 0755 $(BIN_DIR)/$$b $(PREFIX)/$$b && $(PREFIX)/$$b version; done

run: ## Run mg from source with the version injected: make run ARGS="docker 网络"
	go run $(GO_FLAGS) ./cmd/$(BIN) $(ARGS)

test: ## Run tests (no -ldflags, so version defaults are tested)
	go test ./...

test-race: ## Run tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go code
	gofmt -w cmd internal

lint: ## gofmt + go vet (+ golangci-lint when installed)
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

ci: ## Run the checks CI runs locally (lint + race tests)
	$(MAKE) lint
	$(MAKE) test-race

version: build ## Print the identity of the freshly built binary
	@for b in $(BINARIES); do $(BIN_DIR)/$$b version; done

clean: ## Remove build output
	rm -rf $(BIN_DIR)

docker: ## Build the image; version comes in as build args, tag is $(IMAGE):$(IMAGE_TAG)
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILT=$(BUILT) \
	  -t $(IMAGE):$(IMAGE_TAG) .

# Release: commit pending changes, then a version-only release commit, then an annotated tag.
# Never pushes. BUMP=major|minor|patch or BUMP=v1.2.3.
BUMP ?= patch
bump: ## Cut a release: make bump BUMP=patch|minor|major|vX.Y.Z (does not push)
	scripts/bump-version.sh $(BUMP)
