# goga — developer Makefile.
#
# This file doubles as the template goga ships: downstream projects are expected
# to copy it and adjust only the variables at the top. Everything below the
# variables is deliberately POSIX-portable (/bin/sh, no bashisms) and has no
# dependency beyond the Go toolchain itself — golangci-lint is the single
# optional extra and every target that needs it degrades with a clear message.

# ---------------------------------------------------------------------------
# Variables — the only part a downstream project should need to edit.
# ---------------------------------------------------------------------------

GO              ?= go
# Comma-separated build tags. Load-bearing: a file behind `//go:build wireinject`
# is invisible to the default build, so vet and the linter never see it unless
# the tag is named here. Keep this list in sync with .github/workflows/ci.yml.
BUILD_TAGS      ?=
PKGS            ?= ./...
COVERAGE_FILE   ?= coverage.out
COVERAGE_HTML   ?= coverage.html
GOLANGCI        ?= golangci-lint
DIST_DIR        ?= dist

# gofmt is NOT toolchain-switched: GOTOOLCHAIN=auto redirects the `go` command
# only, so a bare `gofmt` on PATH stays whatever the developer's installed Go
# shipped. On goga's 1.27 floor a 1.26 gofmt cannot parse the generic methods in
# goga/registry and fails with "method must have no type parameters" on
# perfectly clean code. Ask the go command which toolchain it resolved and use
# that one's gofmt, falling back to PATH if the binary is not where GOROOT says.
GOFMT           ?= $(shell f="$$($(GO) env GOROOT)/bin/gofmt"; if [ -x "$$f" ]; then echo "$$f"; else echo gofmt; fi)

# Expand BUILD_TAGS into a flag only when it is non-empty, so an empty value
# does not turn into a bare `-tags=` that some tool versions reject.
TAGFLAG         := $(if $(BUILD_TAGS),-tags=$(BUILD_TAGS),)
LINT_TAGFLAG    := $(if $(BUILD_TAGS),--build-tags=$(BUILD_TAGS),)

# Files considered by the gofmt gate: every tracked .go file except generated
# trees that projects conventionally exclude.
GOFILES         := $(shell find . -type f -name '*.go' -not -path './vendor/*' -not -path './$(DIST_DIR)/*')

SHELL           := /bin/sh

.DEFAULT_GOAL   := help

.PHONY: help build test lint fmt fmtcheck vet generate tools tidy cover clean

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------

help: ## Show this help
	@echo 'goga — available targets:'
	@echo ''
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo ''
	@echo 'Variables: BUILD_TAGS, PKGS, COVERAGE_FILE (see the top of the Makefile).'

build: ## Compile every package (no binaries are produced for a library)
	$(GO) build $(TAGFLAG) $(PKGS)

test: ## Run tests with the race detector and atomic coverage
	$(GO) test $(TAGFLAG) -race -covermode=atomic -coverprofile=$(COVERAGE_FILE) $(PKGS)

lint: fmtcheck vet ## Run the full lint suite: gofmt gate, go vet, golangci-lint
	@if command -v $(GOLANGCI) >/dev/null 2>&1; then \
		$(GOLANGCI) run $(LINT_TAGFLAG); \
	else \
		echo 'golangci-lint not found on PATH (gofmt and vet did run).'; \
		echo 'Install it: https://golangci-lint.run/welcome/install/'; \
		exit 1; \
	fi

fmt: ## Rewrite every Go file with gofmt
	$(GOFMT) -s -w $(GOFILES)

fmtcheck: ## Fail if any Go file is not gofmt-clean
	@out=`$(GOFMT) -s -l $(GOFILES)`; \
	if [ -n "$$out" ]; then \
		echo 'These files are not gofmt-clean (run `make fmt`):'; \
		echo "$$out"; \
		exit 1; \
	fi

vet: ## Run go vet
	$(GO) vet $(TAGFLAG) $(PKGS)

generate: ## Run every code generator (go:generate directives)
	# Generators are pinned as `tool` directives in go.mod and invoked from
	# `//go:generate go tool <name>` lines, so `go generate` covers all of them
	# and no generator has to be installed globally. See `make tools`.
	$(GO) generate $(TAGFLAG) $(PKGS)

tools: ## List the generators pinned as go.mod tool directives
	$(GO) tool

tidy: ## Tidy and verify go.mod / go.sum
	$(GO) mod tidy
	$(GO) mod verify

cover: test ## Run tests and open an HTML coverage report
	$(GO) tool cover -func=$(COVERAGE_FILE) | tail -n 1
	$(GO) tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@echo "HTML coverage report written to $(COVERAGE_HTML)"

clean: ## Remove build and coverage output
	$(GO) clean -cache -testcache 2>/dev/null || true
	rm -rf $(DIST_DIR) $(COVERAGE_FILE) $(COVERAGE_HTML)
