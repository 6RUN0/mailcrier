# Mailcrier

Started as a fork of github.com/iggy/slendmail.

A sendmail replacement for machines that send no email: cron, at, sudo,
mdadm, smartd, fail2ban and every other tool that pipes a message into
`/usr/sbin/sendmail` get it delivered to Telegram, Discord, Slack, ntfy,
an HTTP webhook, a service of the shoutrrr library or a program of their
own instead.

- Accepts the command lines of the usual callers (`-t`, `-i`, `-f`, `-F`,
  `-oi`, `-bi`, `-q` and the rest) and reads MIME messages: encoded
  words, quoted-printable, base64, any charset, HTML parts and attachments.
- Sends to all configured targets at the same time, each in the markup of
  its service with everything from the message escaped, cut to the length
  the service accepts, with the attachments it can take.
- Keeps a message in a spool until every target has it: a service that is
  down gets it on a later queue run.
- One static binary, installed setgid so that the jobs of any user can
  send while only root reads the tokens; logs to syslog.

## Build

```sh
go build -o mailcrier ./cmd/mailcrier
```

With `-tags noshoutrrr` the binary leaves out the shoutrrr library and
the 12 modules it brings, about 3 MB of 13.7 MB (linux/amd64, `-trimpath
-ldflags='-s -w'`, go1.27.1); a `shoutrrr` target then rejects the
configuration with exit status 78 and `built without shoutrrr` in the log.
Releases carry both builds, the smaller one as `mailcrier-minimal`.

The binary goes to `/usr/sbin/mailcrier`, with `/usr/sbin/sendmail`,
`/usr/lib/sendmail`, `/usr/bin/newaliases` and `/usr/bin/mailq` as links
to it; see "Options, privileges and containers" for ownership and modes.
The manual page is `docs/mailcrier.8`.

## Install

Each release on <https://github.com/6RUN0/mailcrier/releases> has deb, rpm
and apk packages of the full build for amd64, arm64 and armv7, without
signatures (`apk add --allow-untrusted`), and `checksums.txt` with the
SHA-256 of every file. Every file listed there also has a build provenance
attestation of the release workflow, which the GitHub CLI checks:

```sh
sha256sum --ignore-missing -c checksums.txt
gh attestation verify --repo 6RUN0/mailcrier \
  --signer-workflow 6RUN0/mailcrier/.github/workflows/release.yml \
  ./mailcrier_<version>_linux_amd64.deb
apt install ./mailcrier_<version>_linux_amd64.deb
dnf install ./mailcrier_<version>_linux_amd64.rpm
apk add --allow-untrusted ./mailcrier_<version>_linux_amd64.apk
```

| Path | Owner and mode |
|---|---|
| `/usr/sbin/mailcrier` | `root:mailcrier 2755`, set by the scripts after unpacking |
| `/usr/sbin/sendmail`, `/usr/lib/sendmail`, `/usr/bin/mailq`, `/usr/bin/newaliases` | links to it; in rpm the alternative `mta` |
| `/etc/mailcrier.conf` | `root:mailcrier 0640`, no target |
| `/etc/mailcrier.d/` | `root:mailcrier 0750`, for `token_file` and the like |
| `/var/spool/mailcrier/` | `root:mailcrier 0750`; `tmp`, `queue`, `hold`, `failed`, `locks` in it `2770` |
| `mailcrier-queue.timer` and `.service`, `/etc/cron.d/mailcrier` | queue run, deb and rpm |
| `/etc/crontabs/mailcrier` | queue run, apk |
| `/usr/share/doc/mailcrier/examples/` | example configurations, see "Configuration" |
| `/usr/share/selinux/packages/mailcrier/mailcrier.cil` | SELinux module, rpm, see below |

The install creates the system group and user `mailcrier` (home
`/var/spool/mailcrier`, no login shell) unless they exist. Then, as root:

1. Write a target into `/etc/mailcrier.conf`; its comments and the examples
   show how. A secret goes alone into a file under `/etc/mailcrier.d`,
   `root:mailcrier 0640`.
2. `mailcrier --check-config` reports errors and warnings, exit status 0
   when the file loads.
3. `mailcrier --probe` sends a test message to every target.

On upgrade, rpm and apk set the owner and mode of `/etc/mailcrier.conf`
back to `root:mailcrier 0640` when its content is that of the package;
dpkg keeps them. A secret therefore belongs in a file under
`/etc/mailcrier.d/`, where the package ships no file and changes none.

Until a target is set, every message is held in `hold/` with exit status
78, and `mailcrier-queue.service` exits 78 every 5 minutes and is listed by
`systemctl --failed`; the first queue run after the configuration is fixed
delivers the held messages.

The timer is enabled on install and started where systemd runs; where it
does not, the cron file runs the queue. The package scripts never run
mailcrier and never touch the configuration.

- Debian and Ubuntu: the package provides, conflicts with and replaces
  `mail-transport-agent`, so installing it removes the MTA in place
  (postfix, exim4, msmtp-mta, ...), and removing mailcrier does not bring
  that one back. The setgid bit is a `dpkg-statoverride` entry, which dpkg
  applies to every later version; one the administrator set before is
  kept.
- RHEL, Rocky, Alma, Fedora: the links are the alternative `mta` with
  priority 100, above postfix (60) and sendmail (90), so mailcrier is the
  active one after install; `alternatives --config mta` chooses another.
  After removal the remaining MTA is active again. The package installs an
  SELinux module that labels mailcrier like a stock MTA, so confined
  callers such as smartd can mail; a hook then runs in the mail domain of
  its caller, see [docs/selinux.md](docs/selinux.md). cronie decides when it
  starts whether it mails the output of jobs: a crond started while no
  MTA was installed logs that output until `systemctl restart crond`, and
  the install prints a reminder while crond runs.
- Alpine: the package replaces the BusyBox link `/usr/sbin/sendmail`, and
  BusyBox puts it back after removal. ssmtp, dma and opensmtpd own
  `/usr/sbin/sendmail` as well: apk refuses to overwrite it (`trying to
  overwrite usr/sbin/sendmail owned by ...`), so remove them first. The
  queue runs from BusyBox crond, which Alpine does not start by default:
  `rc-update add crond && rc-service crond start`. Without it a message
  that failed is retried only by the next call of the same user.

In rpm and apk the binary is unpacked as `root:root 0755` on install and on
every upgrade, and the install script then gives it the group and the
setgid bit. Consequences:

- `rpm -V mailcrier` reports `.M....G..  /usr/sbin/mailcrier` and `apk audit
  --check-permissions` reports `M usr/sbin/mailcrier`; that is expected.
- During an upgrade, between unpacking and the script, a call of a user
  other than root cannot read the configuration, and `hold/` is not
  writable for that user either: the message is lost (`message lost, not
  held`) and the call exits 73.
- `rpm --setperms`, `rpm --setugids`, `rpm --restore`, an install or
  upgrade with `--noscripts`, `apk fix` and `apk add --no-scripts` remove
  the bit for good, with the same result for every such call until it is
  restored as root:
  `chgrp mailcrier /usr/sbin/mailcrier && chmod 2755 /usr/sbin/mailcrier`
  (`chgrp` first: it clears the bit).

Removal (`dpkg -r`, `rpm -e`, `apk del`) stops the timer. It keeps the
user, the group and a changed configuration (`dpkg -P` deletes it, `rpm -e`
keeps it as `/etc/mailcrier.conf.rpmsave`). A spool without messages in
`queue/`, `hold/` and `failed/` is removed with the package; one with
messages stays, and every format prints its path on removal (deb on
`dpkg -r` and on `dpkg -P`). A call during the removal can leave the spool
with nothing but `locks/` in it, and files an interrupted call left in
`tmp/` keep it as well (dpkg reports the directory as not empty);
such a directory holds no message and can be deleted. Debian keeps
`/etc/cron.d/mailcrier` until `dpkg -P`; its job does nothing while the
binary is missing.

## Configuration

The program reads `/etc/mailcrier.conf`, a TOML file. Every run parses the
file strictly: an unknown key, a key that the target type does not use, or
an invalid value rejects the file, the message is not delivered but held
in the spool (see "Spool and queue"), and the exit status is 78. A package
installs an example without targets, so until targets are configured every
message is held with status 78; that is expected.

Example configurations to copy whole are installed in
`/usr/share/doc/mailcrier/examples/` and kept in
[`packaging/examples/`](packaging/examples/):

