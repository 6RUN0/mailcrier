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
E2E_IMAGE := mailcrier-setgid-e2e

# units-verify runs systemd-analyze verify on the queue units with the
# binary and the manual page in place, so that ExecStart and
# Documentation are checked too; the image is testdata/units-verify.
UNITS_IMAGE := mailcrier-units-verify
UNITS := mailcrier-queue.service mailcrier-queue.timer

# smoke installs the amd64 packages of dist/ in a container per
# distribution, the images in packaging/smoke/<distro>, and runs each test
# function of packaging/smoke in a fresh one; dist/ must come from HEAD.
SMOKE_DISTROS := debian rocky9 rocky10 alpine
SMOKE_TESTS := TestSmokeSetgid TestSmokeRuntime TestSmokeLifecycle
SMOKE_VOLUMES := -v $(CURDIR)/dist:/pkgs:ro -v $(E2E_DIR):/e2e:ro -v $(CURDIR)/testdata/callers:/callers:ro

# TestSmokeSystemd runs where the packages ship the queue timer, in the
# systemd stage of the image with systemd as PID 1. SYS_ADMIN lets the
# container remount its cgroup namespace writable; apparmor=unconfined,
# because the docker-default profile of Ubuntu denies mount whatever the
# capabilities. SMOKE_SYSTEMD_FLAGS=--privileged is the fallback for a host
# where that is not enough.
SMOKE_SYSTEMD_DISTROS := debian rocky9 rocky10
SMOKE_SYSTEMD_FLAGS ?=

CHANGELOG_START := 12340d0

# Form of a release tag; release.yml runs on any v* tag. grep matches line
# by line, so the case rejects a newline and any other stray character first.
RELEASE_TAG := v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?
is-release-tag = case "$$TAG" in *[!0-9A-Za-z.-]*|'') false;; esac && echo "$$TAG" | grep -Eqx '$(RELEASE_TAG)'

# release-gate wants each of these jobs of the push run of ci.yml on develop
# successful for the tagged commit; TestReleaseWorkflow keeps the list equal
# to the jobs ci.yml runs on push.
RELEASE_JOBS := make check,make snapshot,make smoke-debian,make smoke-rocky9,make smoke-rocky10,make smoke-alpine,make setgid-e2e,make units-verify

.PHONY: check lint lint-go lint-yaml lint-actions tidy test fuzz build licenses vuln check-refs check-commits snapshot release-prepare release-gate release release-guard setgid-e2e units-verify \
	smoke smoke-dist $(addprefix smoke-,$(SMOKE_DISTROS)) FORCE

check: lint tidy test fuzz build licenses check-refs check-commits
	-$(MAKE) vuln

lint: lint-go lint-yaml lint-actions

lint-go:
	$(TOOL) golangci-lint run ./...
	$(TOOL) golangci-lint run --build-tags setgid_e2e,noshoutrrr,smoke ./...

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
	$(GO) build -o mailcrier ./cmd/mailcrier
	$(GO) build -tags noshoutrrr -o /dev/null ./cmd/mailcrier

licenses:
	$(GO_LICENSES) check ./cmd/... --allowed_licenses=$(ALLOWED_LICENSES) $(addprefix --ignore=,$(LICENSE_EXCEPTIONS))

vuln:
	$(TOOL) govulncheck ./...

check-refs:
	$(GO) run ./scripts/check-refs -base $(BASE)

check-commits:
	$(GO) run ./scripts/check-commits -base $(BASE)

snapshot: release-prepare
	$(GORELEASER) release --snapshot --clean

# nFPM writes the mtime of a script file into the apk, untouched by mtime and
# SOURCE_DATE_EPOCH; the commit time keeps packages of any checkout equal.
release-prepare:
	find packaging/scripts -type f -exec touch -d @$$(git log -1 --format=%ct) {} +

# The tag must name a commit that is in main and develop, with main not ahead
# of develop (so a commit in main is in develop too), and whose push run of ci.yml on develop passed. "In main", not
# "the head of main": a rerun after the next fast-forward still passes.
release-gate:
	@$(is-release-tag) || { echo "release-gate needs TAG=vX.Y.Z[-pre]" >&2; exit 2; }
	@[ "$$(git rev-parse -q --verify "refs/tags/$$TAG^{commit}")" = "$$(git rev-parse HEAD)" ] || \
		{ echo "tag $$TAG does not point at HEAD" >&2; exit 1; }
	@git rev-parse -q --verify origin/main >/dev/null && git rev-parse -q --verify origin/develop >/dev/null || \
		{ echo "origin/main or origin/develop is missing; fetch the full history" >&2; exit 1; }
	@git merge-base --is-ancestor origin/main origin/develop || \
		{ echo "main has commits that develop lacks" >&2; exit 1; }
	@git merge-base --is-ancestor HEAD origin/main || \
		{ echo "HEAD is not in main; fast-forward main first" >&2; exit 1; }
	$(GO) run ./scripts/check-release -sha $$(git rev-parse HEAD) -workflow .github/workflows/ci.yml \
		-branch develop -jobs '$(RELEASE_JOBS)'

