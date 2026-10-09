# Telegram

The `telegram` type sends through a bot of the Telegram Bot API. Keys every
target takes and how a failure is classified: [Targets](../targets.md);
a text over the limit: [Long messages](../long-messages.md).

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/telegram.token"  # or token = "123456:ABC..."
chat_id = -1001234567890                        # or "-1001234567890", "@channel"
message_thread_id = 42                          # optional, forum topic
disable_notification = true                     # optional, silent messages
```

`chat_id` (an integer or a non-blank string) is required. A message goes
by `sendMessage` with `parse_mode` HTML and link previews off, then one
`sendDocument` per attachment; a text of at most 1024 characters goes as
the caption of the first document instead, unless that file is empty. The
text holds at most 4096 characters as Telegram counts them, after entities;
a character outside the BMP, such as an emoji, counts once. Up to 10 files
of at most 50 MB each are sent, the others are listed with the `not_sent`
notice. An answer with `"ok": false` is a failure even with status 200;
`error_code` 429 and 5xx are temporary, with `parameters.retry_after`
logged as `retry_after`. When the text arrived and a document did not,
the message counts as delivered and `attachments not delivered` is logged.
