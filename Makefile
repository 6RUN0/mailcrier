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

# The noshoutrrr tag builds the binary without the shoutrrr library, whose
# targets are then a configuration error; test, build and lint-go cover
# both builds.

# Licenses a dependency linked into the binary may carry; anything else,
# GPL in particular, fails licenses.
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC

# Modules taken under another license than the one go-licenses detects.
# paho.golang (MQTT of shoutrrr) is dual licensed EPL-2.0 or EDL-1.0, and
# EDL-1.0 is BSD-3-Clause; go-licenses reports only EPL-2.0.
# TestLicenseExceptionsStillOffered fails when a new version drops EDL-1.0.
LICENSE_EXCEPTIONS := github.com/eclipse/paho.golang

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
# in the commit that fixes them. Without -parallel each target starts
# GOMAXPROCS fuzzing processes; FUZZPARALLEL=N changes the number.
FUZZTIME ?= 10s
FUZZPARALLEL ?= 4
FUZZ_TARGETS := ./internal/app:FuzzSanitize ./internal/sendmail:FuzzParse ./internal/message:FuzzRead \
	./internal/message:FuzzHTMLToText ./internal/text:FuzzEscapeTelegram ./internal/text:FuzzEscapeChat \
	./internal/text:FuzzCutTelegramHTML \
	./internal/render:FuzzFit ./internal/backend/telegram:FuzzTelegramText ./internal/spool:FuzzDecode \
	./internal/config:FuzzGlob

# setgid-e2e installs the binary setgid inside a throwaway container and
# needs docker; the image is testdata/setgid-e2e/Dockerfile.
E2E_DIR := $(CURDIR)/.e2e
E2E_IMAGE := slendmail-setgid-e2e

# units-verify runs systemd-analyze verify on the queue units with the
# binary and the manual page in place, so that ExecStart and
# Documentation are checked too; the image is testdata/units-verify.
UNITS_IMAGE := slendmail-units-verify
UNITS := slendmail-queue.service slendmail-queue.timer

.PHONY: check lint lint-go lint-yaml lint-actions tidy test fuzz build licenses vuln check-refs check-commits snapshot setgid-e2e units-verify

check: lint tidy test fuzz build licenses check-refs check-commits
	-$(MAKE) vuln

lint: lint-go lint-yaml lint-actions

lint-go:
	$(TOOL) golangci-lint run ./...
	$(TOOL) golangci-lint run --build-tags setgid_e2e,noshoutrrr ./...

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
	$(GO) test -race -tags noshoutrrr ./...

fuzz:
	for target in $(FUZZ_TARGETS); do \
		$(GO) test -run '^$$' -fuzz "^$${target#*:}$$" -fuzztime $(FUZZTIME) -parallel $(FUZZPARALLEL) "$${target%%:*}" || exit 1; \
	done

build:
	$(GO) build -o slendmail ./cmd/slendmail
	$(GO) build -tags noshoutrrr -o /dev/null ./cmd/slendmail

licenses:
	$(GO_LICENSES) check ./cmd/... --allowed_licenses=$(ALLOWED_LICENSES) $(addprefix --ignore=,$(LICENSE_EXCEPTIONS))

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
		/e2e/app.test -test.run '^TestSetgid' -test.v -slendmail-binary /e2e/slendmail

units-verify:
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/slendmail ./cmd/slendmail
	docker build -t $(UNITS_IMAGE) testdata/units-verify
	docker run --rm --network none \
		-v $(E2E_DIR)/slendmail:/usr/sbin/slendmail:ro \
		-v $(CURDIR)/docs/slendmail.8:/usr/share/man/man8/slendmail.8:ro \
		$(foreach unit,$(UNITS),-v $(CURDIR)/packaging/systemd/$(unit):/etc/systemd/system/$(unit):ro) \
		$(UNITS_IMAGE) systemd-analyze verify $(addprefix /etc/systemd/system/,$(UNITS))
