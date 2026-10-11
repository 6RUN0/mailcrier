# Logging

Where mailcrier logs, the fields and texts of its records, and what it
masks in them. Reading the log to find a message that did not arrive:
[Checking the configuration](checking.md).

## Finding a message

A caller such as cron shows no error of mailcrier, so the log is where a
message that did not arrive is traced. The records carry the tag of
`syslog_tag`, `mailcrier` by default, and debug records are written as
well:

```sh
journalctl -t mailcrier --since today      # systemd with journald
grep mailcrier /var/log/mail.log           # Debian, Ubuntu with rsyslog
grep mailcrier /var/log/maillog            # RHEL, Rocky with rsyslog
```

The records of one call share the field `call`. A delivered message gives
`message received` and one `target delivered` per target; `message routed`
(debug) lists the `targets` the routes chose, `message suppressed` names
the `[[suppress]]` rule that dropped it, `message held` the `reason` it
waits in `hold/`, and `target failed` the `err` of a target. `mailq` lists
what the spool still keeps, see
[Spool and queue](spool.md#listing-the-queue).

## Where the records go

Logging goes to syslog, facility `mail`, under `syslog_tag`; the records
of a call before its file is loaded (a usage error, a file that does not
load) go under `mailcrier`. Where no syslog socket exists (a container
without `/dev/log`) the records go to stderr, with time in UTC and level,
after one `syslog unavailable` warning that names the socket and the error;
a daemon that stops taking records, or leaves a record without room for 1
s because it stopped reading `/dev/log`, sends the record that failed and
every later one of the process there too, after one `syslog write failed`
warning (`err="... i/o timeout"`), so that the call never waits on the log
longer than that second. PHP-FPM passes the stderr of a worker to its own
log only with `catch_workers_output = yes` in the pool configuration. A
setgid-elevated process without syslog writes to stderr only `mailcrier:
<message>` for warnings and errors, without fields, because its caller must
not see what only the group may read. On stderr the records come before
the report of `--check-config` and before the line of a usage error,
so the last line is the one for the user.

## Records and fields

Each record is a constant message with logfmt fields: `call` (16 hex digits,
one value per invocation) on every record, `msgid` (the Message-ID header,
cut to 256 bytes, absent when the message has none) and `size` (bytes read)
for the message, `target`, `class` (`temp` or `perm`), `status` (the HTTP
status, when the service answered), `retry_after` (the delay the service
asked for) and `err` (the cause, without class and status) for a failed
target: `target failed, retry queued` (warning) when the spool retries
a temporary failure, `target failed` (error) for a permanent one and for
a temporary one nothing retries, `--probe` included. A number of things
is `count`, a list of target names `targets`, a spool directory `dir`,
the operation `mode` (`run`, `drain`, `mailq`, `status`, `probe`). `hook
output` (info, warning when the run failed) carries what an `exec` hook
printed, see [Exec hooks](targets/exec.md). `text truncated for target`
(info) names a target that got a cut text, `template output limit reached`
(warning) one whose template from the configuration wrote more than 1 MiB,
so that the cut is the template's, `text rejected, sent as file` (warning)
one that refused the text and got it as a file, `text rejected, file
failed too` (warning) one that took neither, `attachments not delivered`
(warning) one that took the text but not the files; the message counts as
delivered there. `template failed, built-in used` (warning) names a target
whose template from the configuration failed, `request template failed,
message not sent` (warning) an `http` target whose path, query or header
template failed, see [Templates](templates.md). Records about a spool entry
carry its `id`, the name `mailq` lists, from the moment the message is
written to the spool: those of the call and its targets and `hook output`
too; the queue run after a call does not carry the `msgid` of the message
of the call. `message held`, `message lost` and `message failed` carry
`reason`, `message failed` also `area` (`queue` or `hold`, the area
of the spool the entry left) and `targets` (those that still waited),
`message lost` and `message not delivered` the `targets` concerned. A
lost message gets `message lost` alone, with `err` when the spool failed,
such as `reason="spool not opened" err="stat /var/spool/mailcrier: no
such file or directory"`; a message held after `configuration rejected`
gets `message held`, and goes out once the file loads. Headers and body
of the message are never logged, nor is the response body of a service.

## Masked secrets

Tokens and URLs, including the content of `*_file`, are replaced by `***`
in every log record and in the debug output of the Go HTTP stack. A URL
is masked whole, and so are its host, host labels, request URI, path
segments and query values of 16 characters or more (the request URI also
when it has a query) and its userinfo. The literal text of the query,
path and header templates of an `http` target is masked as well, but for
a few headers that carry no secret:
[HTTP and Mattermost](targets/http.md).

Russian: [logging.ru.md](logging.ru.md).
