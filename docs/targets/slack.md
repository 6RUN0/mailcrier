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
a member of the channel. `&`, `<` and `>` of the message are
escaped, so `<!channel>` or `<@U123>` stays text. The text holds at
most 40000 characters; up to 10 files are sent. An answer with `"ok":
false` is a failure; `ratelimited`, `internal_error`, `fatal_error`,
`service_unavailable` and `request_timeout` are temporary. Several `slack`
targets may post with different tokens to different channels.

## App and bot token

The target needs the bot token of a Slack app installed in the workspace
and the bot in the channel. The steps follow the Slack app quickstart
(<https://docs.slack.dev/app-management/quickstart-app-settings>) and the
reference of `chat.postMessage`
(<https://docs.slack.dev/reference/methods/chat.postMessage>).

1. On the apps page, <https://api.slack.com/apps?new_app=1>, click
   **Create an app**, choose **Blank app** and **Continue**, enter the
   **App Name**, pick the **Workspace** and click **Create**.
2. In **OAuth & Permissions**, under **Scopes**, **Bot Token Scopes**,
   click **Add an OAuth Scope** and add `chat:write`, for the text, and
   `files:write`, for the attachments. mailcrier calls no other method and
   needs no other scope.
3. At the top of **OAuth & Permissions** click **Install to Workspace**,
   then **Allow**. The token, `xoxb-...`, appears under **OAuth Tokens**
   and goes into a file only root and the group `mailcrier` read, as the
   token of the [quick start](../../README.md#quick-start):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/slack.token
   printf '%s\n' 'xoxb-your-token' > /etc/mailcrier.d/slack.token
   ```

   A scope added after the install takes effect once the app is
   reinstalled: **Reinstall to Workspace** on the **Install App** page.
4. In the channel, send `/invite @<app name>`: Slack answers
   `not_in_channel` to a bot outside the channel.
5. Set `channel` to the name of the channel, `#alerts`, or to its ID, `C`
   followed by letters and digits. The ID stays the same when the
   channel is renamed or turned private
   (<https://docs.slack.dev/faq>), a name does not.
6. Check with `mailcrier --probe -- <target name>`. Slack answers a
   rejected call with status 200 and an error code, so the line has no
   `status` and the code is the `err`, such as
   `target=prod-slack class=perm err="not_in_channel"`:
   - `not_in_channel`: the bot is not in the channel, step 4;
   - `channel_not_found`: a wrong name or ID;
   - `missing_scope`: `chat:write` is missing, or was added without the
     reinstall of step 3;
   - `invalid_auth`: the token is wrong;
   - `is_archived`: the channel is archived.

The probe message has no attachments, so it does not check `files:write`.
Without it a mail with an attachment arrives as text only and counts as
delivered; the log has `attachments not delivered` with `missing_scope`
in `err`.

Russian: [slack.ru.md](slack.ru.md).
