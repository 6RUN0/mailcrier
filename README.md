# Slendmail

A portmanteau of slack and sendmail

## sendmail meets modern notifications (nee Slack)

A cron compatible sendmail alternative that sends messages to multiple notification backends instead of email.

Since most people can't actually send email from their computers these days, send the messages via Slack, Mattermost, Discord, or any webhook-compatible service instead.

This is especially useful for homelabbers that can't/don't want to setup something like SES or run their own MTA that can actually send somewhere useful.

## Features

- **Multiple Backends**: Support for Slack, webhooks (Mattermost, Discord, etc.)
- **Flexible Configuration**: TOML-based configuration with support for multiple instances of each backend
- **Webhook Formats**: Built-in support for Mattermost, Slack-compatible, and generic webhook formats
- **Cron Compatible**: Drop-in replacement for sendmail in cron jobs
- **Reliable**: Continues sending to other backends even if one fails
- **Logging**: Comprehensive syslog integration for monitoring and debugging

## Supported Backends

### Slack
- Native Slack API integration
- Rich message formatting with subject and hostname
- Support for multiple Slack workspaces/channels

### Webhook (Generic)
- **Mattermost**: Full compatibility with Mattermost incoming webhooks
- **Discord**: Slack-compatible webhook format
- **Custom APIs**: Generic JSON format for custom integrations
- **Flexible Headers**: Custom HTTP headers for authentication

### Future Backends
The architecture is designed to easily support additional backends like Matrix, MQTT, Pushbullet, IRC, Signal, etc.

## Configuration (cmd/slendmail)

The rewrite under `cmd/slendmail` (`go build -o slendmail ./cmd/slendmail`) reads
`/etc/slendmail.conf` in a new schema; the legacy format below is not accepted.
Every run parses the file strictly: an unknown key, a key that the target type
does not use, or an invalid value rejects the file, the message is not delivered
and the exit status is 78.

```toml
[general]
syslog_tag = "slendmail"        # optional, default "slendmail"
http_timeout = "15s"            # optional, one HTTP request
deadline = "30s"                # optional, delivery to all targets

[strings]                       # optional, notices in place of missing content
no_subject = "(no subject)"
empty_body = "(empty body)"
truncated = "[truncated]"
more_attachments = "... and %d more"

[target.hook]                   # the table key is the target name: [a-z0-9-]
type = "http"
url_file = "/etc/slendmail.d/hook.url"  # or url = "https://..."
preset = "generic-json"
```

- Exactly one of `url` and `url_file` is set; likewise `token` and
  `token_file`. A `*_file` key takes an absolute path; the file content, with
  surrounding whitespace removed, is used as the value.
- The `http` target with `preset = "generic-json"` posts
  `{"subject": ..., "body": ..., "hostname": ...}` with
  `Content-Type: application/json`. Other target types and presets are
  recognized by the parser but rejected with exit status 78 until implemented.
- `[strings]` replaces the English notices that stand in for missing
  content: `no_subject` for a message without a subject, `empty_body` for
  a body without visible text, `truncated` at the end of a body cut to the
  length limit of a target, `more_attachments` after a list of
  attachments cut to that limit, with `%d` for the number left out (exactly
  one `%d` and no other `%`, else exit status 78). Each is optional and
  must not be blank; the values above are the defaults. The `http` target
  applies only `empty_body`: its payload carries the subject as it is,
  empty when the message has none, and it has no length limit. The others
  take effect in the built-in templates of the native targets.
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
  `perm`) and `status` (the HTTP status, when the service answered) for a
  failed target. Headers and body of the message are never logged, nor is
  the response body of a service.
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

### Options, privileges and containers

- `--config PATH` or the environment variable `SLENDMAIL_CONFIG` names
  another configuration file; `--config` wins. sendmail's `-C` is always
  ignored with a warning.
- `--probe`, `--check-config` and `--status` are reserved and exit 64
  (not implemented).
- The intended install is setgid: binary `root:slendmail 2755`,
  configuration and `*_file` files `root:slendmail 0640`, so that cron jobs
  of any user can send while only root and the binary read the secrets.
  When a user other than root runs the binary and the kernel applies the
  setgid bit:
  - before doing anything else the process executes itself once more, through `/proc/self/exe` or
    else `/usr/sbin/slendmail`, with the environment reduced to `USER`,
    `LOGNAME`, `HOME`, `LANG`, `LC_*` and `TZ` (a zone name such as
    `Europe/Berlin` only), so that `GODEBUG`, proxy or TLS variables of the
    caller cannot act with the group privilege. If both fail, it replaces
    its environment the same way and goes on with HTTP/2 off, the Go debug
    output dropped and no proxy, and logs `reexec failed` as an error;
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
  configuration.
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
| `-t` | none | the `To`, `Cc` and `Bcc` headers add recipients |
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
  `mailq` it lists the queue. No message is queued: `mailq` and `-bp` print
  `queue is empty`, and `-q` exits 0 at once. `/etc/aliases` and
  `.forward` are not read, and `-bv` and `-bt` are ignored like other
  unsupported `-b` modes; choosing targets by recipient is left to routes in
  the configuration.
- Recipients are the arguments, split at commas
  (`a@example.org, b@example.org` in one argument, or `a@example.org,` and
  `b@example.org` as Debian cron passes them), and with `-t` the addresses
  of the `To`, `Cc` and `Bcc` headers; duplicates are dropped. `Bcc`
  headers are removed from the message and never reach a notification. A
  message without any recipient is delivered to every target.
- The sender is the value of `-f` or `-r` without surrounding angle
  brackets, else the `From` address, else `EMAIL`, else `USER` or `LOGNAME`
  at the host name, else the login name of the caller's uid at the host
  name. A setgid-elevated process skips the environment and uses the login
  name of the uid. When the uid has no entry in the user database, as in a
  container started with a numeric user, the sender is empty.
- A line break in `-f`, `-r`, `-F` or a recipient argument exits 64. An
  address of a `From`, `To`, `Cc` or `Bcc` header that holds a control
  character is dropped with a warning.
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
| at least one target accepted the message, or a rule suppressed it for every target | 0 |
| `newaliases`, `-bi`, `-I`, `mailq`, `-bp`, `-q`, `--version`, `--help` | 0 |
| no target accepted it: rejected, or a temporary failure with no queue to keep it | 69 |
| usage error: `-f` or `-r` without a value, a line break in the sender, the full name or a recipient, `-bs`, `--config` without a value; stdin is not read | 64 |
| `--probe`, `--check-config`, `--status`: not implemented | 64 |
| stdin cannot be read | 66 |
| panic in the main goroutine (a bug; the redacted record in the log carries the details) | 70 |
| `--probe` or `--check-config` from an elevated caller other than root and the `slendmail` user | 77 |
| the configuration cannot be read, parsed or validated, or defines no targets | 78 |

A panic in any other goroutine ends the process with the Go runtime's own
report on stderr and status 2; that report is not redacted. A target that
failed is logged as `target failed`; without a queue a temporary failure is
also logged as `message lost for target` when another target accepted the
message, and as `message lost` when every target failed temporarily.

The sections below describe the legacy `main.go` in the repository root.

## Configuration

Configuration file should be placed at `/etc/slendmail.conf` in TOML format.

### Basic Example

```toml
# Optional syslog tag (defaults to "slendmail")
syslog_tag = "slendmail"

[backends]

# Slack backend
[[backends.slack]]
name = "alerts"
token = "xoxb-your-slack-bot-token-here"
channel = "#alerts"

# Mattermost webhook
[[backends.webhook]]
name = "mattermost"
url = "https://your-mattermost.com/hooks/xxx-generatedkey-xxx"
format = "mattermost"
username = "slendmail"
```

### Complete Example

```toml
syslog_tag = "slendmail"

[backends]

# Multiple Slack configurations
[[backends.slack]]
name = "production-alerts"
token = "xoxb-prod-token"
channel = "#alerts"

[[backends.slack]]
name = "dev-notifications"
token = "xoxb-dev-token"
channel = "#dev-alerts"

# Mattermost webhook
[[backends.webhook]]
name = "mattermost-alerts"
url = "https://mattermost.example.com/hooks/xxx-key-xxx"
format = "mattermost"
username = "slendmail"
channel = "alerts"

# Discord webhook (using Slack compatibility)
[[backends.webhook]]
name = "discord"
url = "https://discord.com/api/webhooks/123/token/slack"
format = "slack"
username = "Server Bot"

# Custom API with authentication
[[backends.webhook]]
name = "custom-api"
url = "https://api.example.com/notifications"
format = "generic"
[backends.webhook.headers]
"Authorization" = "Bearer your-api-token"
"Content-Type" = "application/json"
```

## Backend Configuration

### Slack Backend

```toml
[[backends.slack]]
name = "optional-name"          # Optional: friendly name for logging
token = "xoxb-your-bot-token"   # Required: Slack bot token
channel = "#channel-name"       # Required: Target channel
```

### Webhook Backend

```toml
[[backends.webhook]]
name = "optional-name"          # Optional: friendly name for logging
url = "https://webhook.url"     # Required: webhook endpoint URL
format = "mattermost"           # Required: "mattermost", "slack", or "generic"
username = "bot-name"           # Optional: override webhook username
channel = "#channel"            # Optional: override webhook channel
[backends.webhook.headers]      # Optional: custom HTTP headers
"Authorization" = "Bearer token"
```

#### Webhook Formats

- **`mattermost`**: Mattermost-compatible format with Markdown formatting
- **`slack`**: Slack-compatible format (works with Discord webhooks ending in `/slack`)
- **`generic`**: Simple JSON with separate `subject`, `body`, and `hostname` fields

## Installation & Setup

1. **Build the binary**:
   ```bash
   go build -o slendmail main.go
   ```

2. **Install system-wide**:
   ```bash
   sudo cp slendmail /usr/local/bin/
   sudo chmod +x /usr/local/bin/slendmail
   ```

3. **Create configuration**:
   ```bash
   sudo cp slendmail.conf.example /etc/slendmail.conf
   sudo chmod 600 /etc/slendmail.conf  # Protect tokens
   ```

4. **Configure cron**:

   **Alpine/BusyBox**: Add to `/etc/crontabs/root`:
   ```bash
   MAILTO=""
   SENDMAIL="/usr/local/bin/slendmail"
   ```

   **Ubuntu/Debian**: Add to `/etc/crontab`:
   ```bash
   MAILTO=""
   SENDMAIL="/usr/local/bin/slendmail"
   ```

## Usage

Slendmail is designed as a drop-in replacement for sendmail. It reads email messages from stdin and parses the subject and body to send via configured backends.

### Direct Usage
```bash
echo -e "Subject: Test Message\n\nThis is a test" | ./slendmail
```

### Cron Integration
Once configured as the system sendmail alternative, any cron job output will automatically be sent through slendmail:

```bash
# This cron job's output will be sent via slendmail
0 2 * * * /path/to/backup-script.sh
```

## Message Format

Slendmail parses standard email format from stdin:

```
Subject: Your Subject Here

Message body goes here.
Multiple lines are supported.
```

The parsed message includes:
- **Subject**: Extracted from email headers
- **Body**: Email message content
- **Hostname**: Automatically detected system hostname

## Logging

Slendmail uses syslog for comprehensive logging:

- **Debug**: Successful backend operations and message content
- **Error**: Backend failures and configuration issues
- **Info**: General operational messages

View logs with:
```bash
# System logs
journalctl -t slendmail

# Traditional syslog
tail -f /var/log/messages | grep slendmail
```

## Error Handling

- **Partial Failures**: If some backends fail, slendmail continues with successful backends
- **Complete Failure**: Only exits with error if ALL configured backends fail
- **Logging**: All failures are logged to syslog with detailed error messages

## Backend Development

Adding new backends is straightforward. Implement the `Backend` interface:

```go
type Backend interface {
    Send(msg *EmailMessage) error
    Name() string
}
```

See the existing `SlackBackend` and `WebhookBackend` implementations as examples.

## TODO

- Look for config in more/configurable places
- More documentation on how to setup the Slack app
- Better compatibility with different cron daemons and other utils that call sendmail directly
- Matrix backend
- MQTT backend
- Pushbullet backend
- IRC backend
- Signal backend
- Message templates/formatting options
- Retry functionality (not really sure how this could work since it's not a running daemon, maybe write out to a file or something)
- Configuration validation

## Compatibility Notes

- **Alpine/BusyBox cron**: Calls sendmail with `sendmail -ti`
  - `-i`: ignore dots alone (finish processing at end of input)
  - `-t`: read headers for to/cc/bcc (parsed but not used)
- **Standard cron**: Compatible with most cron implementations

## Security Considerations

- Store the configuration file with restricted permissions when possible (`chmod 600`)
- Webhook URLs should be treated as secrets
