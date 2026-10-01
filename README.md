# Slendmail

A sendmail replacement for machines that send no email: cron, at, sudo,
mdadm, smartd, fail2ban and every other tool that pipes a message into
`/usr/sbin/sendmail` get it delivered to Telegram, Discord, Slack, ntfy or
an HTTP webhook instead.

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
go build -o slendmail ./cmd/slendmail
```

The binary goes to `/usr/sbin/slendmail`, with `/usr/sbin/sendmail`,
`/usr/lib/sendmail`, `/usr/sbin/newaliases` and `/usr/bin/mailq` as links
to it; see "Options, privileges and containers" for ownership and modes.
The manual page is `docs/slendmail.8`.

## Configuration

The program reads `/etc/slendmail.conf`, a TOML file. Every run parses the
file strictly: an unknown key, a key that the target type does not use, or
an invalid value rejects the file, the message is not delivered but held
in the spool (see "Spool and queue"), and the exit status is 78. A package
installs an example without targets, so until targets are configured every
message is held with status 78; that is expected.

```toml
[general]
syslog_tag = "slendmail"        # optional, default "slendmail"
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
url_file = "/etc/slendmail.d/hook.url"  # or url = "https://..."
preset = "generic-json"
```

- Exactly one of `url` and `url_file` is set; likewise `token` and
  `token_file`. A `*_file` key takes an absolute path; the file content, with
  surrounding whitespace removed, is used as the value.
- The types `exec` and `shoutrrr` are recognized by the parser but
  rejected with exit status 78 until implemented; the other types are
  described under "Targets".
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
  stderr only `slendmail: <message>` for warnings and errors, without
  fields, because its caller must not see what only the group may read.
  Each record is a constant message with logfmt fields: `call` (16 hex
  digits, one value per invocation) on every record, `msgid` (the
  Message-ID header, cut to 256 bytes, absent when the message has none)
  and `size` (bytes read) for the message, `target`, `class` (`temp` or
  `perm`), `status` (the HTTP status, when the service answered) and
  `retry_after` (the delay the service asked for) for a failed target.
  `text truncated for target` (info) names a target that got a cut text,
  `text rejected, sent as file` (warning) one that refused the text and
  got it as a file, `text rejected, file failed too` (warning) one that
  took neither, `attachments not delivered` (warning) one that took
  the text but not the files; the message counts as delivered there.
  Records about a spool entry carry its `id`. Headers and body of the
  message are never logged, nor is the response body of a service.
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

All targets of the file receive every message, at the same time. Each
sends the text in the markup of its service, escaped so that nothing in the
message becomes markup, a link preview or a mention.

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/slendmail.d/tg.token"   # or token = "123456:ABC..."
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
url_file = "/etc/slendmail.d/discord.url"  # https://discord.com/api/webhooks/<id>/<token>
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
token_file = "/etc/slendmail.d/slack.token"  # bot token, xoxb-...
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
url_file = "/etc/slendmail.d/ntfy.url"
```

- `ntfy`: a JSON publish to the server root with the topic, the subject
  as title (line breaks as spaces, at most 1 KB) and the text of at most
  4096 bytes, then one PUT per attachment to the topic with `filename` and
  `title` in the query; up to 10 files of at most 15 MiB each, the
  defaults of an ntfy server. The URL must end with the topic, otherwise
  exit status 78; user information and a query such as `auth` are sent
  with every request.

```toml
[target.mm]
type = "http"
preset = "mattermost"           # mattermost | slack-webhook | generic-json
url_file = "/etc/slendmail.d/mm.url"
username = "slendmail"          # optional, mattermost only
channel = "alerts"              # optional, mattermost only

[target.api]
type = "http"
preset = "generic-json"
url = "https://api.example.org/notify"

[target.api.headers]            # optional, any preset
Authorization = "Bearer api-token"
```

- `http`: one POST of the JSON document of its preset with
  `Content-Type: application/json` and no files. `preset` is required.
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
  `[target.<name>.headers]` adds request headers after the Content-Type of
  the preset, so a `Content-Type` there replaces it; a header name that is
  not an HTTP token or a value with a line break exits 78. Header values of
  8 characters or more, and the credential after a scheme such as
  `Bearer`, are masked in the log like tokens. A redirect is not followed
  and counts as a permanent failure.

