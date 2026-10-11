# Targets

A target is one `[target.<name>]` table of the
[configuration](configuration.md): one destination of the messages. This
page holds what every type shares; each type has its own page:

| Type | Sends to |
|---|---|
| [`telegram`](targets/telegram.md) | a chat, group or channel through a Telegram bot |
| [`discord`](targets/discord.md) | a Discord channel through its webhook |
| [`slack`](targets/slack.md) | a Slack channel with a bot token |
| [`ntfy`](targets/ntfy.md) | a topic of an ntfy server |
| [`http`](targets/http.md) | Mattermost, a Slack incoming webhook, a JSON API or a GET ping |
| [`exec`](targets/exec.md) | a program of your own, the message on stdin |
| [`shoutrrr`](targets/shoutrrr.md) | a service of the shoutrrr library: Gotify, Matrix, Teams, Pushover, SMTP and others |

Every target of the file receives every message unless routes or direct
chats choose the targets, see [Routes and suppression](routing.md); the
targets of a message are sent to at the same time. Each sends the text in
the markup of its service, escaped so that nothing in the message becomes
markup, a link preview or a mention; `shoutrrr` sends plain text, and
`exec` hands on the message itself instead.

`max_file_size`, a key of every type but `http`, `exec` and `shoutrrr`,
sets the size limit of one file in bytes, in place of the one given on the
page of the type (`slack` has none); it must be a positive integer, else
exit status 78. A file over it is listed with the `not_sent` notice and not
sent, and a cut text whose full text exceeds it ends with the
`truncated_size` notice. A value above the limit of the service is taken as
it is, and the service rejects a bigger file.

## Limits

The limits of each service, as the page of its type gives them; `max_text`
and `max_file_size` replace them per target, see
[Long messages](long-messages.md).

| Type | Text | Files |
|---|---|---|
| `telegram` | 4096 characters | 10, at most 50 MB each |
| `discord` | 2000 UTF-16 units | 10, at most 20 MiB each and 25 MiB together |
| `slack` | 40000 UTF-16 units | 10, no size limit |
| `ntfy` | 4096 bytes | 10, at most 15 MiB each (ntfy.sh: 2 MB) |
| `http` | `slack-webhook` 40000 UTF-16 units, `mattermost` and `generic-json` none | none |
| `exec` | the message itself on stdin | none |
| `shoutrrr` | none of mailcrier's, the service cuts | none |

## Failures

Each target fails on its own; the others still deliver. Status 429, 5xx
and any answer with a `Retry-After` header are temporary failures, which
the spool retries; any other failure is permanent and is not retried.
A timeout of `http_timeout` or `deadline` is temporary as well, see
[Timeouts and proxy](configuration.md#timeouts-and-proxy); the rules of a
type that differ from these are on its page.

Russian: [targets.ru.md](targets.ru.md).
