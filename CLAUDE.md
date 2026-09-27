# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with
code in this repository.

## Commands

```sh
make check        # all CI checks of a push; govulncheck only warns here
make lint         # other targets: tidy test fuzz build licenses vuln
                  # check-refs check-commits snapshot
make fuzz FUZZTIME=10m   # longer fuzzing; check runs each target 10s
make setgid-e2e   # needs docker: TestSetgidReexec in a root container
go build -o slendmail ./cmd/slendmail   # new binary; the name is gitignored
go build -o /dev/null .                 # legacy root main.go, must still build
go test ./internal/backend/webhook -update   # rewrite golden files
go test ./internal/app -run TestCallers -update   # caller golden files
mandoc -T lint docs/slendmail.8         # man page; not part of make check
```

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
- `check-refs` and `check-commits` take `BASE` (default `origin/develop`):
  commits in `BASE..HEAD`, files changed since `BASE`, untracked included.
- zizmor runs `--offline` locally; CI sets `ZIZMOR_FLAGS=` and `GH_TOKEN`
  for the online audits (impostor commits, known vulnerable actions).
- `check-commits` does not limit the subject length of Dependabot commits,
  and `check-refs` checks only their subject: the subjects embed module
  paths, the bodies quote upstream release notes. Dependabot is recognized
  by its noreply author email, not by the author name.
- A manual run of the new binary reads `/etc/slendmail.conf`, or the file
  in `SLENDMAIL_CONFIG` or `--config` (honoured when not setgid-elevated).

CI: `.github/workflows/ci.yml` runs `make check`, `make snapshot` and
`make setgid-e2e` on push and PR to `develop` (the main branch) and a
blocking `make vuln` daily; CodeQL runs on `develop`. Actions are pinned by
commit SHA with the version in a comment, the e2e image
(`testdata/setgid-e2e/Dockerfile`) by digest. Dependabot waits 7 days
before proposing a new version.

## Architecture

The rewrite lives in `cmd/slendmail` (entry point) and `internal/`. The
legacy single-file `main.go` in the root is a feature reference, excluded
from golangci-lint and govulncheck, and stays until native targets replace
it.

- `internal/app.Run(ctx, Deps, args, stdin) int` handles one invocation;
  `Deps` carries the logger factory, config `fs.FS` rooted at `/`, HTTP
  client, host name, argv[0] (`Program`), stdout, stderr, uid/gid/egid,
  environment, the error of a failed re-exec and a uid-to-login lookup; the
  unexported `deliver` field lets tests replace delivery. `app.SystemDeps`
  builds the real ones (syslog `LOG_MAIL`, `/etc/slendmail.conf`,
  environment proxies only without elevation).
- Elevated means `egid != gid` and `uid != 0` (`internal/app/privilege.go`).
  `main` sets the umask, then calls `app.Harden` before `SystemDeps`, any
  logger or any time formatting (the time package opens a `TZ` path): an
  elevated process re-executes itself with `sanitizeEnv(environ)` unless
  the environment is already sanitized. Nothing may move ahead of `Harden`.
  `sanitizeEnv` must stay idempotent (`FuzzSanitize`), or the re-exec loops.
  `SLENDMAIL_CONFIG` dropped by the re-exec is reported after it through
  the argv marker `--ignored-env-config`, which `sendmail.Parse` accepts
  only as the first argument.
- `internal/sendmail.Parse(argv0, args)` is the only command line parser:
  getopt-style short flags from the `shortFlags` table, long options before
  `--` only, usage errors exit 64 before stdin is read.
  `Invocation.Envelope` builds sender and recipients. The flag table is
  repeated in README and `docs/slendmail.8`; change all three together.
- `internal/message.Read` returns the message without Bcc, the Bcc
  addresses separately (routing only, never rendered), and constant
  warning texts that `app` logs as they are.
- `internal/config.Load` parses strictly: an unknown key, a key of another
  target type, a bad name or value gives `*config.Error` with
  `path:line:col` and exit 78. URLs must be absolute http(s) (for
  `shoutrrr`: any URL with a scheme), tokens non-blank. Targets are
  `[target.<name>]` tables; keys per type are in `allowedKeys`, which for
  `http` lists only implemented keys. Types other than `http` with
  `preset = "generic-json"` are parsed but rejected by `app` as not
  implemented.
- `internal/backend`: `Sender`, `Caps`, `Payload`, `*Error` with `Class`,
  `Classify`. Targets live in `internal/backend/<name>` (`webhook` is type
  `http`) and are mapped from config only in `internal/app`.
- `internal/redact`: `app.Run` wraps every logger and the standard `log`
  output with one `Redactor` and registers tokens and URLs right after
  `config.Load`; a new secret-bearing config key must be registered in
  `app.registerSecrets`.
- `internal/delivery`: sequential `Deliver`; `ExitCode` is 0 when any target
  accepted the message or every target was suppressed, else 69.
- `TestImportGraph` (`import_graph_test.go`) enforces the package graph: a
  new package needs an entry in `allowedImports`.

## Tests

- `testdata/test-cases.tsv` is the test case registry: `id`, `priority`
  (`critical|high|low`), `status` (`todo|done`), `behaviour`, `source`. A
  subtest for a case is `t.Run("<ID>/<slug>", ...)` with the ID in a string
  literal; `TestIDsCovered` fails when a `critical` and `done` case has none.
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
  "Configuration (cmd/slendmail)" with `config.Load`.

## Gotchas

- `.gitignore` is an allowlist (`/*`, `.*`, then `!/path`): a new top-level
  file or directory stays ignored until listed. Check tracked files with
  `git ls-files | git check-ignore --stdin --no-index` and new paths with
  `git check-ignore <path>` (empty output). Dot-files inside allowed
  directories stay ignored, so fixture names must not start with a dot.
- `go build .` in the root also writes a binary named `slendmail`; use `-o`.
- `app.Run` sets `http_timeout` on a copy of `Deps.HTTP` and `deadline` on
  the context; a sender must pass the context to its requests, or the call
  deadline does not reach it. Error texts drop the request URL
  (`webhook.withoutURL`) because webhook URLs carry tokens.
- `webhook.New` copies the client and disables redirects: net/http would turn
  a redirected POST into a bodiless GET and report success. A 3xx is a
  permanent failure.
