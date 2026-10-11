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

## Webhook URL

The target needs the URL of a webhook of the channel. The steps follow
the Discord article on webhooks
(<https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks>)
and the reference of Execute Webhook
(<https://docs.discord.com/developers/resources/webhook#execute-webhook>).

1. Open **Server Settings**, the **Integrations** tab, and click
   **Create Webhook**; this needs the permission to manage webhooks
   (`MANAGE_WEBHOOKS`). Pick the text channel in the drop-down menu and
   name the webhook: messages show the name as their author.
2. Click **Copy Webhook URL**. The URL,
   `https://discord.com/api/webhooks/<id>/<token>`, is the only
   credential: anyone who has it can post into the channel. It goes into
   a file only root and the group `mailcrier` read, as the token of the
   [quick start](../../README.md#quick-start):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/discord.url
   printf '%s\n' 'https://discord.com/api/webhooks/<id>/<token>' > /etc/mailcrier.d/discord.url
   ```

3. To post into a thread of the channel, append `?thread_id=<thread id>`
   to the URL; Discord unarchives an archived thread. A forum or media
   channel takes a webhook message only with `thread_id` or with
   `thread_name`, which creates a thread; mailcrier sends no
   `thread_name`, so a webhook of such a channel needs `thread_id`.
4. Check with `mailcrier --probe -- <target name>`: `class=ok` and the
   probe message in the channel. A deleted webhook or a cut URL gives
   `class=perm` with the HTTP status in `status` and its text in `err`.
   The reason Discord names in the body, `Unknown webhook` or
   `Invalid webhook token provided`, is not logged, as the body may echo
   the request; status 429 and 5xx are temporary.

Russian: [discord.ru.md](discord.ru.md).
