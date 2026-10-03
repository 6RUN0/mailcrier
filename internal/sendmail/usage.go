package sendmail

// Usage is the text of --help: the modes, flags, configuration files and
// exit statuses of docs/slendmail.8, for hosts without the manual page.
// TestUsageListsOptions and TestUsageListsExitStatuses keep it complete,
// TestUsageLayout within 79 columns of ASCII.
const Usage = `usage: slendmail [flags] [--] [recipient ...]
       slendmail -q | -bp | --status | --check-config | --version | --help
       slendmail --probe [-- target ...]
       mailq | newaliases

Reads one message on stdin and sends it as a notification to the targets
of /etc/slendmail.conf: Telegram, Discord, Slack, ntfy, HTTP, a program or
shoutrrr. A target that fails temporarily gets it from a later queue run.
sendmail, mailq and newaliases are links to slendmail.

Modes, no message is read:
  -q              run the queue once; an interval after -q is ignored
  -bp, mailq      list the queue: every entry for root, the slendmail user
                  and a caller without the setgid bit, the numbers for
                  everyone else
  --status        print the queue counts as one logfmt line
  --check-config  check the configuration and its templates, send nothing
  --probe [-- target ...]
                  send a sample message to every target or to those named;
                  routes and suppress rules do not apply
  -bi, -I, newaliases
                  do nothing
  --version       print the version
  --help          print this text
  Of several modes the last one wins; -bs always exits 64.

Message flags:
  -t              add To, Cc and Bcc to the recipients, or Resent-To,
                  Resent-Cc and Resent-Bcc instead when the message has any
                  of them
  -i, -oi         a line with a single dot does not end the message
  -f ADDR, -r ADDR
                  envelope sender; '' or <> is the null sender
  -F NAME         full name of the sender
  -bm             deliver, the default; -bs exits 64, other -b modes are
                  logged and ignored
  Accepted and ignored: -A -B -C -d -e -G -h -L -m -N -n -O -o -p -R -U
  -V -v -X. -C and unknown flags are logged and ignored.

Configuration:
  --config PATH, else SLENDMAIL_CONFIG=PATH
                  read another file; ignored with a warning when the setgid
                  bit applies to a caller other than root, and then
                  --check-config and --probe exit 64
  /etc/slendmail.conf
                  TOML, one [target.NAME] table per target
  /etc/slendmail.d/
                  files of token_file, url_file and template_file, by
                  convention; any absolute path works
  A minimal configuration:
    [target.ops-telegram]
    type = "telegram"
    token_file = "/etc/slendmail.d/tg.token"
    chat_id = -1001234567890
  Check it with --check-config, then send a test with
  --probe -- ops-telegram.
  Examples: /usr/share/doc/slendmail/examples/ and
  https://github.com/6RUN0/slendmail/tree/develop/packaging/examples

Files:
  /var/spool/slendmail/
                  the spool, unless dir of [spool] names another, or none
                  when dir is "": entries in queue/, hold/ and failed/,
                  files being written in tmp/, queue run locks in locks/

Exit status:
  0   a target accepted the message, even if another rejected it; or none
      rejected it and it is queued for those that failed temporarily; or a
      suppress rule matched; or a mode succeeded
  64  usage error, or no route selects a target and the message is held
  66  standard input cannot be read
  69  no target accepted the message and one rejected it, or all failed
      temporarily with the spool off; --probe: a target rejected it
  70  internal error
  73  a target failed temporarily and no spool entry could be created
  74  a target failed temporarily and the spool entry could not be written,
      -q could not read the spool, or --status found no spool directory
  75  --probe only: a target failed temporarily, none rejected
  77  --check-config or --probe by a setgid caller other than the slendmail
      user
  78  the configuration cannot be read or is invalid; a message being sent
      is held, --check-config, --probe and -q hold nothing

The manual: man 8 slendmail.
`