| File | Shows |
|---|---|
| `mailcrier.conf` | every table with its defaults and one minimal target of each type, as comments like the installed file |
| `telegram.conf` | one Telegram chat with a forum topic, `on_long` and `max_lines` |
| `team-chat.conf` | Slack, Discord and Mattermost together |
| `ntfy.conf` | ntfy with the file limit of ntfy.sh and a template |
| `webhook.conf` | `http` targets with `method`, headers, path and query templates and `generic-json` |
| `hook.conf` | an `exec` hook with `argv` and `timeout`, the message on stdin |
| `routes.conf` | several targets, routes by recipient and subject, `continue`, a catch-all rule, `[[suppress]]` for a noisy cron job, `telegram_direct` |
| `templates.conf` | `template` and `template_file` with the template functions |
| `container.conf` | a container without the setgid bit: the spool on a volume, secrets in `/run/secrets` |

```toml
[general]
syslog_tag = "mailcrier"        # optional, default "mailcrier"
http_timeout = "15s"            # optional, one HTTP request
deadline = "30s"                # optional, delivery to all targets

[strings]                       # optional, notices in place of missing content
no_subject = "(no subject)"
empty_body = "(empty body)"
truncated = "[truncated]"
truncated_size = "[truncated, %s in full]"
more_attachments = "... and %d more"
not_sent = "[not sent]"

[target.hook]                   # the table key is the target name: [a-z0-9-]
type = "http"
url_file = "/etc/mailcrier.d/hook.url"  # or url = "https://..."
preset = "generic-json"
```

- Exactly one of `url` and `url_file` is set; likewise `token` and
  `token_file`. A `*_file` key takes an absolute path; the file content, with
  surrounding whitespace removed, is used as the value.
- `[strings]` replaces the English notices that stand in for missing
  content: `no_subject` for a message without a subject, `empty_body` for
  a body without visible text, `truncated` at the end of a body cut to the
  length limit of a target, `truncated_size` in its place when the full
  text does not go along as a file, with `%s` for its size (exactly one
  `%s` and no other `%`, else exit status 78; when only `truncated` is set,
  it stands there too, without the size), `more_attachments` after a
  list of attachments cut to that limit, with `%d` for the number left out
  (exactly one `%d` and no other `%`, else exit status 78), `not_sent` after
  an attachment listed in the text but not sent because it exceeds a file
  limit of the target. Each is optional and must not be blank; the values
  above are the defaults. The `generic-json` preset uses none of them: it
  carries subject and body as they are, empty when the message has none,
  and has no length limit.
- A text longer than the target accepts is cut: first a subject longer
  than a quarter of the limit (at least 64 units of the target) is cut at
  a word and ends with `...`, then the list of attachments, then the body,
  which ends with the `truncated` notice. The limit is counted the way the
  service counts it, after escaping. When the rest of the text, host and
  sender included, is too long on its own, the text is cut hard; for
  Telegram between tags and entities, with the open tags closed. What goes
  along with a cut text is set per target, see "Long messages".
- Exit status: see "Exit status" below.
- Logging goes to syslog, facility `mail`. Where no syslog socket exists (a
  container without `/dev/log`) the records go to stderr, with time and
  level, after one `syslog unavailable` warning; PHP-FPM passes the stderr
  of a worker to its own log only with `catch_workers_output = yes` in the
  pool configuration. A setgid-elevated process without syslog writes to
  stderr only `mailcrier: <message>` for warnings and errors, without
  fields, because its caller must not see what only the group may read.
  Each record is a constant message with logfmt fields: `call` (16 hex
  digits, one value per invocation) on every record, `msgid` (the
  Message-ID header, cut to 256 bytes, absent when the message has none)
  and `size` (bytes read) for the message, `target`, `class` (`temp` or
  `perm`), `status` (the HTTP status, when the service answered) and
  `retry_after` (the delay the service asked for) for a failed target.
  `hook output` (info, warning when the run failed) carries what an
  `exec` hook printed, see "Targets".
  `text truncated for target` (info) names a target that got a cut text,
  `text rejected, sent as file` (warning) one that refused the text and
  got it as a file, `text rejected, file failed too` (warning) one that
  took neither, `attachments not delivered` (warning) one that took
  the text but not the files; the message counts as delivered there.
  `template failed, built-in used` (warning) names a target whose
  template from the configuration failed, `request template failed,
  message not sent` (warning) an `http` target whose path, query or
  header template failed, see "Templates".
  Records about a spool entry carry its `id`, the name `mailq` lists,
  from the moment the message is written to the spool: those of the
  call and its targets and `hook output` too. `message held`, `message
  lost` and `message failed` carry `reason`, `message failed` also `area`
  (the spool directory the entry left) and `targets` (those that still
  waited), `message lost` and `message not delivered` the `targets`
  concerned; a lost message gets `message lost` alone. Headers and body
  of the message are never logged, nor is the response body of a
  service.
- Tokens and URLs, including the content of `*_file`, are replaced by `***`
  in every log record and in the debug output of the Go HTTP stack. A URL
  is masked whole, and so are its host, host labels, request URI, path
  segments and query values of 16 characters or more (the request URI also
  when it has a query) and its userinfo.
- `http_timeout` bounds one HTTP request from dialing to the end of the
  response; `deadline` bounds the delivery to all targets together. Both
  take Go duration strings (`"500ms"`, `"15s"`, `"1m"`) and must be
  positive. A target that runs out of either counts as a temporary failure.
- Proxy environment variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`)
  are honoured without elevation and ignored by a setgid-elevated process.
- The file mode creation mask is always `007`.

### Targets

Every target of the file receives every message unless routes or direct
chats choose the targets, see "Routes and suppression"; the targets of a
message are sent to at the same time. Each sends the text in the markup of its
service, escaped so that nothing in the message becomes markup, a link
preview or a mention; `shoutrrr` sends plain text, and `exec` hands on the
message itself instead.

`max_file_size`, a key of every type but `http`, `exec` and `shoutrrr`,
sets the size limit of one file in bytes, in place of the one given below
for the type (`slack` has none); it must be a positive integer, else exit
status 78. A file over it is listed with the `not_sent` notice and not
sent, and a cut text whose full text exceeds it ends with the
`truncated_size` notice. A value above the limit of the service is taken as
it is, and the service rejects a bigger file.

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/tg.token"   # or token = "123456:ABC..."
chat_id = -1001234567890                   # or "-1001234567890", "@channel"
message_thread_id = 42                     # optional, forum topic
disable_notification = true                # optional, silent messages
```

- `telegram`: `chat_id` (an integer or a non-blank string) is required.
  `sendMessage` with `parse_mode` HTML and link previews off, then one
  `sendDocument` per attachment; a text of at most 1024 characters goes as
  the caption of the first document instead, unless that file is empty. The
  text holds at most 4096 characters as Telegram counts them, after
  entities; a character outside the BMP, such as an emoji, counts once. Up
  to 10 files of at most 50 MB each are sent, the others are listed with the
  `not_sent` notice. An answer with `"ok": false` is a failure even with
  status 200; `error_code` 429 and 5xx are temporary, with
  `parameters.retry_after` logged as `retry_after`. When the text arrived
  and a document did not, the message counts as delivered and
  `attachments not delivered` is logged.

```toml
[target.ops-discord]
type = "discord"
url_file = "/etc/mailcrier.d/discord.url"  # https://discord.com/api/webhooks/<id>/<token>
```

- `discord`: one webhook execution per message with `wait=true`, so that
  Discord answers after storing the message, and `allowed_mentions` with an
  empty `parse` list, so that `@everyone`, `@here` and user or role
  mentions in the message do not ping. A query of the URL, such as
  `thread_id`, is kept. The text holds at most 2000 characters (counted in
  UTF-16 units); up to 10 files of at most 20 MiB and 25 MiB together go
  in the same request. As for every target, 429, 5xx and any answer with
  a `Retry-After` header are temporary, any other failure is permanent.

```toml
[target.prod-slack]
type = "slack"
token_file = "/etc/mailcrier.d/slack.token"  # bot token, xoxb-...
channel = "#alerts"                          # channel name or ID
```

- `slack`: `channel` is required and must not be blank; white space
  around it is dropped. `chat.postMessage` of the Web API with the bot
  token as a bearer token and link unfurling off, then per attachment
  `files.getUploadURLExternal` and an upload, and one
  `files.completeUploadExternal` that shares the files in the thread of
  the message; the bot needs `chat:write` and `files:write` and must be a
  member of a private channel. `&`, `<` and `>` of the message are
  escaped, so `<!channel>` or `<@U123>` stays text. The text holds at most
  40000 characters; up to 10 files are sent. An answer with `"ok": false`
  is a failure; `ratelimited`, `internal_error`, `fatal_error`,
  `service_unavailable` and `request_timeout` are temporary. Several
  `slack` targets may post with different tokens to different channels.

