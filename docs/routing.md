# Routes and suppression

Which targets get a message: `[[route]]` rules by recipient, sender and
subject, `[[suppress]]` rules that drop a message, and direct chats that
let a caller name a Telegram chat. Without any of them every
[target](targets.md) gets every message.

## Routes and suppression rules

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/telegram.token"
chat_id = -1001234567890

[target.mm]
type = "http"
url_file = "/etc/mailcrier.d/mm.url"
preset = "mattermost"

[[suppress]]
subject = "*Anacron*"

[[route]]
subject = "*raid*"
targets = ["ops-telegram"]
continue = true

[[route]]
recipient_regex = '^backup(@|$)'
targets = ["mm"]

[[route]]                       # no condition: every message
targets = ["mm"]
```

- Without a `[[route]]` every target gets every message, except one whose
  recipients are all direct chats (`<chat>@telegram` with
  `telegram_direct`), which goes to those chats alone. With routes, the
  rules are checked from the first, for each recipient of the envelope on
  its own: the targets of the first rule whose conditions all match are
  taken, and with `continue = true` the check goes on and the targets of
  the next matching rules are added. The targets of all recipients are
  joined: a message to `root` and `backup@example.org` reaches the targets
  of both, even when the rule for `root` comes first without `continue`. A
  rule without conditions matches every message.
- A condition is `subject`, `sender` or `recipient`:
  - `subject` is the subject with encoded words decoded and white space
    collapsed, empty when the message has none;
  - `sender` is the envelope sender, see [Command line](cli.md#recipients-and-sender):
    `-f` or `-r`, else `Resent-From`, else `From`, else the caller; empty
    for the null sender;
  - `recipient` is one envelope recipient, `Bcc` included, as its address
    without the display name: `Root <root@example.org>` on the command
    line is `root@example.org`. A local name stays without a domain: cron
    mails `root`, which `recipient = "root"` matches and `root@*` does
    not.
- Each condition is a glob, or an RE2 expression in Go syntax under the
  key with `_regex` (`subject_regex`, `sender_regex`, `recipient_regex`);
  a rule takes at most one of the two keys of a field. A glob matches the
  whole value with case ignored, Cyrillic and other scripts included: `*`
  is any run of characters, `/` and line breaks included, `?` one
  character, and `\` makes the next character literal; there are no
  `[...]` classes. An expression matches anywhere in the value unless
  anchored with `^` and `$`, with case as written unless it starts with
  `(?i)`. An empty glob or expression is an error: a field without a
  condition leaves its key out.
- `targets` is required, a list of configured target names without
  repeats.
- A message without recipients, as `-t` without recipient headers gives,
  is checked once: only a rule without `recipient` can match it.
- A recipient that no rule matches, while others have targets, is logged
  as `no route for recipient` (warning) with `unrouted`, the number of such
  recipients, never the addresses. When no rule matches at all and no
  recipient is a direct chat, the message is held in `hold/` with the
  reason `no route`, the log records `no route for message` (warning), and
  the call exits 64. With the spool off the message is lost (`message
  lost, spool off`) with the same status; when its entry cannot be created
  or written it is lost as well (`message lost, not held`), and the call
  exits 73 or 74. A queue run checks a held message against the
  rules of the moment and queues it once they select a target; until then
  it stays, counted in the limits of the spool, and moves to `failed/`
  after `hold_ttl`. Until then every later call of the same user and
  every `-q` read and route it again, before the retries of `queue/` and
  out of the same `drain_budget` or `run_budget`: held messages up to the
  per-user limit of the spool take that time from the retries. The
  warning comes once: a run that finds no route again logs it as debug.
  The targets chosen are logged as `message routed` (debug) with
  `targets`.
- The targets of a queued message are fixed when it is queued: a change
  of the routes affects new messages and those released from `hold/`,
  not the retries.
- A `[[suppress]]` rule takes `subject` and `sender` as above, or their
  `_regex` keys, at least one of them: a rule without conditions would
  drop every message. The rules are checked before the routes and before
  the spool; a message that matches every condition of one of them is
  neither sent nor spooled, the log records `message suppressed` (info)
  with `rule`, the number of the rule from 1, and the call exits 0. A
  message released from `hold/` is checked again, and a suppressed one is
  deleted.
- An error in a rule exits 78 like any configuration error and names the
  rule by its number and line (`route 3: ...`), never its expression. The
  line is that of the `[[route]]` or `[[suppress]]` table; rules written
  as an inline array, `route = [{ ... }, { ... }]`, all point at the line
  of its key, and only the number tells them apart.

## Direct chats

Direct chats let a caller name a Telegram chat as a recipient:

```toml
[general]
telegram_direct = "ops-telegram"
telegram_direct_chats = [1234, -1001234567890, "@ops_channel"]
telegram_direct_max = 10        # optional, the default

[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/telegram.token"
chat_id = -1001234567890
```

- With `telegram_direct` naming a `telegram` target, a recipient
  `<chat>@telegram`, the domain in any case, gets a copy of that target
  sent to the chat: `1234@telegram` a user or bot chat,
  `-1001234567890@telegram` a group, `@ops_channel@telegram` a public
  chat by its username. The copy has the token, template, limits,
  `on_long`, `long_file` and `disable_notification` of the target, but no
  `message_thread_id`: a topic belongs to the chat of the target, and
  another chat refuses it. Spellings of one chat are one chat: leading
  zeros of an id and the case of a username do not count.
- Only the chats of `telegram_direct_chats` get a copy. Any local user
  can name a recipient, and with the setgid install users cannot read the
  token, so this list is what decides where the bot writes for them. It is
  required with `telegram_direct`, holds integers and `@username` strings
  (5 to 32 letters, digits or `_`) without repeats; a string of digits is
  an error, as are the list and `telegram_direct_max` without
  `telegram_direct` (exit 78). A chat not in the list, `@telegram`, or any
  other local part is an ordinary recipient, routed like the others and
  logged as `direct chat not allowed` or `direct address invalid`
  (warning) with `count`, never the address. Without `telegram_direct`
  these addresses are ordinary recipients without a warning.
- `telegram_direct_max`, a positive integer, bounds the copies of one
  message: the chats past it, counted in the order of the recipients
  after repeats are dropped, get nothing, and the log records
  `direct chats over limit` (warning) with `dropped`. Every copy goes out
  with the one token of the bot, and a delay a service asks for, as with
  a 429, holds back only the copy that got it for the rest of a queue run,
  not the target the copies are made of nor the other copies.
- The routes see only the other recipients. A message to direct chats
  alone goes to them alone, even past a rule without conditions; when
  the routes select the target of `telegram_direct` and a direct
  recipient names the chat of that target, the chat gets one message.
  With a direct chat, recipients without a route only add the
  `no route for recipient` warning.
- A copy is named `<chat>@telegram` with the chat normalized
  (`1234@telegram`, `@ops_channel@telegram`) in the log (`target`), in the
  spool and in `mailq`: the chats come from the configuration, unlike the
  other addresses. A queued copy is retried with the target and the list
  of the moment, and fails with `target removed, message dropped for it`
  once `telegram_direct` or its chat is gone from the file.
- On the command line a negative id reads as an option: recipients go
  after `--`, as in `sendmail -- -1001234567890@telegram`.
