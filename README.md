# Mailcrier

Started as a fork of github.com/iggy/slendmail.

A sendmail replacement for machines that send no email: cron, at, sudo,
mdadm, smartd, fail2ban and every other tool that pipes a message into
`/usr/sbin/sendmail` get it delivered to Telegram, Discord, Slack, ntfy,
an HTTP webhook, a service of the shoutrrr library or a program of their
own instead.

- Accepts the command lines of the usual callers (`-t`, `-i`, `-f`, `-F`,
  `-oi`, `-bi`, `-q` and the rest) and reads MIME messages: encoded
  words, quoted-printable, base64, any charset, HTML parts and attachments.
- Sends to all configured targets at the same time, each in the markup of
  its service with everything from the message escaped, cut to the length
  the service accepts, with the attachments it can take.
- Keeps a message in a spool until every target has it: a service that is
  down gets it on a later queue run.
- One static binary, installed setgid so that the jobs of any user can
  send while only root reads the tokens; logs to syslog.

## Quick start

Mail of the host to a Telegram chat, on Debian or Ubuntu with the package
of a release; rpm and apk differ in the package of steps 1 and 2, see
[Installation](docs/install.md). Every step runs as root.

1. Set the version of the newest release on
   <https://github.com/6RUN0/mailcrier/releases> and download its package:

   ```sh
   VERSION=0.1.0-rc4
   curl -fLO "https://github.com/6RUN0/mailcrier/releases/download/v$VERSION/mailcrier_${VERSION}_linux_amd64.deb"
   ```

2. Install it; this replaces the MTA of the host:

   ```sh
   apt install "./mailcrier_${VERSION}_linux_amd64.deb"
   ```

3. Put the token of your bot into a file only root and the group
   `mailcrier` read; how to create a bot and find the id of its chat is
   under [Telegram](docs/targets/telegram.md#bot-and-chat-id):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/telegram.token
   printf '%s\n' '123456:ABC-your-token' > /etc/mailcrier.d/telegram.token
   ```

4. Append the target to `/etc/mailcrier.conf`, with the id of your chat:

   ```toml
   [target.ops-telegram]
   type = "telegram"
   token_file = "/etc/mailcrier.d/telegram.token"
   chat_id = -1001234567890
   ```

5. Check the file. The expected output is one line on stderr, exit status
   0:

   ```sh
   mailcrier --check-config
   ```

   ```text
   /etc/mailcrier.conf: 0 errors, 0 warnings
   ```

   An error names the line to fix, see
   [Checking the configuration](docs/checking.md).

6. Send a test message through the target. The expected output is one
   line on stdout, exit status 0, and the message in the chat:

   ```sh
   mailcrier --probe
   ```

   ```text
   target=ops-telegram class=ok
   ```

   `class=perm status=400` with `chat not found` means a wrong `chat_id`
   or a bot that is not in the chat.

7. Send a mail the way cron does and read its records in the log:

   ```sh
   printf 'Subject: test\n\nhello\n' | sendmail root
   journalctl -t mailcrier --since -5min
   ```

   The records `message received` and `target delivered` with
   `target=ops-telegram` show the delivery; without journald they are in
   `/var/log/mail.log` (Debian, Ubuntu) or `/var/log/maillog` (RHEL,
   Rocky), see [Logging](docs/logging.md#finding-a-message).

Until step 4 every message is held in the spool with exit status 78; the
first queue run after the file loads delivers them, see
[Spool and queue](docs/spool.md#held-messages).

## Documentation

| Page | What it answers |
|---|---|
| [Installation](docs/install.md) | packages, what they change on the host, upgrade, removal, build |
| [Configuration](docs/configuration.md) | the file, its strict check, terms, examples, `[general]`, `[strings]`, the table of target keys |
| [Targets](docs/targets.md) | the destinations, one page per type: [Telegram](docs/targets/telegram.md), [Discord](docs/targets/discord.md), [Slack](docs/targets/slack.md), [ntfy](docs/targets/ntfy.md), [HTTP and Mattermost](docs/targets/http.md), [exec hooks](docs/targets/exec.md), [shoutrrr](docs/targets/shoutrrr.md) |
| [Long messages](docs/long-messages.md) | how a text over the limit is cut, what goes along as a file |
| [Templates](docs/templates.md) | your own text per target: data, functions, failures |
| [Routes and suppression](docs/routing.md) | which target gets which message, dropping noise, direct chats |
| [Spool and queue](docs/spool.md) | retries, held and failed messages, limits, `mailq` |
| [Checking the configuration](docs/checking.md) | `--check-config` and `--probe` |
| [Logging](docs/logging.md) | where the records go, their fields, masked secrets |
| [Privileges](docs/privileges.md) | the setgid install and what an elevated call may do |
| [Containers](docs/containers.md) | running without the setgid bit, memory |
| [Command line](docs/cli.md) | sendmail flags, recipients, message parsing, exit statuses |
| [SELinux](docs/selinux.md) | the module of the rpm package |
| [mailcrier(8)](docs/mailcrier.8) | the manual page, also installed with the packages |