```toml
[target.phone]
type = "ntfy"
# https://ntfy.sh/<topic>, user:password@ allowed
url_file = "/etc/mailcrier.d/ntfy.url"
max_file_size = 2000000         # optional, ntfy.sh takes files of 2 MB
```

- `ntfy`: a JSON publish to the server root with the topic, the subject
  as title (line breaks as spaces, at most 1 KB) and the text of at most
  4096 bytes, then one PUT per attachment to the topic with `filename` and
  `title` in the query; up to 10 files of at most 15 MiB each, the
  defaults of an ntfy server. ntfy.sh takes files of at most 2 MB, so a
  target there sets `max_file_size`. The URL must end with the topic,
  otherwise exit status 78; user information and a query such as `auth`
  are sent with every request.

```toml
[target.mm]
type = "http"
preset = "mattermost"           # mattermost | slack-webhook | generic-json
url_file = "/etc/mailcrier.d/mm.url"
username = "mailcrier"          # optional, mattermost only
channel = "alerts"              # optional, mattermost only

[target.api]
type = "http"
preset = "generic-json"
url = "https://api.example.org/notify"
method = "PUT"                  # optional: POST (default), GET, PUT, PATCH

[target.api.headers]            # optional, values are templates
Authorization = "Bearer api-token"

[target.ping]
type = "http"
method = "GET"
url = "https://status.example.org/api/push/abc"
path = "/{{ pathSegment .Hostname }}"   # optional, after the path of url

[target.ping.query]             # optional, values are templates
status = "down"
msg = "{{ .Subject | truncate 200 }}"
```

- `http`: one request of the JSON document of its preset, or of its
  template (see "Templates"), with `Content-Type: application/json` and no
  files. Exactly one of `preset`, `template` and `template_file` is set.
  `method` is `POST`, the default, `GET`, `PUT` or `PATCH`, in capitals;
  any other value exits 78. A `GET` sends no body and no Content-Type, so
  it takes none of `preset`, `template`, `template_file`, `max_text` and
  `max_lines`; the message reaches the service through `path`, `query`
  and the headers.
  `slack-webhook` is cut to 40000 characters, the length Slack keeps;
  `mattermost` has no limit, since Mattermost splits a long text into
  several posts, nor has `generic-json`. `mattermost` posts
  `{"text": ...}` with subject, host, sender and body in Markdown,
  `@channel`, `@all` and `@here` disarmed, and `username` and `channel`
  added when set; a set `channel` must not be blank, white space around
  it is dropped. `max_text` and `max_lines` (see "Long messages") apply to
  every preset. `slack-webhook` posts `{"text": ...}` in Slack mrkdwn
  with `&`, `<` and `>` escaped; `generic-json` posts
  `{"subject": ..., "body": ..., "hostname": ...}`.
  `[target.<name>.headers]` adds request headers after the Content-Type,
  so a `Content-Type` there replaces it; a header name that is not an
  HTTP token, a value with a line break or NUL, `Host`,
  `Content-Length`, `Transfer-Encoding` or `Trailer`, which the HTTP
  client sets itself and would drop, and `Connection`, `Keep-Alive`,
  `Proxy-Connection` or `Upgrade`, which HTTP/2 does not carry, exit 78.
  `path` is appended to the path of `url`, and `[target.<name>.query]`
  adds its values to the query of `url`, encoded, after the query `url`
  has. The values of `headers` and `query` and `path` are templates (see
  "Templates") rendered for every message; scheme, host and port come
  from `url` or `url_file` alone. A path that renders empty appends
  nothing, and the request goes to `url` as it is. A rendered path that
  does not start with a single `/`, holds `?`, `#`, a backslash, a control
  character or a `.` or `..` segment (escaped once or twice, or followed by
  a `;` parameter), or a rendered header value with a line break or NUL
  fails the target permanently before any request; an escaped `/` in the
  path stays escaped. What `path` writes before its first `{{ }}` action is checked
  by these rules when the configuration is loaded, exit status 78. A
  value from the message goes into a path segment through `pathSegment`,
  which escapes `/` as well, but not dots: a value that is `.` or `..`, or
  holds `/../`, still fails the target for that message. The text of
  header and query values outside the `{{ }}` actions, 8 characters or
  more, and the credential after a scheme such as `Bearer`, are masked in
  the log like tokens, and so is every piece of the text of `path` outside
  the actions between two slashes of 16 characters or more, as written
  and unescaped. A redirect is not followed and counts as a permanent
  failure.

```toml
[target.run]
type = "exec"
argv = ["/usr/local/bin/notify", "--channel", "ops"]
timeout = "30s"                 # optional, the default
```

- `exec`: runs a program once per message. `argv` is the command line,
  run without a shell; `argv[0]` must be an absolute path, and no element
  may hold a NUL, else exit status 78. Nothing of the message goes into
  `argv`.
  - stdin: the message as it was read, without its Bcc and Resent-Bcc
    fields. The hook gets no text and no files, so the keys about text
    and files are not valid for it.
  - Environment: only `PATH=/usr/sbin:/usr/bin:/sbin:/bin`,
    `LANG=C.UTF-8`, `TZ` when the caller set it (a setgid call keeps only
    a zone name), and `MAILCRIER_SUBJECT` (the decoded subject),
    `MAILCRIER_FROM` (the address of From, or the envelope sender),
    `MAILCRIER_TO` (the envelope recipients joined by `, `, without the
    addresses only Bcc or Resent-Bcc named), `MAILCRIER_HOSTNAME`,
    `MAILCRIER_TARGET` (the target name), `MAILCRIER_MSGID` (the
    Message-ID, empty when there is none) and `MAILCRIER_SIZE` (the bytes
    on stdin). In each value CR, LF and NUL become spaces, and the value
    is cut at a character to 4096 bytes; the whole environment stays
    within 64 KiB. The values come from whoever wrote the message: a hook
    quotes them (`"$MAILCRIER_SUBJECT"`) and never hands them to `eval`
    or `sh -c`.
  - Outcome: exit status 0 is a delivery; 75 (`EX_TEMPFAIL`) a temporary
    failure, queued and retried; any other status, or death by a signal,
    a permanent failure. A hook that cannot be started (missing file, no
    execute permission, `E2BIG`) fails permanently.
  - Time: `timeout`, a positive Go duration, 30 s by default, bounds one
    run; so do `deadline` and the budget of a queue run, whichever ends
    first. Past it the process group of the hook (each hook starts in
    its own) gets `SIGKILL` and the run is a temporary failure; a child
    that left the group with `setsid` survives. After the hook exits, the
    call waits at most 2 s for children that still hold its stdout or
    stderr, and then goes on without them. `SIGINT`, `SIGTERM` or
    `SIGHUP` to mailcrier end the run the same way: the group is killed,
    the failure is temporary and the message stays queued (a second
    signal ends mailcrier at once). This holds once the message is read;
    a signal while it is still coming in, such as Ctrl-C while it is
    typed, ends mailcrier and discards the message, as sendmail does. A
    signal that mailcrier was started with ignored, as under `nohup`,
    stays ignored. When mailcrier dies by `SIGKILL`, the kernel kills the
    hook itself (parent death signal), but children the hook started
    survive and finish on their own.
  - Output: stdout and stderr together, the first 4096 bytes, go into one
    `hook output` record with `target`, `output` and `output_size` (all
    bytes printed), the secrets of the configuration masked.
  - Ids: a setgid call starts the hook with the real uid and gid of the
    caller and the caller's supplementary groups, without the `mailcrier`
    group, so the hook reads neither the configuration nor the spool. A
    queue run as the `mailcrier` user (the systemd timer or the cron file)
    starts the hooks of queued messages as `mailcrier` with the group
    `mailcrier`, which reads the configuration with all tokens and every
    spool entry: the hook program must be trusted that far. The working
    directory is that of the caller.
  - Secrets: `argv` is visible to every local user through `ps` and
    `/proc/<pid>/cmdline` while the hook runs, and mailcrier does not
    mask it in the log, so it holds no token. A hook that needs one reads
    it itself; started by a setgid call it runs as the caller, so the
    secret is readable by every user who can run `sendmail`. Where that is
    not acceptable, the hook has to refuse to run as anyone but
    `mailcrier` (exit 75 keeps the message queued for the queue run of
    that user), and then reads what `mailcrier` reads, as said above.

```toml
[target.bus]
type = "shoutrrr"
url_file = "/etc/mailcrier.d/bus.url"   # a service URL, gotify://host/token
```

