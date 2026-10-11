# Command line

The command line mailcrier takes as `sendmail`, the message it reads from
stdin, and its exit statuses. `--check-config` and `--probe` are under
[Checking the configuration](checking.md), `-q` and `mailq` under
[Spool and queue](spool.md).

## Configuration file

`--config PATH` or the environment variable `MAILCRIER_CONFIG` names
another configuration file; `--config` wins. sendmail's `-C` is always
ignored with a warning.

## Flags

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
- Called as `newaliases` the binary does nothing and exits
  0; called as `mailq` it lists the queue, see [Spool and
  queue](spool.md#listing-the-queue). `/etc/aliases` and `.forward` are not
  read, and `-bv` and `-bt` are ignored like other unsupported `-b` modes;
  choosing targets by recipient is left to routes in the configuration.

## Recipients and sender

- Recipients are the arguments, split at commas (`a@example.org,
  b@example.org` in one argument, or `a@example.org,` and `b@example.org`
  as Debian cron passes them), and with `-t` the addresses of the `To`,
  `Cc` and `Bcc` headers; duplicates are dropped. A message with a
  `Resent-To`, `Resent-Cc` or `Resent-Bcc` header, even an empty one,
  is a resent message: `-t` takes the addresses of these three headers
  instead of `To`, `Cc` and `Bcc`, as Postfix does. `Bcc` and `Resent-Bcc`
  headers are removed from the message and never reach a notification. A
  message without any recipient is delivered to every target when there
  are no routes, see [Routes and suppression](routing.md).
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

## Message

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
  never the addresses; only the copy for a direct chat carries the chat
  in its name, see [Direct chats](routing.md#direct-chats).

## Exit status

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
| panic of the call, or on an entry of a `-q` run (a bug; the record `panic, call ended` or `panic in queue run` carries the value and the stack, redacted) | 70 |
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
keeps its pending targets, and the call or `-q` exits 70. When the
second recording panics too (`panic in recording a result`), the state
cannot be written, and the entry moves to `failed/` with the reason
`internal error` at once, so that no target gets the message twice. Any
other panic in the main goroutine is logged as `panic, call ended` and
exits 70 without the queue run of the call; a message read and not yet
written to the spool goes to `hold/` with the reason `internal error`,
which a queue run routes like any held message. The stack in these
records is cut to 4 KiB, as syslog daemons cut a long record. A panic in
any other goroutine ends the process with the Go runtime's own report on
stderr and status 2; that report is not redacted. A target that failed
is logged as `target failed`, or `target failed, retry queued` when the
spool retries it; a temporary failure is also logged as `message queued
for target` or `message queued`, or, without a spool entry, as `message
lost for target` when another target accepted the message and as
`message lost` when every target failed temporarily.

Russian: [cli.ru.md](cli.ru.md).
