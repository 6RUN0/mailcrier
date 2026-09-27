GO ?= go
UVX ?= uvx

# Each tool module pins one set of tools; actionlint and goreleaser have
# their own because their dependencies conflict with golangci-lint's.
TOOL := $(GO) tool -modfile=tools/go.mod
ACTIONLINT := $(GO) tool -modfile=tools/actionlint/go.mod actionlint
GORELEASER := $(GO) tool -modfile=tools/goreleaser/go.mod goreleaser
TOOL_MODULES := tools tools/actionlint tools/goreleaser

# Python tools are pinned in tools/requirements.txt, which Dependabot updates.
pinned = $(shell sed -n 's/^$(1)==//p' tools/requirements.txt)
YAMLLINT := $(UVX) yamllint@$(call pinned,yamllint)
ZIZMOR := $(UVX) zizmor@$(call pinned,zizmor)

# Online audits need GH_TOKEN; CI clears the flag and provides the token.
ZIZMOR_FLAGS ?= --offline

# Commits in BASE..HEAD and files changed since BASE are checked by
# check-refs and check-commits.
BASE ?= $(shell git rev-parse -q --verify origin/develop >/dev/null && echo origin/develop || echo HEAD)

# The legacy root package is not shipped and is only a feature reference,
# so its dependencies are not scanned.
VULN_PACKAGES := ./cmd/... ./internal/... ./scripts/...

.PHONY: check lint lint-go lint-yaml lint-actions tidy test build vuln check-refs check-commits snapshot

check: lint tidy test build check-refs check-commits
	-$(MAKE) vuln

lint: lint-go lint-yaml lint-actions

lint-go:
	$(TOOL) golangci-lint run ./...

lint-yaml:
	$(YAMLLINT) --strict .

lint-actions:
	$(ACTIONLINT)
	$(ZIZMOR) $(ZIZMOR_FLAGS) .

tidy:
	$(GO) mod tidy -diff
	for dir in $(TOOL_MODULES); do (cd $$dir && $(GO) mod tidy -diff) || exit 1; done

test:
	$(GO) test -race ./...

build:
	$(GO) build -o slendmail ./cmd/slendmail
	$(GO) build -o /dev/null .

vuln:
	$(TOOL) govulncheck $(VULN_PACKAGES)

check-refs:
	$(GO) run ./scripts/check-refs -base $(BASE)

check-commits:
	$(GO) run ./scripts/check-commits -base $(BASE)

snapshot:
	$(GORELEASER) release --snapshot --clean
