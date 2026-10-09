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

## Documentation

| Page | What it answers |
|---|---|
| [Installation](docs/install.md) | packages, what they change on the host, upgrade, removal, build |
| [Configuration](docs/configuration.md) | the file, its strict check, examples, `[general]` and `[strings]` |
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
