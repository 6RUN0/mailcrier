# Long messages

How a text over the limit of a target is cut, and the keys `on_long`,
`long_file`, `max_text` and `max_lines` that decide what the target gets.
The limits of each service are on the page of its type, under
[Targets](targets.md).

## How a text is cut

A text longer than the target accepts is cut: first a subject longer
than a quarter of the limit (at least 64 units of the target) is cut
at a word and ends with `...`, then the list of attachments, then the
body, which ends with the `truncated` notice. The limit is counted the
way the service counts it, after escaping. When the rest of the text,
host and sender included, is too long on its own, the text is cut hard;
for Telegram between tags and entities, with the open tags closed. What
goes along with a cut text is set per target, as follows.

## What goes along with a cut text

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/telegram.token"
chat_id = -1001234567890
on_long = "file"        # optional: file | truncate | blockquote
long_file = "text"      # optional: text (message.txt) | eml (message.eml)
max_text = 2000         # optional, below the limit of the service
max_lines = 40          # optional, lines of the body in the text
```

- A text over the limit of a target is cut as described under [How a text is
  cut](#how-a-text-is-cut). With `on_long = "file"`, the default, a target
  that sends files gets the full text as `message.txt` (`text/plain`,
  UTF-8, the message in the plain layout without a limit) as its first
  file, listed first among the attachments of the text, which ends the
  cut body with the `truncated` notice. `long_file = "eml"` sends the
  message itself as `message.eml` instead, as it was read, without its
  `Bcc` and `Resent-Bcc` fields; a queued message is sent the same way.
- `on_long = "truncate"` sends the cut text alone; so does a target without
  files (`http`) and a target whose file limit the full text exceeds. The
  body then ends with the `truncated_size` notice, such as
  `[truncated, 1.9 MiB in full]`, with the size of `message.txt`.
- `on_long = "blockquote"` is `file` with the cut body in an expandable
  blockquote in Telegram, which shows a few lines until opened; the other
  targets have no such block and treat it as `file`.
- `max_text` replaces the text limit of the service, in its unit: the
  characters of Telegram, Discord and the `mattermost` and `generic-json`
  documents, the UTF-16 units of Slack and the `slack-webhook` document,
  the bytes of ntfy. A value above the limit of the service is
  taken as it is, and the service rejects a longer text. For `http` the
  limit covers the whole JSON document: one too small for the document
  without body and subject fails the target permanently.
  `max_lines` cuts a body of more lines after that many and treats the
  text as cut; trailing empty lines do not count. Both must be positive;
  `long_file`, like `on_long`, is not a key of `http`.
- A Telegram target that answers 400 to the text, for markup it cannot
  parse or a text over its limit, gets the full text once more as a file
  alone, without text or caption, whatever its `on_long`; the log records
  `text rejected, sent as file` (warning). When that fails too, the record
  is `text rejected, file failed too` (warning) and the target fails with
  the error of the second attempt. Every 400 counts, a wrong `chat_id`
  included, which costs one more request.

Russian: [long-messages.ru.md](long-messages.ru.md).
