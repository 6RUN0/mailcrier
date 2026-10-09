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