- `shoutrrr`: one message through a service of the shoutrrr library
  (github.com/nicholas-fedor/shoutrrr v0.21.1), chosen by the scheme of the
  URL: `gotify`, `matrix`, `teams`, `pushover`, `smtp`, `generic` and the
  others its documentation lists. The text is that of the plain template
  (subject, host, sender, body), without files and without a title; there
  is no length limit of mailcrier's, each service cuts or splits a long
  text as the library does. An unknown scheme or a URL the service rejects
  exits 78 when the configuration is loaded. `matrix` takes an access token
  only (`matrix://:token@host`): with a user name the library logs in while
  the configuration is loaded, outside `http_timeout` and `deadline`, so
  such a URL exits 78 without contacting the server. A failure after an
  HTTP answer outside 2xx is classified by its status, as for the other
  targets; a redirect is not followed and fails permanently even where the
  library would count it a success; any other failure is temporary and
  stays queued until `queue_ttl`. The services that send over HTTP use
  `http_timeout` and the proxy rule above; `smtp`, `xmpp` and `mqtt` open
  their own connections without a proxy, bounded only by `deadline`. Error
  texts of the library may quote the URL, which is masked in the log as
  every URL is.

### Long messages

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/tg.token"
chat_id = -1001234567890
on_long = "file"        # optional: file | truncate | blockquote
long_file = "text"      # optional: text (message.txt) | eml (message.eml)
max_text = 2000         # optional, below the limit of the service
max_lines = 40          # optional, lines of the body in the text
```

- A text over the limit of a target is cut as described under
  "Configuration". With `on_long = "file"`, the default, a target that
  sends files gets the full text as `message.txt` (`text/plain`, UTF-8,
  the message in the plain layout without a limit) as its first file,
  listed first among the attachments of the text, which ends the cut body
  with the `truncated` notice. `long_file = "eml"` sends the message itself
  as `message.eml` instead, as it was read, without its `Bcc` and
  `Resent-Bcc` fields; a queued message is sent the same way.
- `on_long = "truncate"` sends the cut text alone; so does a target without
  files (`http`) and a target whose file limit the full text exceeds. The
  body then ends with the `truncated_size` notice, such as
  `[truncated, 1.9 MiB in full]`, with the size of `message.txt`.
- `on_long = "blockquote"` is `file` with the cut body in an expandable
  blockquote in Telegram, which shows a few lines until opened; the other
  targets have no such block and treat it as `file`.
- `max_text` replaces the text limit of the service, in its unit: the
  characters of Telegram and of the `mattermost` and `generic-json`
  documents, the UTF-16 units of Discord, Slack and the `slack-webhook`
  document, the bytes of ntfy. A value above the limit of the service is
  taken as it is, and the service rejects a longer text. For `http` the
  limit covers the whole JSON document: one too small for the document
  without body and subject fails the target permanently.
  `max_lines` cuts a body of more lines after that many and treats the
  text as cut; trailing empty lines do not count. Both must be positive;
  `long_file`, like `on_long`, is not a key of `http`.
- A Telegram target that answers 400 to the text, for markup it cannot
  parse or a text over its limit, gets the full text once more as a file
  alone, without text or caption, whatever its `on_long`; the log records
  `text rejected, sent as file` (warning). When that fails too, the record
  is `text rejected, file failed too` (warning) and the target fails with
  the error of the second attempt. Every 400 counts, a wrong `chat_id`
  included, which costs one more request.

### Templates

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/tg.token"
chat_id = -1001234567890
template = '''
<b>{{ .Subject | default .Strings.NoSubject | tgHTML }}</b>
<i>{{ .Hostname | tgHTML }}</i> {{ .Date | tz "Europe/Berlin" | date "15:04" }}
<pre>{{ .Body | default .Strings.EmptyBody | trimEnd | tgHTML }}</pre>'''

[target.api]
type = "http"
url = "https://api.example.org/notify"
template = '''
{"title": {{ toJson .Subject }}, "host": {{ toJson .Hostname }},
 "text": {{ toJson .Body }}}'''
```

