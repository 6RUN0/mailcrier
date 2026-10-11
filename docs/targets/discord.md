# Discord

The `discord` type posts through a webhook of a Discord channel. Keys every
target takes and how a failure is classified: [Targets](../targets.md);
a text over the limit: [Long messages](../long-messages.md).

```toml
[target.ops-discord]
type = "discord"
url_file = "/etc/mailcrier.d/discord.url"  # https://discord.com/api/webhooks/<id>/<token>
```

A target of this type makes one webhook execution per message with
`wait=true`, so that Discord answers after storing the message, and
`allowed_mentions` with an empty `parse` list, so that `@everyone`,
`@here` and user or role mentions in the message do not ping. A query
of the URL, such as `thread_id`, is kept. The text holds at most 2000
characters, also outside the BMP; up to 10 files of at most 20 MiB
and 25 MiB together go in the same request.

Russian: [discord.ru.md](discord.ru.md).
