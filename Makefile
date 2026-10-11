GO ?= go
UVX ?= uvx

# Each tool module pins one set of tools; actionlint, goreleaser, gremlins and
# go-licenses have their own because their dependencies conflict with
# golangci-lint's.
TOOL := $(GO) tool -modfile=tools/go.mod
ACTIONLINT := $(GO) tool -modfile=tools/actionlint/go.mod actionlint
GORELEASER := $(GO) tool -modfile=tools/goreleaser/go.mod goreleaser
GO_LICENSES := $(GO) tool -modfile=tools/licenses/go.mod go-licenses
GREMLINS := $(GO) tool -modfile=tools/gremlins/go.mod gremlins
TOOL_MODULES := tools tools/actionlint tools/goreleaser tools/gremlins tools/licenses

# A binary built with -race sleeps atexit_sleep_ms, 1 s by default, before
# it exits: every test binary and every helper process a test starts from
# it. Without the sleep internal/app took 2.6 s instead of 9.8 s.
GORACE ?= atexit_sleep_ms=0

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
RUMDL := $(UVX) rumdl@$(call pinned,rumdl)
MANDOC ?= mandoc

# lint-docs checks the user documentation in one run: links to an anchor of
# another page (MD051) are resolved only when that page is in the same run.
DOCS := README.md README.ru.md docs

# Typographic characters a keyboard does not type, refused by lint-symbols
# in tracked and new files. PCRE needs a UTF-8 locale for \x{} above 0xff.
TYPOGRAPHIC := [\x{2010}-\x{2015}\x{2018}-\x{201F}\x{2026}\x{00AB}\x{00BB}\x{00A0}\x{00AD}\x{200B}\x{2190}-\x{21FF}\x{2713}\x{2717}\x{2022}\x{2500}-\x{257F}\x{00D7}\x{00F7}\x{2264}\x{2265}]

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
SMOKE_TESTS := TestSmokeSetgid TestSmokeRuntime TestSmokeLifecycle TestSmokeUpgrade TestSmokeMTA

# The upgrade tests install the packages of this release first, on /previous;
# smoke-previous fetches them on the host, the containers have no network.
# TestSmokePrevious keeps it the newest release, docs/releasing.md bumps it.
SMOKE_PREVIOUS := v0.1.0-rc5
# sha256 of checksums.txt of that release
SMOKE_PREVIOUS_SUMS := 3d34b0d436ddc5e79d9592a1a91b6007767b2920d771a1f40912777038e464f5
SMOKE_PREVIOUS_DIR := $(E2E_DIR)/previous/$(SMOKE_PREVIOUS)-$(SMOKE_PREVIOUS_SUMS)
SMOKE_PREVIOUS_URL := https://github.com/6RUN0/mailcrier/releases/download/$(SMOKE_PREVIOUS)
SMOKE_PREVIOUS_FILES := $(foreach format,deb rpm apk,mailcrier_$(SMOKE_PREVIOUS:v%=%)_linux_amd64.$(format))

SMOKE_VOLUMES := -v $(CURDIR)/dist:/pkgs:ro -v $(E2E_DIR):/e2e:ro -v $(CURDIR)/testdata/callers:/callers:ro \
	-v $(SMOKE_PREVIOUS_DIR):/previous:ro

# TestSmokeSystemd runs where the packages ship the queue timer, in the
# systemd stage of the image with systemd as PID 1. SYS_ADMIN lets the
# container remount its cgroup namespace writable; apparmor=unconfined,
# because the docker-default profile of Ubuntu denies mount whatever the
# capabilities. SMOKE_SYSTEMD_FLAGS=--privileged is the fallback for a host
# where that is not enough.
SMOKE_SYSTEMD_DISTROS := debian rocky9 rocky10
SMOKE_SYSTEMD_FLAGS ?=