- `template`, a Go `text/template` (<https://pkg.go.dev/text/template>),
  replaces the built-in template of a target of any type but `exec`;
  `template_file`, an absolute path, names a file that holds it, read when
  the configuration is loaded, without the line break at its end. Not both;
  a blank template or file exits 78. An `http` target takes exactly one of
  `preset` and a template, so `username` and `channel` are not available
  with a template.
- The template writes the markup of the target: Telegram HTML (the Bot
  API parse mode is HTML, there is no other), Discord markdown, Slack
  mrkdwn, the plain text of ntfy (the title stays the subject) and of
  `shoutrrr`, the JSON request body of `http`, sent with
  `Content-Type: application/json` unless a header replaces it. Everything
  from the message comes from whoever wrote it and goes through the
  escaper of that markup: `tgHTML`; `discordEscape`, `discordCode` inside a
  code block; `slackEscape`, `slackCode`; `mmEscape`, `mmCode` for
  Mattermost markdown; `toJson` for a JSON value.
- Secrets stay out of the `{{ }}` actions: an error quotes the action it
  failed in, and goes to the log. A token belongs in a header, in the URL,
  or in the template text outside the actions, which no error quotes.
- A template that does not parse, calls an unknown function or uses
  `define`, `template` or `block` rejects the configuration: exit status
  78, the message is held, and `configuration rejected` names the target
  and the line in the template.
- The data:

  | Field | Content |
  |---|---|
  | `.Subject` | decoded Subject, empty when the message has none |
  | `.From` | From address, else the envelope sender: `.From.Name`, `.From.Addr`, `.From.String` |
  | `.Sender`, `.SenderName` | envelope sender and the name of `-F` |
  | `.To`, `.Cc` | addresses of the headers, like `.From` |
  | `.Recipients` | envelope recipients without the addresses only Bcc or Resent-Bcc named |
  | `.Date`, `.ReceivedAt` | the Date header (the receive time without one) and the receive time |
  | `.MessageID` | the Message-ID header |
  | `.Headers` | all header fields but Bcc and Resent-Bcc: `.Headers.Get "X-Cron-Env"` (the first value), `.Headers.Values` (all), `.Headers.Has`, `.Headers.Names`, names in any case |
  | `.Body`, `.BodyHTML` | the text, and the HTML part when there is one |
  | `.Attachments`, `.MoreAttachments` | `.Name`, `.ContentType`, `.Size`, `.IsSkipped` (listed, not sent) of each attachment listed, and the number left out |
  | `.Hostname`, `.Target`, `.Limit` | host name, target name, text limit of the target (0: none) |
  | `.IsCollapsed` | the cut body belongs in a collapsed block (`on_long = "blockquote"`) |
  | `.Strings` | the notices of `[strings]`: `.Strings.NoSubject`, `.Strings.EmptyBody`, `.Strings.Truncated`, `.Strings.MoreAttachments`, `.Strings.NotSent` |

- The functions, besides those of `text/template` (`printf`, `len`,
  `index`, `urlquery` and the others):

  | Function | Result |
  |---|---|
  | `toUpper`, `toLower`, `trimSpace`, `trimEnd`, `title` | the string in capitals, in small letters, without white space around it, without it at the end, with the first letter of every word capitalized and the rest kept |
  | `default D V` | `V`, or `D` when `V` is empty or white space only |
  | `join SEP LIST` | the strings or addresses of `LIST` joined by `SEP`: `{{ .To \| join ", " }}` |
  | `match RE S`, `reReplaceAll RE REPL S` | whether the Go regular expression `RE` matches in `S`; `S` with every match replaced, `$1` in `REPL` for a group |
  | `tz ZONE T`, `date LAYOUT T` | `T` in the IANA zone `ZONE`, read from the zone database of the system, without which any zone but `UTC` and `Local` fails the template, as an unknown one does; `T` in a Go layout: `{{ .Date \| tz "UTC" \| date "2006-01-02 15:04" }}` |
  | `lines S`, `head N S`, `tail N S` | the lines of `S`, for `range`; its first and last `N` lines |
  | `truncate N S` | `S` cut to `N` characters, the last three of them `...`; without them when `N` is 3 or less |
  | `indent N S` | `N` spaces before every line of `S` |
  | `humanizeBytes N` | a size: `1.5 KiB` |
  | `toJson V` | `V` as a JSON value, always valid |
  | `pathSegment S` | `S` escaped for one segment of a URL path, `/` included |
  | `tgHTML`, `discordEscape`, `discordCode`, `slackEscape`, `slackCode`, `mmEscape`, `mmCode` | the escapers above |

- The text is cut to the limit of the target as a built-in one is (see
  "Configuration" and "Long messages"): the subject, the attachments and
  the body are cut and the template rendered again. That works for a
  template whose text grows with them; what it adds on its own is cut
  hard, and an `http` body that does not fit `max_text` at all fails the
  template.
- Cutting a long text renders the template again and again, up to a few
  dozen times. All the renderings of the text of one message for one
  target may take 2 s together, ending no later than `deadline`, and each
  one 1 s and 1 MiB of output. Past 1 MiB a text with a limit counts as
  too long and its body is cut; without a limit (`http` without
  `max_text`, `shoutrrr`) the template fails. A template stopped while
  rendering, by either time limit, stays failed for the rest of the call
  or queue run, so that each further message gets the built-in template at
  once. A rendering due after the time is spent is not started: that
  message gets the built-in template, and the template stays in use.
- A template that fails while rendering a message (an error of a
  function, the time or size limit, a text of white space only, an `http`
  body over `max_text`) leaves that message the built-in template of the
  target, and the log records `template failed, built-in used` (warning)
  with `target` and `err`; the exit status follows the delivery, 0 when the
  target took it. A Telegram target that answers 400 to the text of the
  template gets the text of the built-in template once, with the same
  record, before the full text goes as a file alone (see "Long messages").
- The path, query and header templates of an `http` target (see
  "Targets") are rendered under the same limits, from the same budget as
  the text; each may render nothing, which leaves the value empty. They
  have no built-in template to fall back to: when one fails, the target
  fails permanently, gets nothing for that message, and the log records
  `request template failed, message not sent` (warning) with `target`
  and `err`. A parse error of one of them names
  `<target>.path`, `<target>.query.<name>` or `<target>.headers.<name>`
  and its line.

### Routes and suppression

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/tg.token"
chat_id = -1001234567890

[target.mm]
type = "http"
url_file = "/etc/mailcrier.d/mm.url"
preset = "mattermost"

[[suppress]]
subject = "*Anacron*"

[[route]]
subject = "*raid*"
targets = ["ops-telegram"]
continue = true

[[route]]
recipient_regex = '^backup(@|$)'
targets = ["mm"]

[[route]]                       # no condition: every message
targets = ["mm"]
```

- Without a `[[route]]` every target gets every message, except one whose
  recipients are all direct chats (`<chat>@telegram` with
  `telegram_direct`), which goes to those chats alone. With routes, the
  rules are checked from the first, for each recipient of the envelope on
  its own: the targets of the first rule whose conditions all match are
  taken, and with `continue = true` the check goes on and the targets of
  the next matching rules are added. The targets of all recipients are
  joined: a message to `root` and `backup@example.org` reaches the targets
  of both, even when the rule for `root` comes first without `continue`. A
  rule without conditions matches every message.
- A condition is `subject`, `sender` or `recipient`:
  - `subject` is the subject with encoded words decoded and white space
    collapsed, empty when the message has none;
  - `sender` is the envelope sender, see "sendmail command line": `-f` or
    `-r`, else `Resent-From`, else `From`, else the caller; empty for the
    null sender;
  - `recipient` is one envelope recipient, `Bcc` included, as its address
    without the display name: `Root <root@example.org>` on the command
    line is `root@example.org`. A local name stays without a domain: cron
    mails `root`, which `recipient = "root"` matches and `root@*` does
    not.
- Each condition is a glob, or an RE2 expression in Go syntax under the
  key with `_regex` (`subject_regex`, `sender_regex`, `recipient_regex`);
  a rule takes at most one of the two keys of a field. A glob matches the
  whole value with case ignored, Cyrillic and other scripts included: `*`
  is any run of characters, `/` and line breaks included, `?` one
  character, and `\` makes the next character literal; there are no
  `[...]` classes. An expression matches anywhere in the value unless
  anchored with `^` and `$`, with case as written unless it starts with
  `(?i)`. An empty glob or expression is an error: a field without a
  condition leaves its key out.
- `targets` is required, a list of configured target names without
  repeats.
- A message without recipients, as `-t` without recipient headers gives,
  is checked once: only a rule without `recipient` can match it.
- A recipient that no rule matches, while others have targets, is logged
  as `no route for recipient` (warning) with `unrouted`, the number of such
  recipients, never the addresses. When no rule matches at all and no
  recipient is a direct chat, the message is held in `hold/` with the
  reason `no route`, the log records `no route for message` (warning), and
  the call exits 64. With the spool off the message is lost (`message
  lost, spool off`) with the same status; when its entry cannot be created
  or written it is lost as well (`message lost, not held`), and the call
  exits 73 or 74. A queue run checks a held message against the
  rules of the moment and queues it once they select a target; until then
  it stays, counted in the limits of the spool, and moves to `failed/`
  after `hold_ttl`. Until then every later call of the same user and
  every `-q` read and route it again, before the retries of `queue/` and
  out of the same `drain_budget` or `run_budget`: held messages up to the
  per-user limit of the spool take that time from the retries. The
  warning comes once: a run that finds no route again logs it as debug.
  The targets chosen are logged as `message routed` (debug) with
  `targets`.
- The targets of a queued message are fixed when it is queued: a change
  of the routes affects new messages and those released from `hold/`,
  not the retries.
- A `[[suppress]]` rule takes `subject` and `sender` as above, or their
  `_regex` keys, at least one of them: a rule without conditions would
  drop every message. The rules are checked before the routes and before
  the spool; a message that matches every condition of one of them is
  neither sent nor spooled, the log records `message suppressed` (info)
  with `rule`, the number of the rule from 1, and the call exits 0. A
  message released from `hold/` is checked again, and a suppressed one is
  deleted.
- An error in a rule exits 78 like any configuration error and names the
  rule by its number and line (`route 3: ...`), never its expression. The
  line is that of the `[[route]]` or `[[suppress]]` table; rules written
  as an inline array, `route = [{ ... }, { ... }]`, all point at the line
  of its key, and only the number tells them apart.

Direct chats let a caller name a Telegram chat as a recipient:

```toml
[general]
telegram_direct = "ops-telegram"
telegram_direct_chats = [1234, -1001234567890, "@ops_channel"]
telegram_direct_max = 10        # optional, the default

[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/tg.token"
chat_id = -1001234567890
```

- With `telegram_direct` naming a `telegram` target, a recipient
  `<chat>@telegram`, the domain in any case, gets a copy of that target
  sent to the chat: `1234@telegram` a user or bot chat,
  `-1001234567890@telegram` a group, `@ops_channel@telegram` a public
  chat by its username. The copy has the token, template, limits,
  `on_long`, `long_file` and `disable_notification` of the target, but no
  `message_thread_id`: a topic belongs to the chat of the target, and
  another chat refuses it. Spellings of one chat are one chat: leading
  zeros of an id and the case of a username do not count.
- Only the chats of `telegram_direct_chats` get a copy. Any local user
  can name a recipient, and with the setgid install users cannot read the
  token, so this list is what decides where the bot writes for them. It is
  required with `telegram_direct`, holds integers and `@username` strings
  (5 to 32 letters, digits or `_`) without repeats; a string of digits is
  an error, as are the list and `telegram_direct_max` without
  `telegram_direct` (exit 78). A chat not in the list, `@telegram`, or any
  other local part is an ordinary recipient, routed like the others and
  logged as `direct chat not allowed` or `direct address invalid`
  (warning) with `count`, never the address. Without `telegram_direct`
  these addresses are ordinary recipients without a warning.
- `telegram_direct_max`, a positive integer, bounds the copies of one
  message: the chats past it, counted in the order of the recipients
  after repeats are dropped, get nothing, and the log records
  `direct chats over limit` (warning) with `dropped`. Every copy goes out
  with the one token of the bot, and a delay a service asks for, as with
  a 429, holds back only the copy that got it for the rest of a queue run,
  not the target the copies are made of nor the other copies.
- The routes see only the other recipients. A message to direct chats
  alone goes to them alone, even past a rule without conditions; when
  the routes select the target of `telegram_direct` and a direct
  recipient names the chat of that target, the chat gets one message.
  With a direct chat, recipients without a route only add the
  `no route for recipient` warning.
- A copy is named `<chat>@telegram` with the chat normalized
  (`1234@telegram`, `@ops_channel@telegram`) in the log (`target`), in the
  spool and in `mailq`: the chats come from the configuration, unlike the
  other addresses. A queued copy is retried with the target and the list
  of the moment, and fails with `target removed, message dropped for it`
  once `telegram_direct` or its chat is gone from the file.
- On the command line a negative id reads as an option: recipients go
  after `--`, as in `sendmail -- -1001234567890@telegram`.

### Spool and queue

```toml
[spool]                         # optional; the values are the defaults
dir = "/var/spool/mailcrier"    # "" turns the spool off
drain_budget = "10s"            # queue run after the own message of a call
drain_max_messages = 20
run_budget = "60s"              # queue run of -q
queue_ttl = "168h"              # then queue/ -> failed/
hold_ttl = "168h"               # then hold/ -> failed/
failed_ttl = "720h"             # then deleted
max_queue_messages = 1000
max_queue_bytes = 268435456
max_queue_messages_per_uid = 200
max_queue_bytes_per_uid = 67108864

[target.hook]
type = "http"
url_file = "/etc/mailcrier.d/hook.url"
preset = "generic-json"
```

- Every message is written to the spool before its delivery: the raw
  message as `queue/<id>.eml` and its state per target as
  `queue/<id>.json`, each first written to `tmp/`, synced and moved into
  `queue/`. The state is rewritten after each target, and the entry
  removed once no target waits for it, or moved to `failed/` with the
  reason `internal error` when a target failed by a panic (see "Exit
  status"). A target that fails temporarily
  stays pending, and the call exits 0 with the warning `message queued` (or
  `message queued for target` when another target has the message): the
  message is accepted, as a classic MTA accepts it into its queue.
- A queue run retries a pending target one minute after its first failure,
  then after twice the previous delay, at most 24 hours, and never before
  the delay a service asked for with `Retry-After`; when a service asks for
  a delay, the rest of the run skips that target in the other entries. An
  entry queued longer than `queue_ttl`, counted from its release for a
  message that was held, moves to `failed/` with the error
  `message failed`, and after `failed_ttl` there it is deleted. Files
  older than one hour that a killed process left behind, in `tmp/` unless
  a live process still holds them and sidecars without their message
  elsewhere, are removed. A message file without its sidecar, which
  `mailq` lists as `unreadable`, is the trace of a removal cut short once
  no target waited for the message: the next run that comes to it deletes it, logged
  as `message without sidecar removed` with its id and area.
- An entry whose sidecar cannot be read stays where it is with the
  warning `spool entry unreadable, left alone` on every queue run, which
  then exits 74. A corrupt sidecar has lost the dates of the entry, so
  the message file stands in for them: written with the entry and never
  rewritten, once it is older than `queue_ttl` (`hold_ttl` in `hold/`)
  the entry moves to `failed/` with the reason `corrupt sidecar` and a
  new sidecar without targets, owned by the owner of the file. Owner and
  age are approximate: a message that `-q` copied from `hold/` of the
  default directory belongs to the user of that run and dates from the
  copy, and one released from `hold/` counts from the time it was held,
  not from its release. The message is kept as it was; when its headers
  name the recipients,
  `mailcrier -t < failed/<id>.eml` sends it again. A corrupt sidecar in
  `failed/` goes with its message `failed_ttl` after the file was
  written. An entry of another sidecar version is left alone, for the
  release that wrote it, with its own warning and exit status 0.
- A retry renders the stored message with the configuration and templates
  of the moment, to the targets fixed when it was queued. Renaming or
  removing a target drops its pending messages: they fail for that target
  with `target removed, message dropped for it`.
- A message whose configuration is rejected goes to `hold/` (of the
  default directory when the file cannot be read) and the call exits 78;
  the first queue run with a valid configuration routes it as a new
  message, see "Routes and suppression". When the valid file names a
  `dir` other than the default, `-q` also moves what `hold/` of the
  default directory keeps into that queue. `hold_ttl` bounds the wait:
  while the file is rejected, `-q` moves expired entries to `failed/` and
  exits 78, in the `dir` of the file when it parses and only its targets
  are rejected, as the call held its message there, in the default
  directory otherwise.
  A message to be held, for a rejected configuration or without a route,
  whose entry cannot be created or written (`message lost, not held`)
  exits 73 or 74 instead of 78 or 64, as a temporary failure without a
  spool entry does: the message is not kept. Whether it is tried again is
  up to the caller: cron, PHP `mail()` and most scripts tell no exit status
  from another, while an MTA that runs mailcrier as a pipe transport
  defers only on 75 (Postfix) or on 75 and 73 (Exim) and bounces on the
  others.
- Each call runs the queue for at most `drain_budget` and
  `drain_max_messages` entries after delivering its own message, only for
  the entries of its caller's uid, and not at all while another call of the
  same user is doing so (`locks/drain-<uid>.lock`). `mailcrier -q` runs it
  for at most `run_budget`: root, the `mailcrier` user and a process
  without the setgid bit take every entry, any other user only their own.
  An entry is locked while a process works on it, and a run skips locked
  entries, so parallel runs never deliver an entry twice.
- The packages run `mailcrier -q` as `mailcrier` every 5 minutes: the
  systemd timer `mailcrier-queue.timer` (its service stops a run after 3
  minutes, which covers `run_budget` of 60 seconds and the delivery under
  way; a larger `run_budget` needs a larger `TimeoutStartSec` in a
  drop-in), or where systemd is not running
  `/etc/cron.d/mailcrier`, or `/etc/crontabs/mailcrier` on Alpine, all
  without mail from cron since `-q` logs to syslog only (`MAILTO=` for
  BusyBox crond, which takes `MAILTO=""` for an address). Without a running cron
  daemon or timer, as in most containers, only the calls themselves run the
  queue; a container that sends rarely runs `mailcrier -q` from a
  scheduler such as supercronic, a sidecar or a health check.
- Limits: all entries in `tmp/`, `queue/` and `hold/` together stay within
  `max_queue_messages` and `max_queue_bytes`, those of one uid within the
  `_per_uid` limits; users other than root and `mailcrier` fill at most 80%
  of the totals, so root still gets its mail queued. Parallel calls never
  exceed a limit together, but near it they may all be refused where one
  would have fit. A message over a limit
  is not queued (`spool entry not written`): it is delivered all the same,
  and a temporary failure then exits 73.
- Delivery is at least once: a process killed after a service accepted
  the message, or a timeout after the service accepted it, repeats that
  delivery on the next run, and so may a power failure, since the state
  written after a delivery is not synced to disk before the next step.
  A state that cannot be written at all (`spool entry not updated`, a full
  disk) leaves the entry as it was before the attempt: the rest of that
  call or run skips it, and the next run, in another process, finds it
  due and tries again without the delay of a retry.
- The spool must be on a local file system: the locks (`flock`) do not
  exclude processes over NFS. The directory must exist; the program creates
  `tmp`, `queue`, `hold`, `failed` and `locks` in it when missing, which
  suits a container. With the setgid install the packages create the
  directory and all five as `root:mailcrier 2770`: made by the program,
  they would belong to whichever user sent first. Entries are created with
  mode `0660`.
- `mailq` and `-bp` list the entries of `queue/`, `hold/` and `failed/`
  with the state, attempts, next attempt and last error of each target,
  for root, the `mailcrier` user and a process without the setgid bit;
  anyone else sees one line of counts; a spool directory that does not
  exist lists as `queue is empty`. `--status` prints one logfmt line for
  monitoring,
  `queued=1 held=0 failed=0 tmp=0 bytes=1432 oldest_age_seconds=75`, and
  exits 74 when the directory does not exist, and also, after printing the
  line, when this process cannot write one of its areas or the file
  system is read-only (`spool not writable`). Neither creates anything in
  the spool. `-q` on such a spool exits 74 without a run (`spool not
  writable, queue not run`): it would deliver entries it cannot record
  and deliver them again on the next run.
- Both also count, and `mailq` lists with the field `dir="..."`, what
  `hold/` of the default directory keeps when the file names another
  `dir`, which `-q` moves into that queue; `bytes` and `tmp` stay those
  of the `dir`, and a default directory the caller cannot read gives the
  warning `default spool not listed` and the rest of the output. Under a
  rejected configuration they show the `dir` of the file when it parses
  and only its targets are rejected, since a call holds its message
  there, the default directory otherwise, and log `spool listed under a
  rejected configuration` with the directory.

### Options, privileges and containers

- `--config PATH` or the environment variable `MAILCRIER_CONFIG` names
  another configuration file; `--config` wins. sendmail's `-C` is always
  ignored with a warning.
- `--check-config` checks the configuration, see "Checking the
  configuration": it reads neither stdin nor the spool and sends nothing,
  writes its findings to stderr and exits 0 without an error, 78 with
  one. It takes no arguments (64). root, the `mailcrier` user and a
  caller without the setgid bit may run it.
- `--probe` sends a sample message, subject `mailcrier probe from
  <host>`, to every target of the configuration, or with `--probe --
  name ...` to the targets named (an unknown name exits 64 before
  anything is sent; the names go after `--`, since a name may start with
  `-`). These are real notifications, and `exec` hooks run. Routes,
  `[[suppress]]` rules and direct chats do not apply. It reads no stdin,
  does not touch the spool and does not run the queue. One logfmt line
  per target goes to stdout, such as `target=mm class=temp status=503
  err="..."`; a template that failed and was replaced by the built-in one
  shows `template=fallback` with status 0, and `--check-config` shows the
  template error. The exit status is 0 when every target took the
  message, 69 when one rejected it, and 75 when one failed temporarily
  and none rejected it. root, the `mailcrier` user and a caller without
  the setgid bit may run it. Mail flags such as `-f` or `-t` are ignored,
  and of several mode options the last one wins.
- The intended install is setgid: binary `root:mailcrier 2755`,
  configuration and `*_file` files `root:mailcrier 0640`, so that cron jobs
  of any user can send while only root and the binary read the secrets.
  When a user other than root runs the binary and the kernel applies the
  setgid bit:
  - before doing anything else the process executes itself once more,
    through `/proc/self/exe` or else `/usr/sbin/mailcrier`, with the
    environment reduced to `USER`, `LOGNAME`, `HOME`, `LANG`, `LC_*` and
    `TZ` (a zone name such as `Europe/Berlin` only), so that `GODEBUG`,
    proxy or TLS variables of the caller cannot act with the group
    privilege. If both fail, it replaces its environment the same way and
    goes on with HTTP/2 off, the Go debug output dropped and no proxy, and
    logs `reexec failed` as an error;
  - `--config` and `MAILCRIER_CONFIG` are ignored with a warning; with
    `--check-config` or `--probe` they exit 64 instead, so that a mode
    never works on another file than the one named;
  - `--probe` and `--check-config` exit 77 for everyone but root and the
    `mailcrier` user.
- Without the setgid bit taking effect the binary runs with the ids of its
  caller, and that is the normal mode for containers: Docker with
  `--security-opt no-new-privileges`, Kubernetes with
  `allowPrivilegeEscalation: false`, a systemd unit with
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes`, or a binary copied
  without the bit. The configuration and its `*_file` files must then be
  readable by the caller (for PHP-FPM, for example `0640 root:www-data`),
  `--config` and `MAILCRIER_CONFIG` work, and secrets mounted as files
  (Docker or Kubernetes secrets) go in through `*_file`. For PHP set
  `sendmail_path = /usr/sbin/sendmail -t -i` with `/usr/sbin/sendmail` a
  link to the binary, and pass the variable with
  `env[MAILCRIER_CONFIG] = /run/secrets/mailcrier.conf` in the pool
  configuration. The spool directory, `/var/spool/mailcrier` unless
  `[spool] dir` names another, must exist and be writable by the caller (a
  volume, when queued messages must survive the container); without it a
  temporary failure exits 73, and `dir = ""` turns the spool off.
- Memory of one call, as the peak of its cgroup (Docker, `--cpus=1`, amd64,
  Go 1.27.2): a message of up to 100 KiB takes under 15 MB, and 20 such
  calls at once took 58 MB together. A message of 10 MiB, the most that
  is read, takes up to about 40 MB for an `exec` target, about 100 MB
  with one Telegram target, about 200 MB with 3 and about 420 MB with 10:
  each target with a text limit renders the whole body. Calls at the same
  time add up. A call the kernel kills for memory exits 137 to its
  caller. Killed while the spool entry is written, the message is lost,
  and the files left in `tmp/` are deleted after an hour. Killed after
  that, the entry stays in `queue/`, and every retry renders it again
  until `queue_ttl` moves it to `failed/`.
- On a host with the setgid install, a caller running under
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes` does not get the group:
  neither the configuration nor the spool is accessible to a caller other
  than root, the message is lost (`message lost, not held`) and the call
  exits 73.

### Checking the configuration

`mailcrier --check-config` loads the configuration and builds its targets
as every call does, so it finds every error that would reject a message
with 78, and adds warnings about what loads but may not work as meant.
The errors and warnings go to stderr, one per line, and a last line counts
them; stdout stays empty. A call stops at the first error: after an error
in the file nothing else is reported, after an error of a target (a
template that does not parse, an unknown shoutrrr service) the warnings
about the file still are. The exit status is 78 with an error and 0
otherwise, warnings included. Run it as root after each change of the
configuration: root gets the group of a setgid binary like any other
user, so the checks of permissions below run.

```toml
[target.ops]
type = "http"
url = "https://discord.com/api/webhooks/123/abc/slack"
preset = "slack-webhook"

[target.backup]
type = "discord"
url = "https://discord.com/api/webhooks/456/def"

[[route]]
subject = "*backup*"
targets = ["ops"]
```

<!-- rumdl-disable MD013 -->
```text
warning: /etc/mailcrier.conf:4:1: target "ops": preset "slack-webhook" on a Discord host does not disable mentions, use type "discord"
warning: /etc/mailcrier.conf:6:9: target "backup": no route names this target
warning: /etc/mailcrier.conf:10:3: routes have no rule without conditions: a message no rule matches is held and the call exits 64, or the message is lost with the spool off
/etc/mailcrier.conf: 0 errors, 3 warnings
```
<!-- rumdl-enable MD013 -->

| Warning | Cause | What to do |
|---|---|---|
| `file of key "token_file" is readable by all users` (or `url_file`) | the secret file has the read bit for others | `chmod o-r`, owner group `mailcrier`, mode `0640` |
| `file is readable by all users and holds secrets` | the configuration writes out `token`, `url`, `headers`, `query` or `path` and has the read bit for others | `chmod 0640`, or move the secrets into `*_file` files |
| `file of key "..." is not readable by the group of the binary, every call of another user exits 78` | a `*_file` or `template_file`, or the configuration itself, belongs to the group of the binary without the read bit for the group, or to another group without the read bit for others: the kernel applies the bits of one class only | `chgrp mailcrier` and `chmod g+r` |
| `directory of key "..." is not searchable by the group of the binary, ...` | a directory on the way to the file, `/` included, belongs to the group of the binary without the search bit for the group, or to another group without the search bit for others | `chmod o+x`, or group `mailcrier` with `g+x` |
| `shoutrrr service "telegram" has a native target type "telegram"` (also `slack`, `discord`, `ntfy`) | the shoutrrr target sends plain text without the escaping, length limits and files of the service | use the target type of the service |
| `preset "slack-webhook" on a Discord host does not disable mentions` | the Slack-compatible endpoint of a Discord webhook lets `@everyone` from a message ping the channel | use type `discord` |
| `routes have no rule without conditions` | routes exist, but no rule matches every message | add a last `[[route]]` with `targets` only; a condition such as `subject = "*"` does not count |
| `no route names this target` | routes exist, and neither a route nor `telegram_direct` names the target | add it to a route, or remove it |
| `first element of key "argv" is not an executable file` | the program of an `exec` target is missing or has no execute bit | install it, `chmod +x` |
| `template fails on the sample message, the built-in one is used` | the template from the configuration fails or does not fit the limit of the target | fix the template; calls use the built-in one meanwhile |
| `request template fails on the sample message, the target gets nothing` | a `path`, `query` or `headers` template fails | fix the template; the target gets no message meanwhile |
| `the sample message does not render, the target gets nothing` | the built-in template does not fit `max_text`, for JSON or MarkdownV2 | raise `max_text` |

The four checks of permissions run only when the setgid bit applied to
the process, which is the case for root and for the `mailcrier` user with
another primary group; without the bit (a container, a binary without
the bit, `sudo -u mailcrier` with the primary group `mailcrier`) they are
skipped, because the group that reads the files is unknown. They read the
mode and the owner group only: access control lists are not seen, and for
a directory that is a symbolic link the target directory is checked but
not the directories on the way to it. The check of `argv` runs with the
ids of the caller of `--check-config`, while a hook runs with those of
the caller of each message.

The warnings that end in "sample message" come from rendering a built-in
sample message, the one `--probe` sends, for every target, with nothing
sent and no hook run. The sample has no attachments, no HTML, no long text
and no Bcc, so a template branch for those is not tried; and the URL an
`http` target builds from its rendered `path` is checked only when it
sends.

### sendmail command line

The binary is meant to be installed as `/usr/sbin/sendmail` and understands
the command lines of cron, anacron, at, sudo, mdadm, mailx, apt-listchanges,
unattended-upgrades, fail2ban and similar callers. Flags follow getopt: a
value is glued (`-froot`) or the next argument (`-f root`), flags without a
value group (`-ti`), `--` ends the options, and options and recipients may
come in any order before it. The value of a flag is never read as an
option: `-f --probe` names a sender.

| Flags | Value | Effect |
|---|---|---|
| `-f`, `-r` | required | envelope sender; `''` or `<>` is the null sender |
| `-F` | required | sender's full name |
| `-t` | none | the `To`, `Cc` and `Bcc` headers add recipients, or only the `Resent-To`, `Resent-Cc` and `Resent-Bcc` headers when there are any |
| `-i`, `-oi` | none | a line with a single dot is ordinary text |
| `-o` | required | `-oi` as above, any other `-o` option ignored |
| `-b` | required | by the first letter: `m` delivers (the default), `i` does nothing, `p` lists the queue, `s` is refused with 64, other letters are ignored with a warning |
| `-q` | optional, glued only | runs the queue once; an interval (`-q30m`) is ignored |
| `-I` | none | does nothing, like `-bi` |
| `-C` | required | ignored with a warning; `--config` names another configuration |
| `-B`, `-h`, `-L`, `-N`, `-O`, `-R`, `-V`, `-X`, `-p`, `-A` | required | ignored |
| `-d`, `-e` | glued, or the next argument unless it starts with `-` | ignored |
| `-v`, `-m`, `-n`, `-U`, `-G` | none | ignored |
| any other letter | none | ignored with an `unknown option` warning naming the flag |

- Long options are `--config PATH` (or `--config=PATH`), `--version`,
  `--help`, `--probe`, `--check-config` and `--status`; an unknown one is
  logged by name, without its value, and ignored. A group of unknown
  letters yields one warning, and a command line at most 16 plus one
  `warnings suppressed` record with the count. A flag sendmail knows but
  mailcrier does not never stops the call: refusing it would lose the
  message of a cron job.
- Called as `newaliases` the binary does nothing and exits 0; called as
  `mailq` it lists the queue, see "Spool and queue". `/etc/aliases` and
  `.forward` are not read, and `-bv` and `-bt` are ignored like other
  unsupported `-b` modes; choosing targets by recipient is left to routes in
  the configuration.
- Recipients are the arguments, split at commas
  (`a@example.org, b@example.org` in one argument, or `a@example.org,` and
  `b@example.org` as Debian cron passes them), and with `-t` the addresses
  of the `To`, `Cc` and `Bcc` headers; duplicates are dropped. A message
  with a `Resent-To`, `Resent-Cc` or `Resent-Bcc` header, even an empty
  one, is a resent message: `-t` takes the addresses of these three
  headers instead of `To`, `Cc` and `Bcc`, as Postfix does. `Bcc` and
  `Resent-Bcc` headers are removed from the message and never reach a
  notification. A message without any recipient is delivered to every
  target when there are no routes, see "Routes and suppression".
- The sender is the value of `-f` or `-r` without surrounding angle
  brackets, else the first `Resent-From` address, else the `From` address,
  else `EMAIL`, else `USER` or `LOGNAME` at the host name, else the login
  name of the caller's uid at the host name. A setgid-elevated process
  skips the environment and uses the login name of the uid. When the uid
  has no entry in the user database, as in a container started with a
  numeric user, the sender is empty.
- A line break in `-f`, `-r`, `-F` or a recipient argument exits 64. An
  address of a `From`, `To`, `Cc` or `Bcc` header or of their `Resent-`
  counterparts that holds a control character is dropped with a warning.
- The message: CRLF line ends become LF; a leading mbox envelope line
  (`From` and a space) is dropped; without `-i` a line with a single dot
  ends the message and the first dot of a line starting with two dots is
  removed, as in sendmail 8 (fail2ban passes no `-i`); input whose
  first line is no header field, a continuation line included, is all body;
  a later line with a space before the colon or without a colon starts the
  body and logs a warning. Input beyond 10 MiB is read and discarded with
  a warning. A body without visible text is sent as `(empty body)`, or the
  `empty_body` text of `[strings]`.
- MIME: `Content-Type` and `Content-Transfer-Encoding` are honoured with or
  without `MIME-Version`. The text is the `text/plain` parts, named or not,
  unless `Content-Disposition: attachment` marks them, else the first
  `text/html` part, also one inside `multipart/related`, converted to text
  (scripts, styles and embedded objects dropped, link targets in
  parentheses after the link text, links other than `http`, `https`,
  `mailto` and `ftp` reduced to their text, white space collapsed outside
  `pre`, and several empty lines in a row reduced to one everywhere, `pre`
  included); every other part is an attachment. Multipart nesting deeper
  than 8 levels is kept as one attachment; a broken multipart structure
  keeps the parts before the damage and logs a warning.
- Charsets: encoded words (RFC 2047), quoted-printable and base64 are
  decoded, and text is converted to UTF-8. A body declared `US-ASCII`
  (cron under the C locale says `ANSI_X3.4-1968`) or in an unknown charset
  is taken as UTF-8 when it is valid UTF-8. Text without a charset or
  declared `US-ASCII` that is not valid UTF-8, and text declared
  `ISO-8859-1`, is read as windows-1252, as browsers do: bytes 0x80-0x9F
  are the quotes, dashes and euro sign of that charset. A charset that
  browsers refuse to decode, such as ISO-2022-KR, counts as unknown.
  Encoded words follow the same rules. Bytes that stay invalid become
  U+FFFD. The subject has its white space collapsed.
- The log records the size of the message and the number of recipients,
  never the addresses; only the copy for a direct chat carries the chat in
  its name, see "Routes and suppression".

### Exit status

| Situation | Status |
|---|---|
| at least one target accepted the message; the targets that failed temporarily are queued | 0 |
| a `[[suppress]]` rule matched; the message is neither sent nor spooled | 0 |
| every target failed temporarily and the message is queued | 0 |
| `newaliases`, `-bi`, `-I`, `mailq`, `-bp`, `-q`, `--status`, `--version`, `--help` | 0 |
| `--check-config` found no error, with or without warnings | 0 |
| `--probe`: every target took the sample message, if only with `template=fallback` | 0 |
| no target accepted it and one rejected it, or all failed temporarily with the spool off | 69 |
| `--probe`: a target rejected the sample message | 69 |
| a target failed temporarily, or a message to be held (rejected configuration, no route) was not held, and the spool entry could not be created: directory missing or not writable, or a limit reached | 73 |
| a target failed temporarily, or a message to be held was not held, and the spool entry could not be written; `-q` or `--status` could not read the spool, `mailq` a spool that exists; `-q` or `--status` found it not writable; `mailq` or `--status` could not write their output | 74 |
| `--probe` only: a target failed temporarily and none rejected the sample message; nothing is queued | 75 |
| usage error: `-f` or `-r` without a value, a line break in the sender, the full name or a recipient, `-bs`, `--config` without a value; stdin is not read | 64 |
| `--check-config` with an argument, `--probe` naming no configured target, or either from an elevated caller with `--config` or `MAILCRIER_CONFIG` | 64 |
| no route selects a target; the message is held, or lost with the spool off | 64 |
| stdin cannot be read | 66 |
| panic in the main goroutine (a bug; the record `panic, call ended` carries the value and the stack, redacted) | 70 |
| `--probe` or `--check-config` from an elevated caller other than root and the `mailcrier` user | 77 |
| for a call with a message: the configuration cannot be read, parsed or validated, defines no targets, or holds a template that does not parse; the message is held, or lost with the spool off | 78 |
| `--check-config` found an error, `--probe` cannot use the configuration, or `-q` ran with a rejected configuration and only expired entries; nothing is held | 78 |

The targets are sent to at the same time. A panic while rendering or
sending for one target (a bug) fails that target permanently, logged
redacted as `target failed` with the field `stack`, and the others still
deliver. Its spool entry is not removed: once no target of the message
is pending, or `queue_ttl` expires first, it moves to `failed/` with the
reason `internal error` (`message failed`), where `mailq` shows the
target with `internal="panic: ..."` until `failed_ttl` deletes it; a
retry would repeat the bug. `--check-config` reports such a panic as a
finding and logs its stack as `panic in sample rendering`. A panic on
one entry of a queue run is logged as `panic in queue run` with the id
and the stack, moves that entry to `failed/` the same way, and the run
goes on with the next one; `-q` then exits 70, while the short queue run
after a call leaves the exit status of the call to its own message, as
with any other error of that run.
A panic while the result of one target is recorded in the spool loses no
result: every target is logged and recorded once all finished, the entry
keeps its pending targets, and the call or `-q` exits 70. Any other
panic in the main goroutine is logged as `panic, call ended` and exits
70 without the queue run of the call; a message read and not yet written
to the spool goes to `hold/` with the reason `internal error`, which a
queue run routes like any held message. The stack in these records is
cut to 4 KiB, as syslog daemons cut a long record. A panic in any other
goroutine ends the process with the Go runtime's own report on stderr
and status 2; that report is not redacted. A target that failed is
logged as `target failed`; a temporary failure is also logged as
`message queued for target` or `message queued`, or, without a spool
entry, as `message lost for target` when another target accepted the
message and as `message lost` when every target failed temporarily.
