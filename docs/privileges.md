# Privileges

Who may read the configuration and the spool, and what a setgid call
does to keep the group privilege to itself. The paths and modes the
packages set are under [Installation](install.md); running without the
setgid bit is under [Containers](containers.md).

- The intended install is setgid: binary `root:mailcrier 2755`,
  configuration and `*_file` files `root:mailcrier 0640`, so that cron jobs
  of any user can send while only root and the binary read the secrets.
  When a user other than root runs the binary and the kernel applies the
  setgid bit:
  - before doing anything else the process executes itself once more,
    through `/proc/self/exe` or else `/usr/sbin/mailcrier`, with the
    environment reduced to `USER`, `LOGNAME`, `HOME`, `LANG`, `LC_*` and
    `TZ` (a zone name such as `Europe/Berlin` only), so that `GODEBUG`,
    proxy or TLS variables of the caller cannot act with the group
    privilege. If both fail, it replaces its environment the same way and
    goes on with HTTP/2 off, the Go debug output dropped and no proxy, and
    logs `reexec failed` as an error;
  - `--config` and `MAILCRIER_CONFIG` are ignored with a warning; with
    `--check-config` or `--probe` they exit 64 instead, so that a mode
    never works on another file than the one named;
  - `--probe` and `--check-config` exit 77 for everyone but root and the
    `mailcrier` user.
- On a host with the setgid install, a caller running under
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes` does not get the group:
  neither the configuration nor the spool is accessible to a caller other
  than root, the message is lost (`message lost, not held`) and the call
  exits 73.

Russian: [privileges.ru.md](privileges.ru.md).
