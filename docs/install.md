# Installation

Where the packages come from, what they install and change on the host,
how to upgrade and remove them, and how to build the binary. The first
configuration is under [Configuration](configuration.md).

## Packages

Each release on <https://github.com/6RUN0/mailcrier/releases> has deb, rpm
and apk packages of the full build for amd64, arm64 and armv7, without
signatures (`apk add --allow-untrusted`), and `checksums.txt` with the
SHA-256 of every file. Every file listed there also has a build provenance
attestation of the release workflow, which the GitHub CLI checks:

```sh
sha256sum --ignore-missing -c checksums.txt
gh attestation verify --repo 6RUN0/mailcrier \
  --signer-workflow 6RUN0/mailcrier/.github/workflows/release.yml \
  ./mailcrier_<version>_linux_amd64.deb
apt install ./mailcrier_<version>_linux_amd64.deb
dnf install ./mailcrier_<version>_linux_amd64.rpm
apk add --allow-untrusted ./mailcrier_<version>_linux_amd64.apk
```

| Path | Owner and mode |
|---|---|
| `/usr/sbin/mailcrier` | `root:mailcrier 2755`, set by the scripts after unpacking |
| `/usr/sbin/sendmail`, `/usr/lib/sendmail`, `/usr/bin/mailq`, `/usr/bin/newaliases` | links to it; in rpm the alternative `mta` |
| `/etc/mailcrier.conf` | `root:mailcrier 0640`, no target |
| `/etc/mailcrier.d/` | `root:mailcrier 0750`, for `token_file` and the like |
| `/var/spool/mailcrier/` | `root:mailcrier 0750`; `tmp`, `queue`, `hold`, `failed`, `locks` in it `2770` |
| `mailcrier-queue.timer` and `.service`, `/etc/cron.d/mailcrier` | queue run, deb and rpm |
| `/etc/crontabs/mailcrier` | queue run, apk |
| `/usr/share/doc/mailcrier/examples/` | example configurations, see [Configuration](configuration.md#examples) |
| `/usr/share/selinux/packages/mailcrier/mailcrier.cil` | SELinux module, rpm, see [SELinux](selinux.md) |

The install creates the system group and user `mailcrier` (home
`/var/spool/mailcrier`, no login shell) unless they exist. Then, as root:

1. Write a target into `/etc/mailcrier.conf`; its comments and the examples
   show how. A secret goes alone into a file under `/etc/mailcrier.d`,
   `root:mailcrier 0640`.
2. `mailcrier --check-config` reports errors and warnings, exit status 0
   when the file loads.
3. `mailcrier --probe` sends a test message to every target.

On upgrade, rpm and apk set the owner and mode of `/etc/mailcrier.conf`
back to `root:mailcrier 0640` when its content is that of the package;
dpkg keeps them. A secret therefore belongs in a file under
`/etc/mailcrier.d/`, where the package ships no file and changes none.

Until a target is set, every message is held in `hold/` with exit status
78, and `mailcrier-queue.service` exits 78 every 5 minutes and is listed by
`systemctl --failed`; the first queue run after the configuration is fixed
delivers the held messages.

## Distributions

The timer is enabled on install and started where systemd runs; where it
does not, the cron file runs the queue. The package scripts never run
mailcrier and never touch the configuration.

- Debian and Ubuntu: the package provides, conflicts with and replaces
  `mail-transport-agent`, so installing it removes the MTA in place
  (postfix, exim4, msmtp-mta, ...), and removing mailcrier does not bring
  that one back. The setgid bit is a `dpkg-statoverride` entry, which dpkg
  applies to every later version; one the administrator set before is
  kept.
- RHEL, Rocky, Alma, Fedora: the links are the alternative `mta` with
  priority 100, above postfix (60) and sendmail (90), so mailcrier is
  the active one after install; `alternatives --config mta` chooses
  another. After removal the remaining MTA is active again. The package
  installs an SELinux module that labels mailcrier like a stock MTA, so
  confined callers such as smartd can mail; a hook then runs in the mail
  domain of its caller, see [SELinux](selinux.md). cronie decides when
  it starts whether it mails the output of jobs: a crond started while
  no MTA was installed logs that output until `systemctl restart crond`,
  and the install prints a reminder while crond runs.
- Alpine: the package replaces the BusyBox link `/usr/sbin/sendmail`, and
  BusyBox puts it back after removal. ssmtp, dma and opensmtpd own
  `/usr/sbin/sendmail` as well: apk refuses to overwrite it (`trying to
  overwrite usr/sbin/sendmail owned by ...`), so remove them first. The
  queue runs from BusyBox crond, which Alpine does not start by default:
  `rc-update add crond && rc-service crond start`. Without it a message
  that failed is retried only by the next call of the same user.

## The setgid bit in rpm and apk

In rpm and apk the binary is unpacked as `root:root 0755` on install and on
every upgrade, and the install script then gives it the group and the
setgid bit. Consequences:

- `rpm -V mailcrier` reports `.M....G..  /usr/sbin/mailcrier` and `apk audit
  --check-permissions` reports `M usr/sbin/mailcrier`; that is expected.
- During an upgrade, between unpacking and the script, a call of a user
  other than root cannot read the configuration, and `hold/` is not
  writable for that user either: the message is lost (`message lost, not
  held`) and the call exits 73.
- `rpm --setperms`, `rpm --setugids`, `rpm --restore`, an install or
  upgrade with `--noscripts`, `apk fix` and `apk add --no-scripts` remove
  the bit for good, with the same result for every such call until it is
  restored as root:
  `chgrp mailcrier /usr/sbin/mailcrier && chmod 2755 /usr/sbin/mailcrier`
  (`chgrp` first: it clears the bit).

## Removal

Removal (`dpkg -r`, `rpm -e`, `apk del`) stops the timer. It keeps the
user, the group and a changed configuration (`dpkg -P` deletes it, `rpm -e`
keeps it as `/etc/mailcrier.conf.rpmsave`). A spool without messages in
`queue/`, `hold/` and `failed/` is removed with the package; one with
messages stays, and every format prints its path on removal (deb on
`dpkg -r` and on `dpkg -P`). A call during the removal can leave the spool
with nothing but `locks/` in it, and files an interrupted call left in
`tmp/` keep it as well (dpkg reports the directory as not empty);
such a directory holds no message and can be deleted. Debian keeps
`/etc/cron.d/mailcrier` until `dpkg -P`; its job does nothing while the
binary is missing.

## Build

```sh
go build -o mailcrier ./cmd/mailcrier
```

With `-tags noshoutrrr` the binary leaves out the shoutrrr library and
the 12 modules it brings, about 3 MB of 13.7 MB (linux/amd64, `-trimpath
-ldflags='-s -w'`, go1.27.1); a `shoutrrr` target then rejects the
configuration with exit status 78 and `built without shoutrrr` in the log.
Releases carry both builds, the smaller one as `mailcrier-minimal`.

The binary goes to `/usr/sbin/mailcrier`, with `/usr/sbin/sendmail`,
`/usr/lib/sendmail`, `/usr/bin/newaliases` and `/usr/bin/mailq` as links
to it; ownership and modes are under [Privileges](privileges.md). The
manual page is `docs/mailcrier.8`.
