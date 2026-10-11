# Slack

The `slack` type posts with a bot token of the Slack Web API. Keys every
target takes and how a failure is classified: [Targets](../targets.md);
a text over the limit: [Long messages](../long-messages.md).

```toml
[target.prod-slack]
type = "slack"
token_file = "/etc/mailcrier.d/slack.token"  # bot token, xoxb-...
channel = "#alerts"                          # channel name or ID
```

`channel` is required and must not be blank; white space around
it is dropped. A message goes by `chat.postMessage` of the Web API
with the bot token as a bearer token and link unfurling off, then
per attachment `files.getUploadURLExternal` and an upload, and one
`files.completeUploadExternal` that shares the files in the thread of
the message; the bot needs `chat:write` and `files:write` and must be
a member of a private channel. `&`, `<` and `>` of the message are
escaped, so `<!channel>` or `<@U123>` stays text. The text holds at
most 40000 characters; up to 10 files are sent. An answer with `"ok":
false` is a failure; `ratelimited`, `internal_error`, `fatal_error`,
`service_unavailable` and `request_timeout` are temporary. Several `slack`
targets may post with different tokens to different channels.

Russian: [slack.ru.md](slack.ru.md).
