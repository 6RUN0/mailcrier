GO ?= go
UVX ?= uvx

# Each tool module pins one set of tools; actionlint, goreleaser and
# go-licenses have their own because their dependencies conflict with
# golangci-lint's.
TOOL := $(GO) tool -modfile=tools/go.mod
ACTIONLINT := $(GO) tool -modfile=tools/actionlint/go.mod actionlint
GORELEASER := $(GO) tool -modfile=tools/goreleaser/go.mod goreleaser
GO_LICENSES := $(GO) tool -modfile=tools/licenses/go.mod go-licenses
TOOL_MODULES := tools tools/actionlint tools/goreleaser tools/licenses

# Licenses a dependency linked into the binary may carry; anything else,
# GPL in particular, fails licenses.
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC

# Python tools are pinned in tools/requirements.txt, which Dependabot updates.
pinned = $(shell sed -n 's/^$(1)==//p' tools/requirements.txt)
YAMLLINT := $(UVX) yamllint@$(call pinned,yamllint)
ZIZMOR := $(UVX) zizmor@$(call pinned,zizmor)

# Online audits need GH_TOKEN; CI clears the flag and provides the token.
ZIZMOR_FLAGS ?= --offline

# Commits in BASE..HEAD and files changed since BASE are checked by
# check-refs and check-commits.
BASE ?= $(shell git rev-parse -q --verify origin/develop >/dev/null && echo origin/develop || echo HEAD)

# Each fuzz target runs this long in check; a longer run takes
# FUZZTIME=10m. Inputs that fail are saved under testdata/fuzz and belong
# in the commit that fixes them.
FUZZTIME ?= 10s
FUZZ_TARGETS := ./internal/app:FuzzSanitize ./internal/sendmail:FuzzParse ./internal/message:FuzzRead \
	./internal/message:FuzzHTMLToText ./internal/text:FuzzEscapeTelegram ./internal/text:FuzzEscapeChat \
	./internal/render:FuzzFit

# setgid-e2e installs the binary setgid inside a throwaway container and
# needs docker; the image is testdata/setgid-e2e/Dockerfile.
E2E_DIR := $(CURDIR)/.e2e
E2E_IMAGE := slendmail-setgid-e2e

.PHONY: check lint lint-go lint-yaml lint-actions tidy test fuzz build licenses vuln check-refs check-commits snapshot setgid-e2e

check: lint tidy test fuzz build licenses check-refs check-commits
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

fuzz:
	for target in $(FUZZ_TARGETS); do \
		$(GO) test -run '^$$' -fuzz "^$${target#*:}$$" -fuzztime $(FUZZTIME) "$${target%%:*}" || exit 1; \
	done

build:
	$(GO) build -o slendmail ./cmd/slendmail

licenses:
	$(GO_LICENSES) check ./cmd/... --allowed_licenses=$(ALLOWED_LICENSES)

vuln:
	$(TOOL) govulncheck ./...

check-refs:
	$(GO) run ./scripts/check-refs -base $(BASE)

check-commits:
	$(GO) run ./scripts/check-commits -base $(BASE)

snapshot:
	$(GORELEASER) release --snapshot --clean

setgid-e2e:
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/slendmail ./cmd/slendmail
	CGO_ENABLED=0 $(GO) test -c -tags setgid_e2e -o $(E2E_DIR)/app.test ./internal/app
	docker build -t $(E2E_IMAGE) testdata/setgid-e2e
	docker run --rm --network none --cap-add SYS_PTRACE -v $(E2E_DIR):/e2e:ro $(E2E_IMAGE) \
		/e2e/app.test -test.run '^TestSetgidReexec$$' -test.v -slendmail-binary /e2e/slendmail