# selinux-<distro> runs testdata/selinux/check.sh on a Rocky cloud image
# under qemu with /dev/kvm, in the container of testdata/selinux, and
# selinux_test.go on its output (docs/selinux.md). Dependabot does not see
# the images: a new minor release of Rocky moves them off
# dl.rockylinux.org, and the URL and the sum from the CHECKSUM file beside
# the image are updated here by hand.
SELINUX_DISTROS := rocky9 rocky10
SELINUX_DIR := $(E2E_DIR)/selinux
SELINUX_IMAGE := mailcrier-selinux
SELINUX_URL_rocky9 := https://dl.rockylinux.org/pub/rocky/9.8/images/x86_64/Rocky-9-GenericCloud-Base-9.8-20260525.0.x86_64.qcow2
SELINUX_SUM_rocky9 := 92c206cc6f790c61583247eefe87890f8828420662c17cacf247cec78ab4eec8
SELINUX_URL_rocky10 := https://dl.rockylinux.org/pub/rocky/10.2/images/x86_64/Rocky-10-GenericCloud-Base-10.2-20260525.0.x86_64.qcow2
SELINUX_SUM_rocky10 := 9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48

# contract runs the TestContract* tests of internal/app against real ntfy
# and Mattermost servers, the images in testdata/contract/<server>, each on
# a random port of 127.0.0.1 so that parallel runs and a busy host do not
# collide. Mattermost posts under the username and into the channel a
# webhook asks for only with the override settings, and the tests create
# their own user and team, which needs open signup.
CONTRACT_SERVERS := ntfy mattermost
CONTRACT_PORT_ntfy := 80
CONTRACT_PORT_mattermost := 8065
CONTRACT_HEALTH_ntfy := /v1/health
CONTRACT_HEALTH_mattermost := /api/v4/system/ping
CONTRACT_FLAGS_mattermost := -e MM_SERVICESETTINGS_ENABLEINCOMINGWEBHOOKS=true \
	-e MM_SERVICESETTINGS_ENABLEPOSTUSERNAMEOVERRIDE=true \
	-e MM_TEAMSETTINGS_ENABLEUSERCREATION=true -e MM_TEAMSETTINGS_ENABLEOPENSERVER=true

# cover runs the tests of both builds with counters in the binary format of
# GOCOVERDIR: go tool covdata merges the two, while concatenated text
# profiles count a block of both builds twice. COVER_MIN is the total of
# 2026-10-11 (91.8%) rounded down; make cover prints the total to raise it.
COVER_DIR := $(CURDIR)/.cover
COVER_MIN := 91

# mutants runs gremlins on MUTANTS_DIR, a directory of the module, in a
# copy of the tracked and new files under .mutants: gremlins copies the
# whole module directory for each worker, .e2e and dist included, and a
# dry run of internal/backend did not end in 20 minutes on that copy.
# timeout-coefficient 10 of .gremlins.yaml: with the default a loaded host
# timed out 86 of 165 mutants of internal/text and hid 19 survivors.
# Survivors are checked against testdata/mutants-allow.txt by
# scripts/check-mutants.
MUTANTS_DIR ?= .
MUTANTS_WORKERS ?= 4
MUTANTS_WORK := $(CURDIR)/.mutants

# bench runs the benchmarks matching BENCH, BENCHCOUNT times each for
# BENCHTIME, and keeps the output in .bench/<commit>.txt: runs of two
# commits compare with benchstat, which wants at least 6 runs of each.
BENCH ?= .
BENCHTIME ?= 1s
BENCHCOUNT ?= 6
BENCH_DIR := $(CURDIR)/.bench

CHANGELOG_START := 12340d0

# Form of a release tag; release.yml runs on any v* tag. grep matches line
# by line, so the case rejects a newline and any other stray character first.
# The suffix is rcN without a dot: apk installs 0.1.0_rc.2 but cannot order
# it. No leading zeros: rc01 and rc1 would give packages of one version.
RELEASE_TAG := v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc[1-9][0-9]*)?
is-release-tag = case "$$TAG" in *[!0-9A-Za-z.-]*|'') false;; esac && echo "$$TAG" | grep -Eqx '$(RELEASE_TAG)'

# release-gate wants each of these jobs of the push run of ci.yml on develop
# successful for the tagged commit; TestReleaseWorkflow keeps the list equal
# to the jobs ci.yml runs on push.
RELEASE_JOBS := make check,make snapshot,make smoke-debian,make smoke-rocky9,make smoke-rocky10,make smoke-alpine,make setgid-e2e,make units-verify

# ci-local runs the jobs of RELEASE_JOBS on this machine: check and snapshot
# first, then the jobs that test the packages of the snapshot, as needs:
# orders them in ci.yml. vuln stays out, as it only warns in check.
comma := ,
CI_JOBS := $(filter-out make,$(subst $(comma), ,$(RELEASE_JOBS)))

.DEFAULT_GOAL := help

.PHONY: help ci-local check cover mutants bench lint lint-go lint-yaml lint-actions lint-docs lint-symbols lint-man tidy test fuzz build licenses vuln check-refs check-commits snapshot release-prepare release-gate release release-guard setgid-e2e units-verify \
	smoke smoke-dist smoke-previous $(addprefix smoke-,$(SMOKE_DISTROS)) contract \
	selinux-images $(addprefix selinux-,$(SELINUX_DISTROS)) $(addprefix selinux-image-,$(SELINUX_DISTROS)) FORCE

# A static pattern rule is listed as its pattern with % read as <distro>;
# a target without "## " is internal and stays out of the list.
help: ## list the targets
	@grep -hE '^[^#[:space:]][^=]*:.*## ' $(MAKEFILE_LIST) | \
		sed -E 's/^\$$\([^)]*\)\)?: *([a-z-]+)%:/\1<distro>:/' | \
		awk 'BEGIN { FS = ":.*## " } { printf "  %-22s %s\n", $$1, $$2 }'

check: lint tidy test cover fuzz build licenses check-refs check-commits ## the CI job make check; vuln only warns here
	-$(MAKE) vuln

lint: lint-go lint-yaml lint-actions lint-docs lint-symbols lint-man ## all linters

lint-docs: ## rumdl over README and docs, links and anchors included
	$(RUMDL) check $(DOCS)

lint-symbols: ## no typographic characters in tracked and new files
	@LC_ALL=C.UTF-8 git grep -nIP --untracked '$(TYPOGRAPHIC)'; status=$$?; \
	if [ $$status = 0 ]; then echo "typographic characters above; type ASCII punctuation instead" >&2; exit 1; fi; \
	[ $$status = 1 ]

lint-man: ## mandoc lint of the manual page
	$(MANDOC) -T lint -W warning docs/mailcrier.8

cover: ## statement coverage of both builds, below COVER_MIN fails; .cover/cover.html
	rm -rf $(COVER_DIR) && mkdir -p $(COVER_DIR)/default $(COVER_DIR)/noshoutrrr
	$(GO) test -cover -covermode=atomic -coverpkg=./... ./... -args -test.gocoverdir=$(COVER_DIR)/default
	$(GO) test -cover -covermode=atomic -coverpkg=./... -tags noshoutrrr ./... -args -test.gocoverdir=$(COVER_DIR)/noshoutrrr
	$(GO) tool covdata textfmt -i=$(COVER_DIR)/default,$(COVER_DIR)/noshoutrrr -o $(COVER_DIR)/cover.out
	$(GO) tool cover -html=$(COVER_DIR)/cover.out -o $(COVER_DIR)/cover.html
	@$(GO) tool cover -func=$(COVER_DIR)/cover.out | awk -v min=$(COVER_MIN) '/^total:/ { sub("%", "", $$NF); \
		print "statements covered: " $$NF "%, floor " min "%"; if ($$NF + 0 < min) exit 1 }'

mutants: ## mutation testing of MUTANTS_DIR, survivors against testdata/mutants-allow.txt
	rm -rf $(MUTANTS_WORK) && mkdir -p $(MUTANTS_WORK)/src
	git ls-files -z -co --exclude-standard | tar --null -T - -cf - | tar -C $(MUTANTS_WORK)/src -xf -
	cd $(MUTANTS_WORK)/src && $(GREMLINS) unleash --config .gremlins.yaml --workers $(MUTANTS_WORKERS) \
		-o $(MUTANTS_WORK)/report.json ./$(MUTANTS_DIR)
	$(GO) run ./scripts/check-mutants -allow testdata/mutants-allow.txt $(MUTANTS_DIR)=$(MUTANTS_WORK)/report.json

bench: ## benchmarks matching BENCH, output in .bench/<commit>.txt
	mkdir -p $(BENCH_DIR)
	out=$(BENCH_DIR)/$$(git rev-parse --short HEAD).txt; \
	$(GO) test -run '^$$' -bench '$(BENCH)' -benchmem -benchtime $(BENCHTIME) -count $(BENCHCOUNT) ./... >"$$out"; \
	status=$$?; cat "$$out"; exit $$status

ci-local: ## the jobs of ci.yml here, needs docker; make -j4 -O ci-local
	$(MAKE) $(filter check snapshot,$(CI_JOBS))
	$(MAKE) $(filter-out check snapshot,$(CI_JOBS))

lint-go: ## golangci-lint, also under the build tags of the e2e tests
	$(TOOL) golangci-lint run ./...
	$(TOOL) golangci-lint run --build-tags setgid_e2e,noshoutrrr,smoke,selinux ./...

lint-yaml: ## yamllint --strict
	$(YAMLLINT) --strict .

lint-actions: ## actionlint and zizmor over the workflows
	$(ACTIONLINT)
	$(ZIZMOR) $(ZIZMOR_FLAGS) .

tidy: ## go mod tidy -diff of the module and the tool modules
	$(GO) mod tidy -diff
	for dir in $(TOOL_MODULES); do (cd $$dir && $(GO) mod tidy -diff) || exit 1; done

test: ## go test -race, with and without shoutrrr
	GORACE=$(GORACE) $(GO) test -race ./...
	GORACE=$(GORACE) $(GO) test -race -tags noshoutrrr ./...

fuzz: ## each fuzz target FUZZTIME long, FUZZPARALLEL processes
	for target in $(FUZZ_TARGETS); do \
		$(GO) test -run '^$$' -fuzz "^$${target#*:}$$" -fuzztime $(FUZZTIME) -parallel $(FUZZPARALLEL) "$${target%%:*}" || exit 1; \
	done

build: ## the binary ./mailcrier, and the build without shoutrrr
	$(GO) build -o mailcrier ./cmd/mailcrier
	$(GO) build -tags noshoutrrr -o /dev/null ./cmd/mailcrier

licenses: ## licenses of the linked modules
	$(GO_LICENSES) check ./cmd/... --allowed_licenses=$(ALLOWED_LICENSES) $(addprefix --ignore=,$(LICENSE_EXCEPTIONS))

vuln: ## govulncheck
	$(TOOL) govulncheck ./...

check-refs: ## no plan or review references since BASE
	$(GO) run ./scripts/check-refs -base $(BASE)

check-commits: ## commit messages of BASE..HEAD
	$(GO) run ./scripts/check-commits -base $(BASE)

snapshot: release-prepare ## packages of HEAD in dist/
	$(GORELEASER) release --snapshot --clean

# nFPM writes the mtime of a script file into the apk, untouched by mtime and
# SOURCE_DATE_EPOCH; the commit time keeps packages of any checkout equal.
release-prepare:
	find packaging/scripts -type f -exec touch -d @$$(git log -1 --format=%ct) {} +

# The tag must name a commit that is in main and develop, with main not ahead
# of develop (so a commit in main is in develop too), and whose push run of ci.yml on develop passed. "In main", not
# "the head of main": a rerun after the next fast-forward still passes.
release-gate: ## gate of release.yml for TAG
	@$(is-release-tag) || { echo "release-gate needs TAG=vX.Y.Z[-rcN]" >&2; exit 2; }
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
release: release-guard release-prepare ## publish TAG; release.yml only
	prev=$$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' "refs/tags/$$TAG^" 2>/dev/null) || \
		prev=$(CHANGELOG_START)^; \
	GORELEASER_CURRENT_TAG="$$TAG" GORELEASER_PREVIOUS_TAG="$$prev" $(GORELEASER) release --clean

release-guard:
	@[ "$$GITHUB_ACTIONS" = true ] || { echo "release publishes to GitHub; run it from release.yml" >&2; exit 2; }
	@$(is-release-tag) || { echo "release needs TAG=vX.Y.Z[-rcN]" >&2; exit 2; }

setgid-e2e: ## TestSetgid* in a root container (docker)
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/mailcrier ./cmd/mailcrier
	CGO_ENABLED=0 $(GO) test -c -tags setgid_e2e -o $(E2E_DIR)/app.test ./internal/app
	docker build -t $(E2E_IMAGE) testdata/setgid-e2e
	docker run --rm --network none --cap-add SYS_PTRACE -v $(E2E_DIR):/e2e:ro $(E2E_IMAGE) \
		/e2e/app.test -test.run '^TestSetgid' -test.v -mailcrier-binary /e2e/mailcrier

units-verify: ## systemd-analyze verify of the queue units (docker)
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) build -o $(E2E_DIR)/mailcrier ./cmd/mailcrier
	docker build -t $(UNITS_IMAGE) testdata/units-verify
	docker run --rm --network none \
		-v $(E2E_DIR)/mailcrier:/usr/sbin/mailcrier:ro \
		-v $(CURDIR)/docs/mailcrier.8:/usr/share/man/man8/mailcrier.8:ro \
		$(foreach unit,$(UNITS),-v $(CURDIR)/packaging/systemd/$(unit):/etc/systemd/system/$(unit):ro) \
		$(UNITS_IMAGE) systemd-analyze verify $(addprefix /etc/systemd/system/,$(UNITS))

smoke: $(addprefix smoke-,$(SMOKE_DISTROS)) ## smoke tests of dist/ on every distribution

# Each test function runs even when an earlier one failed: they share no
# state, and every failure is worth seeing.
$(addprefix smoke-,$(SMOKE_DISTROS)): smoke-%: smoke-dist smoke-previous $(E2E_DIR)/smoke.test $(E2E_DIR)/app.test ## smoke tests of dist/ on one distribution of SMOKE_DISTROS
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

contract: ## contract tests of ntfy, shoutrrr and Mattermost against real servers (docker)
	$(foreach server,$(CONTRACT_SERVERS),docker build -t mailcrier-contract-$(server) testdata/contract/$(server) &&) true
	mkdir -p $(E2E_DIR)
	status=0; \
	$(contract-remove) \
	{ $(foreach server,$(CONTRACT_SERVERS),$(call contract-start,$(server)) &&) \
		$(foreach server,$(CONTRACT_SERVERS),$(call contract-wait,$(server)) &&) \
		MAILCRIER_CONTRACT_NTFY_URL=http://$$addr_ntfy MAILCRIER_CONTRACT_MATTERMOST_URL=http://$$addr_mattermost \
		$(GO) test -count=1 -v -run '^TestContract' ./internal/app; } || status=1; \
	if [ $$status != 0 ]; then \
		for cid in $(E2E_DIR)/contract-*.cid; do \
			[ -f $$cid ] && docker logs $$(cat $$cid) 2>&1 | tail -50; \
		done; \
	fi; \
	$(contract-remove) \
	exit $$status

# Containers are removed by the ids in their cidfiles, before a run as well:
# an interrupted run leaves them behind.
define contract-remove
for cid in $(E2E_DIR)/contract-*.cid; do \
	[ -f $$cid ] || continue; docker rm -f $$(cat $$cid) >/dev/null 2>&1; rm -f $$cid; \
done;
endef

# All servers start before the first wait, so that their start-up overlaps.
define contract-start
docker run -d --cidfile $(E2E_DIR)/contract-$(1).cid -p 127.0.0.1::$(CONTRACT_PORT_$(1)) $(CONTRACT_FLAGS_$(1)) \
		mailcrier-contract-$(1) >/dev/null && \
	addr_$(1)=$$(docker port $$(cat $(E2E_DIR)/contract-$(1).cid) $(CONTRACT_PORT_$(1))/tcp | head -n 1)
endef

# Docker accepts on the published port before the server listens and then
# resets the connection, so curl retries every error, not only a refusal.
# Mattermost takes about a minute to answer the first time.
define contract-wait
{ curl -fs -o /dev/null --retry 300 --retry-all-errors --retry-delay 1 --retry-max-time 300 \
		http://$$addr_$(1)$(CONTRACT_HEALTH_$(1)) || { echo "$(1) did not answer $(CONTRACT_HEALTH_$(1))" >&2; false; }; }
endef

smoke-dist:
	@for format in deb rpm apk; do \
		set -- dist/mailcrier_*_linux_amd64.$$format; \
		[ -f "$$1" ] || { echo "dist/ has no amd64 $$format package, run make snapshot" >&2; exit 1; }; \
	done
	@commit=$$(sed -n 's/.*"commit":"\([0-9a-f]*\)".*/\1/p' dist/metadata.json 2>/dev/null); \
	[ "$$commit" = "$$(git rev-parse HEAD)" ] || { echo "dist/ is from $${commit:-an unknown commit}, run make snapshot" >&2; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || echo "warning: uncommitted changes are not in dist/" >&2

# A file is fetched only when missing and moved in place only once its sum
# matches, so an interrupted download leaves nothing in place and a rerun
# needs no network; every run checks all of them again.
smoke-previous: ## fetch the packages of SMOKE_PREVIOUS
	mkdir -p $(SMOKE_PREVIOUS_DIR)/partial
	cd $(SMOKE_PREVIOUS_DIR) && for file in checksums.txt $(SMOKE_PREVIOUS_FILES); do \
		[ -f "$$file" ] && continue; \
		curl -fsSL --proto '=https' --retry 3 -o "partial/$$file" "$(SMOKE_PREVIOUS_URL)/$$file" || exit 1; \
		if [ "$$file" = checksums.txt ]; then \
			echo '$(SMOKE_PREVIOUS_SUMS)  partial/checksums.txt' | sha256sum -c --quiet - || exit 1; \
		else \
			awk -v file="$$file" '$$2 == file { print $$1 "  partial/" file }' checksums.txt | sha256sum -c --quiet - || exit 1; \
		fi; \
		mv "partial/$$file" "$$file"; \
	done
	cd $(SMOKE_PREVIOUS_DIR) && echo '$(SMOKE_PREVIOUS_SUMS)  checksums.txt' | sha256sum -c --quiet - && \
		for file in $(SMOKE_PREVIOUS_FILES); do \
			awk -v file="$$file" '$$2 == file' checksums.txt | sha256sum -c --quiet - || exit 1; \
		done

# The run leaves its output in $(SELINUX_DIR)/<distro>, and the test reads
# it also after a failed run, to name the step. The container runs as root
# for /dev/kvm and ssh; run.sh hands its files to OWNER.
$(addprefix selinux-,$(SELINUX_DISTROS)): selinux-%: smoke-dist smoke-previous selinux-image-% ## SELinux check of dist/ in a VM (docker, /dev/kvm)
	docker build -t $(SELINUX_IMAGE) testdata/selinux
	mkdir -p $(SELINUX_DIR)/$*
	status=0; \
	docker run --rm --device /dev/kvm -e OWNER=$$(id -u):$$(id -g) \
		-v $(SELINUX_DIR)/$(notdir $(SELINUX_URL_$*)):/images/base.qcow2:ro \
		-v $(CURDIR)/dist:/pkgs:ro -v $(SMOKE_PREVIOUS_DIR):/previous:ro \
		-v $(CURDIR)/testdata/selinux:/stand:ro -v $(SELINUX_DIR)/$*:/work \
		$(SELINUX_IMAGE) sh /stand/run.sh || status=1; \
	$(GO) test -tags selinux -count=1 -run '^TestSELinux$$' . -args -selinux-log $(SELINUX_DIR)/$*/check.log || status=1; \
	exit $$status

selinux-images: $(addprefix selinux-image-,$(SELINUX_DISTROS)) ## fetch the Rocky cloud images

# Fetched like smoke-previous: moved in place only once the sum matches,
# checked again on every run.
$(addprefix selinux-image-,$(SELINUX_DISTROS)): selinux-image-%:
	mkdir -p $(SELINUX_DIR)/partial
	cd $(SELINUX_DIR) && file=$(notdir $(SELINUX_URL_$*)) && \
		if [ ! -f "$$file" ]; then \
			curl -fsSL --proto '=https' --retry 3 -o "partial/$$file" '$(SELINUX_URL_$*)' && \
			echo "$(SELINUX_SUM_$*)  partial/$$file" | sha256sum -c --quiet - && \
			mv "partial/$$file" "$$file"; \
		fi && \
		echo "$(SELINUX_SUM_$*)  $$file" | sha256sum -c --quiet -

# FORCE rebuilds the test binaries on every run: go's cache keeps that
# cheap, and a stale binary would test old code.
$(E2E_DIR)/smoke.test: FORCE
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) test -c -tags smoke -o $@ ./packaging/smoke

$(E2E_DIR)/app.test: FORCE
	mkdir -p $(E2E_DIR)
	CGO_ENABLED=0 $(GO) test -c -tags setgid_e2e -o $@ ./internal/app

FORCE:
