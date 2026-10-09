# Checking the configuration

Two commands check a configuration before a real message depends on it:
`--check-config` reads it without sending anything, `--probe` sends a
test message through the targets. Where the records of a real message
go is under [Logging](logging.md).

## Checking the file

`--check-config` reads neither stdin nor the spool and sends nothing,
writes its findings to stderr and exits 0 without an error, 78 with
one. It takes no arguments (64). root, the `mailcrier` user and a
caller without the setgid bit may run it.

`mailcrier --check-config` loads the configuration and builds its targets
as every call does, so it finds every error that would reject a message
with 78, and adds warnings about what loads but may not work as meant.
The errors and warnings go to stderr, one per line, and a last line counts
them; stdout stays empty. A call stops at the first error: after an error
in the file nothing else is reported, after an error of a target (a
template that does not parse, an unknown shoutrrr service, an ntfy URL
without topic) the warnings about the file still are. An error of a
target is at the line of its key; a template from `template_file` is
named by the path of that file, and the line after it is a line of that
file. The exit status is 78 with an error and 0
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

[target.phone]
type = "ntfy"
url = "https://ntfy.example.org/"

[[route]]
subject = "*backup*"
targets = ["ops", "phone"]
```

<!-- rumdl-disable MD013 -->
```text
error: /etc/mailcrier.conf:12:1: target "phone": URL has no topic
warning: /etc/mailcrier.conf:4:1: target "ops": preset "slack-webhook" on a Discord host does not disable mentions, use type "discord"
warning: /etc/mailcrier.conf:6:9: target "backup": no route names this target
warning: /etc/mailcrier.conf:14:3: routes have no rule without conditions: a message no rule matches is held and the call exits 64, or the message is lost with the spool off
/etc/mailcrier.conf: 1 error, 3 warnings
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

## Sending a probe

`--probe` sends a sample message, subject `mailcrier probe from <host>`,
to every target of the configuration, or with `--probe -- name ...`
to the targets named (an unknown name exits 64 before anything is sent;
the names go after `--`, since a name may start with `-`). These are real
notifications, and `exec` hooks run. Routes, `[[suppress]]` rules and
direct chats do not apply. It reads no stdin, does not touch the spool and
does not run the queue. One logfmt line per target goes to stdout, such as
`target=mm class=temp status=503 err="Service Unavailable"`; a template that
failed and was replaced by the built-in one shows `template=fallback` with
status 0, and `--check-config` shows the template error. A configuration
it cannot use goes to stderr as `mailcrier: <error>`, and its records in
the log carry `mode=probe`. The exit status is 0 when every target took
the message, 69 when one rejected it, and 75 when one failed temporarily
and none rejected it. root, the `mailcrier` user and a caller without
the setgid bit may run it. Mail flags such as `-f` or `-t` are ignored,
and of several mode options the last one wins.