# TAG reaches the recipes as an environment variable, never spliced into the
# shell text: a git tag name may hold a dollar sign or a semicolon.
# GORELEASER_CURRENT_TAG picks it when an rc and the final tag name the same
# commit. Publishing needs the token and the tag of release.yml; a local run would
# publish whatever the work tree holds. A refused guard stops make before
# goreleaser, also under -j, where only the harmless touch may run.
# The notes run from the last final tag before the tagged commit, so an rc
# and the final release on its commit get the same notes; without one they
# start at CHANGELOG_START.
release: release-guard release-prepare
	prev=$$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' "refs/tags/$$TAG^" 2>/dev/null) || \
		prev=$(CHANGELOG_START)^; \
	GORELEASER_CURRENT_TAG="$$TAG" GORELEASER_PREVIOUS_TAG="$$prev" $(GORELEASER) release --clean

release-guard:
	@[ "$$GITHUB_ACTIONS" = true ] || { echo "release publishes to GitHub; run it from release.yml" >&2; exit 2; }
	@$(is-release-tag) || { echo "release needs TAG=vX.Y.Z[-pre]" >&2; exit 2; }

setgid-e2e:
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/mailcrier ./cmd/mailcrier
	CGO_ENABLED=0 $(GO) test -c -tags setgid_e2e -o $(E2E_DIR)/app.test ./internal/app
	docker build -t $(E2E_IMAGE) testdata/setgid-e2e
	docker run --rm --network none --cap-add SYS_PTRACE -v $(E2E_DIR):/e2e:ro $(E2E_IMAGE) \
		/e2e/app.test -test.run '^TestSetgid' -test.v -mailcrier-binary /e2e/mailcrier

units-verify:
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/mailcrier ./cmd/mailcrier
	docker build -t $(UNITS_IMAGE) testdata/units-verify
	docker run --rm --network none \
		-v $(E2E_DIR)/mailcrier:/usr/sbin/mailcrier:ro \
		-v $(CURDIR)/docs/mailcrier.8:/usr/share/man/man8/mailcrier.8:ro \
		$(foreach unit,$(UNITS),-v $(CURDIR)/packaging/systemd/$(unit):/etc/systemd/system/$(unit):ro) \
		$(UNITS_IMAGE) systemd-analyze verify $(addprefix /etc/systemd/system/,$(UNITS))

smoke: $(addprefix smoke-,$(SMOKE_DISTROS))

# Each test function runs even when an earlier one failed: they share no
# state, and every failure is worth seeing.
$(addprefix smoke-,$(SMOKE_DISTROS)): smoke-%: smoke-dist $(E2E_DIR)/smoke.test $(E2E_DIR)/app.test
	docker build --target plain -t mailcrier-smoke-$* packaging/smoke/$*
	$(if $(filter $*,$(SMOKE_SYSTEMD_DISTROS)),docker build --target systemd -t mailcrier-smoke-$*-systemd packaging/smoke/$*)
	status=0; for test in $(SMOKE_TESTS); do \
		docker run --rm --network none --cap-add SYS_PTRACE $(SMOKE_VOLUMES) mailcrier-smoke-$* \
			/e2e/smoke.test -test.run "^$$test\$$" -test.v -test.timeout 20m || status=1; \
	done; \
	$(if $(filter $*,$(SMOKE_SYSTEMD_DISTROS)),$(call smoke-systemd,$*) || status=1;) \
	exit $$status

# The container is removed by the id in its cidfile, also when systemd or
# the test fails, and a container an interrupted run left behind before the
# next one starts; on a failure its state and output come first.
define smoke-systemd
cid=$(E2E_DIR)/smoke-$(1).cid; \
	if [ -f $$cid ]; then docker rm -f $$(cat $$cid) >/dev/null 2>&1; rm -f $$cid; fi; \
	docker run -d --cidfile $$cid --network none --cgroupns=private --tmpfs /run --tmpfs /run/lock \
		--cap-add SYS_ADMIN --security-opt apparmor=unconfined $(SMOKE_SYSTEMD_FLAGS) $(SMOKE_VOLUMES) \
		mailcrier-smoke-$(1)-systemd >/dev/null && \
	docker exec $$(cat $$cid) /e2e/smoke.test -test.run '^TestSmokeSystemd$$' -test.v -test.timeout 20m; \
	s=$$?; \
	if [ -f $$cid ]; then \
		if [ $$s != 0 ]; then \
			docker inspect -f 'container {{.State.Status}}, exit code {{.State.ExitCode}}' $$(cat $$cid); \
			docker logs $$(cat $$cid) 2>&1 | tail -50; \
		fi; \
		docker rm -f $$(cat $$cid) >/dev/null; \
	fi; \
	rm -f $$cid; [ $$s = 0 ]
endef

smoke-dist:
	@for format in deb rpm apk; do \
		set -- dist/mailcrier_*_linux_amd64.$$format; \
		[ -f "$$1" ] || { echo "dist/ has no amd64 $$format package, run make snapshot" >&2; exit 1; }; \
	done
	@commit=$$(sed -n 's/.*"commit":"\([0-9a-f]*\)".*/\1/p' dist/metadata.json 2>/dev/null); \
	[ "$$commit" = "$$(git rev-parse HEAD)" ] || { echo "dist/ is from $${commit:-an unknown commit}, run make snapshot" >&2; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || echo "warning: uncommitted changes are not in dist/" >&2

# FORCE rebuilds the test binaries on every run: go's cache keeps that
# cheap, and a stale binary would test old code.
$(E2E_DIR)/smoke.test: FORCE
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) test -c -tags smoke -o $@ ./packaging/smoke

$(E2E_DIR)/app.test: FORCE
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) test -c -tags setgid_e2e -o $@ ./internal/app

FORCE:
