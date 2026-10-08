# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## Commands

```sh
make check        # the CI job make check; govulncheck only warns here
make lint         # lint-go lint-yaml lint-actions; other targets: tidy test
                  # fuzz build licenses vuln check-refs check-commits snapshot
make fuzz FUZZTIME=10m   # longer fuzzing; check runs each target 10s
make fuzz FUZZPARALLEL=8  # fuzzing processes per target, default 4
make setgid-e2e   # needs docker: TestSetgid* in a root container
make units-verify # needs docker: systemd-analyze verify of packaging/systemd
make smoke        # needs docker and dist/ of HEAD (make snapshot): packages
                  # the first run fetches SMOKE_PREVIOUS over the network
make smoke-alpine # one of debian rocky9 rocky10 alpine
make -j4 -O smoke # all distributions in parallel, 5-7 min with built images
go build -o mailcrier ./cmd/mailcrier   # the binary; the name is gitignored
MAILCRIER_TELEGRAM_ENV=/path/to/telegram.env \
  go test -run TestLiveTelegram ./internal/backend/telegram   # real Bot API
go test ./internal/backend/webhook -update   # rewrite golden files
go test ./internal/app -run TestCallers -update   # caller golden files
go test ./internal/render -run TestBuiltinGolden -update   # template golden
mandoc -T lint -W warning docs/mailcrier.8   # man page; not in make check
make release-gate TAG=v0.1.0   # gate of release.yml; check-release needs
                               # GH_TOKEN and GITHUB_REPOSITORY
make release TAG=v0.1.0        # publishes; refuses without GITHUB_ACTIONS=true
```

The release procedure is `docs/releasing.md`.

- Tools are pinned where Dependabot updates them: golangci-lint and
  govulncheck in `tools/go.mod`, actionlint in `tools/actionlint/go.mod`,
  goreleaser in `tools/goreleaser/go.mod`, go-licenses in
  `tools/licenses/go.mod` (run as `go tool -modfile=...`; separate modules
  because their dependencies conflict with golangci-lint's),
  yamllint and zizmor in `tools/requirements.txt` (run through `uvx`, the
  `Makefile` reads the versions from that file).
- `go get -tool` records tool modules as `// indirect`, which Dependabot
  skips; each one is named in the `allow` list of the tools entry in
  `.github/dependabot.yml`, and `TestDependabotCoversTools` fails when a tool
  module is missing there.
- `check-refs` and `check-commits` take `BASE` (default `origin/develop`,
  `HEAD` when that ref is missing):
  commits in `BASE..HEAD`, files changed since `BASE`, untracked included.
- zizmor runs `--offline` locally; CI sets `ZIZMOR_FLAGS=` and `GH_TOKEN`
  for the online audits (impostor commits, known vulnerable actions).
- `check-commits` does not limit the subject length of Dependabot commits,
  and `check-refs` checks only their subject: the subjects embed module
  paths, the bodies quote upstream release notes. Dependabot is recognized
  by its noreply author email, not by the author name.
- `make smoke` refuses a `dist/` whose `metadata.json` names another commit
  than HEAD. Its first run fetches the amd64 packages of the release
  `SMOKE_PREVIOUS` with the network of the host into `.e2e/previous`
  (`make smoke-previous`, checked against `SMOKE_PREVIOUS_SUMS`), which
  `TestSmokeUpgrade` installs before those of `dist/`. The Rocky 10 image
  needs an x86-64-v3 host. Each test function of `packaging/smoke` (tag
  `smoke`, built static) runs as root in its own container without network;
  it starts cron and atd itself, waits on receiver requests and FIFOs, never
  on time, and `TestSmokeRuntime` lasts up to 7 minutes because only the
  `*/5` cron job of the package delivers its queued message. Distributions
  are keyed by `ID` and the major `VERSION_ID` of `/etc/os-release`. The
  `plain` stage of the Debian image keeps the apt lists and holds the
  postfix packages in `/opt/postfix` for `TestSmokeMTA`; its `systemd`
  stage drops the lists. `TestSmokeSystemd` runs through `docker exec` in
  the `systemd` stage (Debian, Rocky) with systemd as PID 1, which needs
  `--cap-add SYS_ADMIN` and `--security-opt apparmor=unconfined` to remount
  its cgroup tree writable (the docker-default AppArmor profile of Ubuntu
  denies mount whatever the capabilities); `SMOKE_SYSTEMD_FLAGS=--privileged`
  is the fallback for a host where that is not enough.
- A manual run of the binary reads `/etc/mailcrier.conf`, or the file
  in `MAILCRIER_CONFIG` or `--config` (honoured when not setgid-elevated).

CI: `.github/workflows/ci.yml` runs `make check`, `make snapshot`,
`make setgid-e2e`, `make units-verify` and, on the amd64 packages of the
snapshot job, `make smoke-<distro>` per distribution on push to `develop`
and `main`, on PR to `develop`, and a blocking `make vuln` daily; CodeQL
runs on the same push and PR events (`TestCIBranches`) and weekly.
`.github/workflows/release.yml` runs on a pushed `v*` tag only: `make
release-gate` (the tag names HEAD, HEAD is in `main`, `main` has no commit
`develop` lacks, and `scripts/check-release` finds the newest
push run of `ci.yml` on `develop` for that commit completed with every job
of `RELEASE_JOBS` successful), `make vuln`, then `make release` (goreleaser
publishes the release at once, a tag `-rcN` (`RELEASE_TAG` allows no
other suffix) as a prerelease, notes from the built-in changelog since the last
final tag before the tagged commit, without Dependabot commits) and
`actions/attest-build-provenance` over `dist/checksums.txt`.
`develop` is the development branch, `main` the default branch that only
fast-forwards to a commit of `develop`: before the first release also to
one that changes the CI infrastructure, after it only to a commit being
released.
Every entry of `.github/dependabot.yml` sets `target-branch:
develop` (`TestDependabotTargetsDevelop`): without it version updates
open against `main`.
Actions are pinned by commit SHA with the version in a comment, the e2e
images (`testdata/*/Dockerfile`, `packaging/smoke/*/Dockerfile`) by digest,
each directory named in the docker entry of `.github/dependabot.yml`
(`TestDependabotCoversDockerfiles`). Dependabot waits 7 days before
proposing a new version.

## Architecture

The program lives in `cmd/mailcrier` (entry point) and `internal/`; the
root package holds only repository-wide tests, so `go build .` there fails
with "build constraints exclude all Go files".

- `internal/app.Run(ctx, Deps, args, stdin) int` handles one invocation;
  `Deps` carries the logger factory, config `fs.FS` rooted at `/`, HTTP
  client, host name, argv[0] (`Program`), stdout, stderr, `Credentials`
  (uid, gid, egid, service uid), environment, the error of a failed
  re-exec, a uid-to-login lookup and the default spool directory
  (`SpoolDir`, empty in tests unless set); the unexported `deliver` field
  lets tests replace delivery, `spoolSaved` and `entryLocked` let a
  helper process kill or stop itself at a spool state.
  Everything time-dependent in the spool (ids, backoff, TTL, budgets, age
  of `tmp/` files) reads `Deps.Now`; only the context deadlines use the
  real clock. `app.SystemDeps`
  builds the real ones (syslog `LOG_MAIL`, `/etc/mailcrier.conf`,
  environment proxies only without elevation).
- Elevated means `egid != gid` and `uid != 0` (`internal/app/privilege.go`).
  `main` sets the umask, then calls `app.Harden` before `SystemDeps`, any
  logger or any time formatting (the time package opens a `TZ` path): an
  elevated process re-executes itself with `sanitizeEnv(environ)` unless
  the environment is already sanitized. Nothing may move ahead of `Harden`.
  After it `main` sets `Deps.CatchSignals` to `app.CancelOnSignal`, which
  `Run` applies only once the message is read (or at once for `-q` and
  `--probe`; mailq, `--status` and `--check-config` keep the default
  action): SIGINT, SIGTERM and SIGHUP then cancel the call and a hook is
  killed with its group; before that the default action discards a
  message being typed. Signals ignored at start stay ignored.
  `sanitizeEnv` must stay idempotent (`FuzzSanitize`), or the re-exec loops.
  `MAILCRIER_CONFIG` dropped by the re-exec is reported after it through
  the argv marker `--ignored-env-config`, which `sendmail.Parse` accepts
  only as the first argument.
- `internal/sendmail.Parse(argv0, args)` is the only command line parser:
  getopt-style short flags from the `shortFlags` table, long options before
  `--` only, usage errors exit 64 before stdin is read.
  `Invocation.Envelope` builds sender and recipients. The flag table is
  repeated in README, `docs/mailcrier.8` and `--help` (`sendmail.Usage`);
  change all four together. `TestUsage*` check `Usage` against the parser
  tables and the exit statuses of the manual page, and its layout.
- `internal/message.Read` returns the message without Bcc and Resent-Bcc,
  their addresses separately as `BlindCopies` (routing only, never
  rendered), and constant warning texts that `app` logs as they are.
  MIME, encoded words and charsets are decoded there (stdlib plus
  `x/text`, own multipart splitter over slices of the input, `x/net/html`
  tokenizer for HTML to text); the input is held once, so a new step must
  not copy it per line or per part.
- `internal/text` (leaf): escapers per target markup, `MeasureTelegramHTML`,
  truncation. `internal/render`: `Data` (built by `NewData`, which drops
  Bcc-only and Resent-Bcc-only recipients), built-in templates in
  `defaults/<format>.tmpl` keyed by `text.Format`, `Fit` (a long subject
  cut to a quarter of the limit in the target's unit and marked `...`,
  then binary searches over the attachment list, the raw body and the
  subject, measured after escaping; when nothing fits, Telegram HTML is
  cut hard by `text.CutTelegramHTML`, any other lenient format at a
  character boundary, and the JSON formats and MarkdownV2 (`strictFormats`)
  give `ErrLimitTooSmall`; strictness comes from the format a template is
  parsed for, not from who wrote it; `FitLines` adds `max_lines`). Golden
  output of every template for every caller fixture is in
  `internal/render/testdata/golden`. `Parse` takes a template from the
  configuration: `define`/`template`/`block` rejected after parsing; it
  runs in a goroutine with a 1 s timeout and a writer that fails past
  1 MiB (the fields `userLimits`, which tests shrink); `WithBudget`
  gives a copy that `delivery` uses for the text of one target, all its
  executions within 2 s and the delivery deadline; a timed-out
  template stays broken for the process, and every failure, including
  `ErrLimitTooSmall`, is a `*TemplateError`; past the output limit `Fit`
  treats the text as too long when there is a limit. Built-in templates
  run without the goroutine. The functions (`funcs.go`) are shared.
  `ParsePart` is the same for a path, query or header template of the
  http target, which may render nothing; `Deadline` hands the budget of
  one copy on to the next, so parts and text share it.
- `internal/config.Load` parses strictly: an unknown key, a key of another
  target type, a bad name or value gives `*config.Error` with
  `path:line:col` and exit 78. URLs must be absolute http(s) (for
  `shoutrrr`: any URL with a scheme), tokens non-blank, `chat_id` an
  integer or a non-blank string, `channel` non-blank (both trimmed).
  Targets are `[target.<name>]` tables; keys per type are in
  `allowedKeys`, which lists only implemented keys, required ones in
  `requiredKeys`; a new key goes into `docs/mailcrier.8` in the same
  change (`TestManPageListsKeys`; `TestManPageListsOptions` does the same
  for the flags of `sendmail`). `Error.Path` is the absolute path,
  `"/" + path` of `ConfigFS`, also inside the texts of file errors
  (`fileErrorText`). `config.Check` is `Load` plus `Warning`s with
  positions (files readable by all, files and directories the group of
  the binary cannot read, the latter only with `group >= 0`, shoutrrr
  services with a native type, `slack-webhook` on Discord, routes without
  a catch-all, targets no route names, a hook that is not executable);
  `SecretKeys` lists the keys `app.registerSecrets` masks
  (`TestRegisterSecretsCoversSecretKeys` checks one way).
  `template_file` is read at load, but `config` does not import `render`:
  `app.buildTargets` parses the template, and a parse error rejects the
  configuration (78, message held). `[[route]]` and
  `[[suppress]]` are compiled at load (`compileRules`, globs through
  `compileGlob` into `(?is)^...$` RE2, `FuzzGlob`) into `Match`; their
  errors name the rule number and key, never the expression. `keyIndex`
  gives each element of an array of tables its own paths
  (`elementPath`: `route\x1f<i>\x1f<key>`), and pointer fields tell a
  key that is present. `telegram_direct_chats` is normalized at load
  (`setDirectChats`) the way `route.NormalizeChat` normalizes a recipient.
- `internal/backend`: `Sender`, `Caps`, `Payload`, `*Error` with `Class`,
  `IsPartial` (text arrived, files did not: counts as delivered),
  `Classify`, and the HTTP guards every target uses (`WithoutRedirects`,
  `TransportError`, `StatusError`, `Drain`). Targets live in
  `internal/backend/<name>` (`webhook` is type `http`, `hook` is type
  `exec`, both named off the stdlib package they use; Slack is plain Web
  API calls, no client library) and are mapped from config only in
  `internal/app.buildTargets`, each with its built-in template.
  `hook` asks for `Payload.Message` through `Caps.CanTakeMessage` (raw
  message without Bcc, envelope fields), runs `argv` in its own process
  group with a fresh environment and, from `app.hookProcess`, the real
  ids when `egid != gid`, `Pdeathsig` set from a locked thread; its tests
  run real `/bin/sh` scripts, `TestSpoolHook*` signal a helper process, and
  `TestSetgidHookDropsGroup` checks the ids under a real setgid bit.
  `shoutrrr` sits behind `//go:build !noshoutrrr` (`without.go` makes
  `New` fail, so the target is a configuration error); its client
  wrapper records the status and transport error of the last request,
  because the library reports them only as text that quotes the URL.
  Tests of either build carry the matching tag; `make test`, `build` and
  `lint-go` run both builds. Files tagged `setgid_e2e` or `smoke` are not
  in `make test`; `lint-go` type-checks them under their tags.
  `TestLiveTelegram` sends to the real Bot API only with
  `MAILCRIER_TELEGRAM_ENV` naming a file with `TELEGRAM_BOT_TOKEN` and
  `TELEGRAM_CHAT_ID`.
- `internal/route` (imports only `message`): `Router` from rules with
  compiled `*regexp.Regexp` (`app.newRouter` copies them from `config`),
  `Suppressed`, `Targets` (first match per recipient, `continue`, union
  over recipients, `Decision.IsImplicit` without rules, `Unrouted`),
  `Addresses` (recipients parsed by `message.ParseAddressList`, argv ones
  carry display names) and `SplitDirect` for `<chat>@telegram`.
- `internal/redact`: `app.Run` wraps every logger and the standard `log`
  output with one `Redactor` and registers tokens, URLs and header values after
  `config.Load`; a new secret-bearing config key must be registered in
  `app.registerSecrets`.
- `internal/delivery`: `Deliver` runs one goroutine per target that picks
  the files within `Caps` (`Target.MaxFileSize` over `Caps.MaxFileSize`),
  fits the target's template with `render.FitLines` (`Target.MaxText`
  over `Caps.MaxText`, `MaxLines`),
  puts the full text first among the files of a cut text per `OnLong`
  (`message.txt` in the plain template, or `message.eml` from
  `message.WithoutBlindCopies`, also the `Raw` of `Payload.Message`),
  and sends; after a `backend.Error` with
  `IsTextRejected` it sends that file alone once more. `Target.Fallback`
  is the built-in template when `Template` comes from the configuration:
  a `*render.TemplateError` renders the text with it, and a rejected text
  of `Template` goes once more in its text before the file rule;
  `Result.TemplateErr` carries the cause, which `app.logResult` logs as a
  warning. `Target.Request` (`RequestTemplates`, parsed by
  `app.parseRequest`) renders the path, query and header values first
  into `backend.Payload.Request`; a failure there is Perm without a send
  and without fallback (`request template failed, message not sent`).
  `Result.RequestErr` marks it for `app.logResult`. `Template` is nil for
  an http GET, which sends no text. A panic in the goroutine of a target
  is recovered into a permanent result, so a target must not share
  mutable state with another. `DeliverEach` also hands each result to a
  callback as its target finishes (the spool marks `Done` there).
  `ExitCode(results, queue)` is the exit status matrix, rules in order:
  Temp without a spool entry 73/74, any OK or all suppressed 0, any Perm
  69, Temp queued 0, else 69.
- `internal/spool` (imports only `message`): areas `tmp`, `queue`, `hold`,
  `failed`, `locks` under the spool directory; an entry is `<id>.eml` (the
  `message.Message.Raw` bytes, read back with `IgnoreDots`) and `<id>.json`
  (sidecar: `Version` 1, `OwnerUID`, state per target). Create reserves
  both files in `tmp/` (message truncated to its size) and only then scans
  the usage and checks the quota without its own entry, so parallel calls
  stay within the limits with no lock to wait for (a stopped caller holding
  one would stop the mail of the host); it then writes and syncs both,
  links the sidecar into the area, renames the message, and removes the
  sidecar from `tmp/` last, so a `.eml` always has its sidecar beside it;
  the message is flocked since `tmp/`. No spool lock is ever waited for:
  every flock is `LOCK_NB`. Move writes the new sidecar into the target
  area, renames the message, and removes the old sidecar last. So only
  Remove (sidecar first) leaves a `.eml` without sidecar, and `Lock`
  deletes such a file as finished; a stray `.json` without `.eml` is
  harmless and `RemoveStale` deletes it after an hour. The entry lock is
  `flock(LOCK_EX|LOCK_NB)` on the `.eml`, opened `O_NOFOLLOW`; after taking
  it the sidecar is read again and the open inode compared with the path.
  `locks/drain-<uid>.lock` is taken by the run after a call and by `-q` of
  an elevated user; `-q` of root, the service user or an unelevated caller
  takes none. `Save` and `Remove` do not fsync the directory (a lost rename
  only repeats a delivery). Quota usage reads every sidecar for `OwnerUID`.
  `OpenExisting` is for the listing modes and creates nothing.
- `internal/app`: `loadTargets` (load, `registerSecrets`, logger with
  `syslog_tag`, `client.Timeout`, `buildTargets`) serves `-q`, `mailq`,
  `--status`, `--check-config` and `--probe`; `Run` keeps its own sequence
  for a message, which holds it on an error. `admitServiceMode`
  (`check.go`) gives 77 to an elevated caller other than the service user
  and 64 to an ignored `--config` or `MAILCRIER_CONFIG` and to arguments
  of `--check-config`, before anything is loaded. With a valid file whose
  `dir` is not `Deps.SpoolDir`, `-q` also releases the `hold/` of
  `Deps.SpoolDir` (`queue.otherHold`), where a message held under a
  rejected file went; with a rejected file `-q` only expires entries and
  exits 78. `--check-config` (`check.go`) prints its findings to stderr and
  renders `sample.go`'s message through `delivery.Deliver` with
  `dryRunSender`; `--probe` (`probe.go`) sends it with `DeliverEach`,
  without spool, routes or suppression, and exits by `probeExitCode`
  (0, 69, 75), the only place of 75.
- `internal/app/queue.go`: own message (write-ahead, deliver, record each
  result, remove), `hold/` on a rejected configuration or without a route
  (`hold` takes the reason), queue runs (`hold/` released first, then
  `queue/`, oldest first), `mailq`, `--status`. Errors stored in sidecars
  pass through the redactor. `internal/app/routing.go`: `queue.decide`
  runs after `buildTargets` and before the spool: suppression (0, nothing
  spooled), direct chats, routes; no target gives `hold/` with
  `no route` and 64. `release` and `releaseInto` decide again through
  `routeHeld`, which reads the message only when there are rules or
  `telegram_direct`; warnings of a repeated decision go to debug, as
  `drainOwn` runs `hold/` after every call. `queue.target` builds the copy
  `<chat>@telegram` of the `telegram_direct` target (`directTarget`) on
  every lookup, so a queued copy takes the configuration of its retry.
  The targets of a `queue/` entry are never routed again.
- `TestImportGraph` (`import_graph_test.go`) enforces the package graph: a
  new package needs an entry in `allowedImports`.

## Tests

- `testdata/test-cases.tsv` is the test case registry: `id`, `priority`
  (`critical|high|low`), `status` (`todo|done`), `behaviour`, `source`. A
  subtest for a case is `t.Run("<ID>/<slug>", ...)` with the ID in a string
  literal; `TestIDsCovered` fails when a `critical` and `done` case has none,
  and on a subtest naming an ID the registry lacks; IDs look like
  `T-<ADJ|MTA|CALL|ESC|LIM|TPL|PKG>-<nn>`.
- Comments, docs and commit messages carry no references to plan stages or
  review and decision IDs; `scripts/check-refs` enforces it.
- Golden files: `internal/golden`, one txtar per fixture, `-update` rewrites.
- Caller fixtures: `testdata/callers/<name>.eml` and `<name>.argv` (argv[0]
  first, one argument per line), hand-written with `example.org` and
  `example.com` only; `TestCallers` compares each with
  `internal/app/testdata/callers/<name>.txtar`. A table test whose rows
  carry case IDs calls `t.Run("<ID>/<slug>", row.check)` per row, because
  `TestIDsCovered` reads only literals in the `Run` call.
- `TestReadmeExamplesLoad` loads the TOML blocks of the README section
  "Configuration" with `config.Load`. `TestReadmeTemplatesParse` parses
  every README TOML block that sets a template; `TestReadmeCheckConfigOutput`
  runs `--check-config` on the example under "Checking the configuration"
  and compares stderr with the text block after it, so a changed finding
  text needs the README edited.
- `TestFormerNameGone` fails when a tracked path or file carries the former
  name of the program in any case; the only exception is the fork line
  under the README title, once. It needs its own git work tree, whose top
  is the module directory: skipped outside one (a source archive, also one
  unpacked inside another repository), failed there when `CI` is set.
- Multi-process spool tests (`internal/app/spool_process_test.go`) start
  the test binary itself as `-test.run=^TestSpoolHelper$` with
  `MAILCRIER_SPOOL_HELPER` set; the helper reports on fd 3 and waits on fd
  4, the parent synchronizes on those pipes and on `wait4(WUNTRACED)`,
  never on sleeps. Run them repeatedly with
  `go test -race -count=5 -run 'TestSpool' ./internal/app`. Tests with
  real uids and the setgid bit are in `setgid_e2e_test.go`
  (`make setgid-e2e`). `internal/spool/move_crash_test.go` kills a helper
  at a named step of `Create` and `Move` the same way
  (`MAILCRIER_MOVE_HELPER_*`).
- `packaging/` holds the systemd unit and timer and the cron files of the
  queue run (`TestQueueRunnerFiles` pins their key lines), the
  configuration file of the packages and their maintainer scripts in
  `packaging/scripts/`; the packages are the `nfpms` section of
  `.goreleaser.yaml`. `TestPackageContents` pins that section entry by
  entry, `TestPackageScripts` runs each script with stub commands under
  `/bin/sh`, and also `dash` and `busybox sh` where installed,
  `TestPackageConfigExample` loads the configuration file.
  `TestConfigExamplesLoad` runs `--check-config` on each file of
  `packaging/examples` (the reference with `## ` lines dropped and `# `
  removed) and wants no finding; a new example goes into `configExamples`
  and into `contents` of `.goreleaser.yaml`.

## Gotchas

- `make licenses` lets the modules of `LICENSE_EXCEPTIONS` through
  (`--ignore`): `eclipse/paho.golang` is taken under EDL-1.0, which
  go-licenses does not detect. `TestLicenseExceptionsStillOffered` reads
  the module in the module cache and fails when the license file stops
  offering it; a new exception needs its own check there.
- `.gitignore` is an allowlist (`/*`, `.*`, then `!/path`): a new top-level
  file or directory stays ignored until listed. Check tracked files with
  `git ls-files | git check-ignore --stdin --no-index` and new paths with
  `git check-ignore <path>` (empty output). Dot-files inside allowed
  directories stay ignored, so fixture names must not start with a dot.
- `app.Run` sets `http_timeout` on a copy of `Deps.HTTP` and `deadline` on
  the context; a sender must pass the context to its requests, or the call
  deadline does not reach it. Error texts drop the request URL
  (`backend.WithoutURL`) because webhook and Bot API URLs carry tokens.
- The http target (`webhook.buildURL`) appends the rendered path and query
  to the configured URL and checks the result against its scheme, host,
  port and user: `backend` never imports `render`, so the parts arrive as
  strings. `app.registerSecrets` registers the text of header, query and
  path templates outside `{{ }}`, never what the actions render.
- nFPM writes the mtime of a script file into the apk, which neither
  `mtime` nor `SOURCE_DATE_EPOCH` changes; `make snapshot` sets it to the
  commit time first, so a workflow that publishes packages runs make, not
  goreleaser directly.
- `packaging/mailcrier.conf` is the conffile of the packages: any change to
  it in a release makes dpkg ask every administrator who edited the file,
  and rpm and apk write `.rpmnew` and `.apk-new` beside an edited one.
- `New` of every HTTP-based target copies the client with
  `backend.WithoutRedirects`:
  net/http would turn a redirected POST into a bodiless GET and report
  success. A 3xx is a permanent failure.
- Pull requests go to `develop` only: `main` takes code by fast-forward,
  so CI does not run on a PR into it; retarget such a PR to `develop`.
- GitHub runs `schedule` workflows only from the default branch `main`, at
  its last commit and with its version of the workflow, so a scheduled job
  changed on `develop` runs only after `main` fast-forwards to it.
- Releases are published only by `make release` in `release.yml`; it
  refuses to run without `GITHUB_ACTIONS=true`. A job of `ci.yml` renamed or
  added changes `RELEASE_JOBS` in the `Makefile` in the same commit
  (`TestReleaseWorkflow`), or the gate waits for a job that never runs.
- The changelog of goreleaser filters on the commit subject only, not the
  author: subjects `build(deps)`, `ci(deps)`, `test(deps)` (the prefixes
  Dependabot gets from `.github/dependabot.yml`) and `Bump ...` are left out
  of the release notes, a hand-written commit with such a subject as well.
- Without a final tag before the tagged commit the notes start at
  `CHANGELOG_START` (`12340d0`, the first commit of the rewrite): the 61
  upstream commits before it include two in Conventional Commits form.
- A release whose gate failed because the CI of its commit was not yet green
  is repeated with "Re-run failed jobs" of `release.yml` once it is. The tag
  ruleset forbids deleting a `v*` tag and updating it other than by
  fast-forward; a pushed tag is never moved at all: a mistake is fixed by
  the next version number.
- apk accepts a pre-release suffix only as letters and a number, so a tag is
  `-rc1`, never `-rc.1` (apk version `0.1.0_rc.1`, which it installs but
  cannot order), and `make snapshot` versions packages
  `<next>-rc<commit time>`: newer than every rc of that version, older
  than the release itself. `TestSmokeLifecycle` checks the installed apk
  version with `apk version -c`.
- Dependabot security updates are off in the repository settings. Turned
  on, they open against the default branch `main` whatever `target-branch`
  says and without the `commit-message` prefix, which `check-commits`
  rejects.
