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

## Bot and chat id

The target needs the token of a bot and the id of a chat the bot may
write to. The steps follow the Telegram Bot API documentation
(<https://core.telegram.org/bots/api#getupdates>); `curl` runs on any host
with access to `api.telegram.org`.

1. In Telegram, send `/newbot` to `@BotFather` and answer its questions.
   It replies with the token, such as `123456:ABC-...`, which goes into
   `/etc/mailcrier.d/telegram.token` as in the
   [quick start](../../README.md#quick-start).
2. Let the bot into the chat:
   - a private chat: open the bot and send it `/start`; a bot cannot
     write first (`bot can't initiate conversation with a user`);
   - a group: add the bot as a member, then send `/start@<bot name>` in
     the group, which reaches the bot even with its privacy mode on;
   - a channel: add the bot as an administrator that may post, then post
     a message in the channel. A public channel also takes its username,
     `chat_id = "@channel"`, without this step.
3. Read the updates the bot received; the token stays out of the shell
   history:

   ```sh
   curl -s "https://api.telegram.org/bot$(cat /etc/mailcrier.d/telegram.token)/getUpdates"
   ```

   The id is `"chat":{"id":...}` of the message from step 2: a positive
   number for a private chat, a negative one for a group, `-100...` for a
   supergroup or a channel. An empty `"result":[]` means that no update
   arrived yet, or that a webhook is set for the bot, which `getUpdates`
   does not serve.
4. Set `chat_id` to that number and check it with
   `mailcrier --probe -- <target name>`: `class=perm status=400` names the
   reason Telegram gave, such as `chat not found`.

A group that becomes a supergroup gets a new id, `-100...`; messages to
the old id fail with status 400 until `chat_id` is changed.

Russian: [telegram.ru.md](telegram.ru.md).
