# Containers

Running mailcrier where the setgid bit does not apply: a container, a
unit with `NoNewPrivileges=yes`, a copied binary. The setgid install is
under [Privileges](privileges.md).

- Without the setgid bit taking effect the binary runs with the ids of its
  caller, and that is the normal mode for containers: Docker with
  `--security-opt no-new-privileges`, Kubernetes with
  `allowPrivilegeEscalation: false`, a systemd unit with
  `NoNewPrivileges=yes` or `RestrictSUIDSGID=yes`, or a binary copied
  without the bit. The configuration and its `*_file` files must then be
  readable by the caller (for PHP-FPM, for example `0640 root:www-data`),
  `--config` and `MAILCRIER_CONFIG` work, and secrets mounted as files
  (Docker or Kubernetes secrets) go in through `*_file`. For PHP set
  `sendmail_path = /usr/sbin/sendmail -t -i` with `/usr/sbin/sendmail` a
  link to the binary, and pass the variable with
  `env[MAILCRIER_CONFIG] = /run/secrets/mailcrier.conf` in the pool
  configuration. The spool directory, `/var/spool/mailcrier` unless
  `[spool] dir` names another, must exist and be writable by the caller (a
  volume, when queued messages must survive the container); without it a
  temporary failure exits 73, and `dir = ""` turns the spool off.
- Memory of one call, as the peak of its cgroup (Docker, `--cpus=1`, amd64,
  Go 1.27.2): a message of up to 100 KiB takes under 15 MB, and 20 such
  calls at once took 58 MB together. A message of 10 MiB, the most that
  is read, takes up to about 40 MB for an `exec` target, about 100 MB
  with one Telegram target, about 200 MB with 3 and about 420 MB with 10:
  each target with a text limit renders the whole body. Calls at the same
  time add up. A call the kernel kills for memory exits 137 to its
  caller. Killed while the spool entry is written, the message is lost,
  and the files left in `tmp/` are deleted after an hour. Killed after
  that, the entry stays in `queue/`, and every retry renders it again
  until `queue_ttl` moves it to `failed/`.

## Checklist

What a container needs besides the points above, each described where it
belongs:

- The log: without `/dev/log` the records go to stderr, and PHP-FPM passes
  them on only with `catch_workers_output = yes`, see
  [Logging](logging.md#where-the-records-go).
- The queue: without a cron daemon or timer only the calls themselves run
  it; `mailcrier -q` from a scheduler such as supercronic retries the rest,
  see [Spool and queue](spool.md#queue-runs).
- A complete example with the spool on a volume and secrets in
  `/run/secrets`:
  [`packaging/examples/container.conf`](../packaging/examples/container.conf).

Russian: [containers.ru.md](containers.ru.md).