### Long messages

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/slendmail.d/tg.token"
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

### Spool and queue

```toml
[spool]                         # optional; the values are the defaults
dir = "/var/spool/slendmail"    # "" turns the spool off
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
url_file = "/etc/slendmail.d/hook.url"
preset = "generic-json"
```

- Every message is written to the spool before its delivery: the raw
  message as `queue/<id>.eml` and its state per target as
  `queue/<id>.json`, each first written to `tmp/`, synced and moved into
  `queue/`. The state is rewritten after each target, and the entry
  removed once no target waits for it. A target that fails temporarily
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
  elsewhere, are removed.
- A retry renders the stored message with the configuration and templates
  of the moment, to the targets fixed when it was queued. Renaming or
  removing a target drops its pending messages: they fail for that target
  with `target removed, message dropped for it`.
- A message whose configuration is rejected goes to `hold/` (of the
  default directory when the file cannot be read) and the call exits 78;
  the first queue run with a valid configuration sends it to every
  configured target. When the valid file names a `dir` other than the
  default, `-q` also moves what `hold/` of the default directory keeps
  into that queue. `hold_ttl` bounds the wait.
- Each call runs the queue for at most `drain_budget` and
  `drain_max_messages` entries after delivering its own message, only for
  the entries of its caller's uid, and not at all while another call of the
  same user is doing so (`locks/drain-<uid>.lock`). `slendmail -q` runs it
  for at most `run_budget`: root, the `slendmail` user and a process
  without the setgid bit take every entry, any other user only their own.
  An entry is locked while a process works on it, and a run skips locked
  entries, so parallel runs never deliver an entry twice.
- The packages run `slendmail -q` as `slendmail` every 5 minutes: the
  systemd timer `slendmail-queue.timer` (its service stops a run after 3
  minutes, which covers `run_budget` of 60 seconds and the delivery under
  way; a larger `run_budget` needs a larger `TimeoutStartSec` in a
  drop-in), or where systemd is not running
  `/etc/cron.d/slendmail`, or `/etc/crontabs/slendmail` on Alpine, all
  with `MAILTO=""` since `-q` logs to syslog only. Without a running cron
  daemon or timer, as in most containers, only the calls themselves run the
  queue; a container that sends rarely runs `slendmail -q` from a
  scheduler such as supercronic, a sidecar or a health check.
- Limits: all entries in `tmp/`, `queue/` and `hold/` together stay within
  `max_queue_messages` and `max_queue_bytes`, those of one uid within the
  `_per_uid` limits; users other than root and `slendmail` fill at most 80%
  of the totals, so root still gets its mail queued. Parallel calls never
  exceed a limit together, but near it they may all be refused where one
  would have fit. A message over a limit
  is not queued (`spool entry not written`): it is delivered all the same,
  and a temporary failure then exits 73.
- Delivery is at least once: a process killed after a service accepted
  the message, or a timeout after the service accepted it, repeats that
  delivery on the next run, and so may a power failure, since the state
  written after a delivery is not synced to disk before the next step.
- The spool must be on a local file system: the locks (`flock`) do not
  exclude processes over NFS. The directory must exist; the program creates
  `tmp`, `queue`, `hold`, `failed` and `locks` in it when missing, which
  suits a container. With the setgid install the packages create the
  directory and all five as `root:slendmail 2770`: made by the program,
  they would belong to whichever user sent first. Entries are created with
  mode `0660`.
- `mailq` and `-bp` list the entries of `queue/`, `hold/` and `failed/`
  with the state, attempts, next attempt and last error of each target,
  for root, the `slendmail` user and a process without the setgid bit;
  anyone else sees one line of counts; a spool directory that does not
  exist lists as `queue is empty`. `--status` prints one logfmt line for
  monitoring,
  `queued=1 held=0 failed=0 tmp=0 bytes=1432 oldest_age_seconds=75`, and
  exits 74 when the directory does not exist. Neither creates anything in
  the spool.

### Options, privileges and containers

- `--config PATH` or the environment variable `SLENDMAIL_CONFIG` names
  another configuration file; `--config` wins. sendmail's `-C` is always
  ignored with a warning.
- `--probe` and `--check-config` are reserved and exit 64 (not
  implemented).
- The intended install is setgid: binary `root:slendmail 2755`,
  configuration and `*_file` files `root:slendmail 0640`, so that cron jobs
  of any user can send while only root and the binary read the secrets.
  When a user other than root runs the binary and the kernel applies the
  setgid bit:
  - before doing anything else the process executes itself once more,
    through `/proc/self/exe` or else `/usr/sbin/slendmail`, with the
    environment reduced to `USER`, `LOGNAME`, `HOME`, `LANG`, `LC_*` and
    `TZ` (a zone name such as `Europe/Berlin` only), so that `GODEBUG`,
    proxy or TLS variables of the caller cannot act with the group
    privilege. If both fail, it replaces its environment the same way and
    goes on with HTTP/2 off, the Go debug output dropped and no proxy, and
    logs `reexec failed` as an error;
  - `--config` and `SLENDMAIL_CONFIG` are ignored with a warning;
  - `--probe` and `--check-config` exit 77 unless the caller is the
    `slendmail` user.
- Without the setgid bit taking effect the binary runs with the ids of its
  caller, and that is the normal mode for containers: Docker with
  `--security-opt no-new-privileges`, Kubernetes with
  `allowPrivilegeEscalation: false`, a systemd unit with
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes`, or a binary copied
  without the bit. The configuration and its `*_file` files must then be
  readable by the caller (for PHP-FPM, for example `0640 root:www-data`),
  `--config` and `SLENDMAIL_CONFIG` work, and secrets mounted as files
  (Docker or Kubernetes secrets) go in through `*_file`. For PHP set
  `sendmail_path = /usr/sbin/sendmail -t -i` with `/usr/sbin/sendmail` a
  link to the binary, and pass the variable with
  `env[SLENDMAIL_CONFIG] = /run/secrets/slendmail.conf` in the pool
  configuration. The spool directory, `/var/spool/slendmail` unless
  `[spool] dir` names another, must exist and be writable by the caller (a
  volume, when queued messages must survive the container); without it a
  temporary failure exits 73, and `dir = ""` turns the spool off.
- On a host with the setgid install, a caller running under
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes` does not get the group:
  the configuration is unreadable and the call exits 78.

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
  slendmail does not never stops the call: refusing it would lose the
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
  target.
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
  never the addresses.

### Exit status

| Situation | Status |
|---|---|
| at least one target accepted the message, or a rule suppressed it for every target; the targets that failed temporarily are queued | 0 |
| every target failed temporarily and the message is queued | 0 |
| `newaliases`, `-bi`, `-I`, `mailq`, `-bp`, `-q`, `--status`, `--version`, `--help` | 0 |
| no target accepted it and one rejected it, or all failed temporarily with the spool off | 69 |
| a target failed temporarily and the spool entry could not be created: directory missing or not writable, or a limit reached | 73 |
| a target failed temporarily and the spool entry could not be written; `-q` or `--status` could not read the spool, `mailq` a spool that exists | 74 |
| usage error: `-f` or `-r` without a value, a line break in the sender, the full name or a recipient, `-bs`, `--config` without a value; stdin is not read | 64 |
| `--probe`, `--check-config`: not implemented | 64 |
| stdin cannot be read | 66 |
| panic in the main goroutine (a bug; the redacted record in the log carries the details) | 70 |
| `--probe` or `--check-config` from an elevated caller other than root and the `slendmail` user | 77 |
| the configuration cannot be read, parsed or validated, or defines no targets; the message is held | 78 |

The targets are sent to at the same time. A panic while rendering or
sending for one target (a bug) fails that target permanently, logged
redacted as `target failed`, and the others still deliver. A panic in any
other goroutine ends the process with the Go runtime's own report on
stderr and status 2; that report is not redacted. A target that failed is
logged as `target failed`; a temporary failure is also logged as
`message queued for target` or `message queued`, or, without a spool
entry, as `message lost for target` when another target accepted the
message and as `message lost` when every target failed temporarily.
