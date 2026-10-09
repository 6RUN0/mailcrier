# Configuration

What every run reads, how the file is checked, and the settings of
`[general]` and `[strings]`. The keys of each destination are under
[Targets](targets.md), the queue under [Spool and queue](spool.md), the
log under [Logging](logging.md).

The program reads `/etc/mailcrier.conf`, a TOML file. Every run parses the
file strictly: an unknown key, a key that the target type does not use,
or an invalid value rejects the file, the message is not delivered but
held in the spool (see [Spool and queue](spool.md)), and the exit status
is 78. A package installs an example without targets, so until targets
are configured every message is held with status 78; that is expected.

## Examples

Example configurations to copy whole are installed in
`/usr/share/doc/mailcrier/examples/` and kept in
[`packaging/examples/`](../packaging/examples/):

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

## General settings and notices

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

[target.api]                    # the table key is the target name: [a-z0-9-]
type = "http"
url_file = "/etc/mailcrier.d/api.url"   # or url = "https://..."
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

## Timeouts and proxy

- `http_timeout` bounds one HTTP request from dialing to the end of the
  response; `deadline` bounds the delivery to all targets together. Both
  take Go duration strings (`"500ms"`, `"15s"`, `"1m"`) and must be
  positive. A target that runs out of either counts as a temporary
  failure whose error names the bound, such as `Post: http_timeout 15s
  exceeded` or `Post: deadline 30s exceeded`; `run_budget`,
  `drain_budget` and the `timeout` of a hook show the same way, a stop
  signal as `terminated signal received`. A timeout or a connection closed
  while the answer of Telegram or Slack is read is temporary as well
  (`answer not read`), whatever the status.
- Proxy environment variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`)
  are honoured without elevation and ignored by a setgid-elevated process.
- The file mode creation mask is always `007`.
