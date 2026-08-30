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

# The tag guarding the container-backed tests, kept separate from BUILD_TAGS on
# purpose: `make test` must stay untagged so the fast loop never tries to start
# a container. Mirrors INTEGRATION_BUILD_TAGS in .github/workflows/ci.yml.
INTEGRATION_BUILD_TAGS ?= integration
# Go's default -timeout is 10m, calibrated for unit tests. A container suite can
# spend the first minutes of that pulling images before a test body runs, so the
# integration budget is larger — and, unlike no timeout at all, a wedged
# container still fails in minutes. Mirrors the CI action's default.
INTEGRATION_TIMEOUT    ?= 15m
INTEGRATION_COVERAGE_FILE ?= coverage-integration.out
COVERAGE_HTML   ?= coverage.html
GOLANGCI        ?= golangci-lint
DIST_DIR        ?= dist

# The code generators live in their own module (tools/go.mod) rather than in
# `tool` directives in the root go.mod. That is not cosmetic: a tool directive
# is a module *requirement*, and a requirement propagates into every consumer's
# module graph — buf alone drags cel-go and sqlc into projects that import
# nothing but goga/config. See tools/go.mod for the full account. The cost of
# the split is this indirection: the generators are built into TOOLS_BIN and
# put on PATH, because a generator has to resolve its target packages against
# the *root* module, so it cannot simply be run from inside tools/.
TOOLS_DIR       ?= tools
TOOLS_BIN       ?= $(CURDIR)/bin

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

.PHONY: help build test test-integration lint fmt fmtcheck vet generate tools tidy cover clean

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------

help: ## Show this help
	@echo 'goga — available targets:'
	@echo ''
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo ''
	@echo 'Variables: BUILD_TAGS, INTEGRATION_BUILD_TAGS, INTEGRATION_TIMEOUT, PKGS,'
	@echo '           COVERAGE_FILE (see the top of the Makefile).'

build: ## Compile every package (no binaries are produced for a library)
	$(GO) build $(TAGFLAG) $(PKGS)

test: ## Run tests with the race detector and atomic coverage
	$(GO) test $(TAGFLAG) -race -covermode=atomic -coverprofile=$(COVERAGE_FILE) $(PKGS)

test-integration: ## Run the container-backed tests (needs a running Docker daemon)
	# The local counterpart of the go-test-integration CI action, and kept in
	# step with it: same tag, same timeout, same flags. Note that -tags is
	# additive — the untagged tests in $(PKGS) run here too.
	$(GO) test -tags=$(INTEGRATION_BUILD_TAGS) -race -covermode=atomic \
		-coverprofile=$(INTEGRATION_COVERAGE_FILE) \
		-timeout=$(INTEGRATION_TIMEOUT) $(PKGS)

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

generate: tools ## Run every code generator (go:generate directives)
	# `//go:generate <name>` lines name the bare binary, not `go tool <name>`:
	# the generators are not tool directives of this module any more. `tools`
	# above put them in $(TOOLS_BIN), and prepending that to PATH is what makes
	# them resolvable — while `go generate` still runs in the root module, so a
	# generator that loads packages (mockgen, wire) resolves them correctly.
	PATH='$(TOOLS_BIN)':"$$PATH" $(GO) generate $(TAGFLAG) $(PKGS)

tools: ## Build the pinned code generators from $(TOOLS_DIR) into $(TOOLS_BIN)
	@if [ ! -f '$(TOOLS_DIR)/go.mod' ]; then \
		echo 'No $(TOOLS_DIR)/go.mod — this project pins no generators.'; \
		exit 0; \
	fi; \
	echo 'building generators from $(TOOLS_DIR) into $(TOOLS_BIN)'; \
	GOBIN='$(TOOLS_BIN)' $(GO) -C '$(TOOLS_DIR)' install tool

tidy: ## Tidy and verify go.mod / go.sum, in the root module and in $(TOOLS_DIR)
	$(GO) mod tidy
	$(GO) mod verify
	@if [ -f '$(TOOLS_DIR)/go.mod' ]; then \
		echo 'tidying $(TOOLS_DIR)'; \
		$(GO) -C '$(TOOLS_DIR)' mod tidy; \
		$(GO) -C '$(TOOLS_DIR)' mod verify; \
	fi

cover: test ## Run tests and open an HTML coverage report
	$(GO) tool cover -func=$(COVERAGE_FILE) | tail -n 1
	$(GO) tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@echo "HTML coverage report written to $(COVERAGE_HTML)"

clean: ## Remove build and coverage output
	$(GO) clean -cache -testcache 2>/dev/null || true
	rm -rf $(DIST_DIR) $(COVERAGE_FILE) $(INTEGRATION_COVERAGE_FILE) $(COVERAGE_HTML)
