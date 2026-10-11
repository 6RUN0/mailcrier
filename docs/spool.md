# Spool and queue

Every message is written to a spool before it is sent, so that a target
that is down gets it on a later queue run. This page holds the `[spool]`
settings, the areas `tmp/`, `queue/`, `hold/`, `failed/` and `locks/`,
retries, limits and `mailq`. On a package install the defaults need no
change.

## Settings

```toml
[spool]                         # optional; the values are the defaults
dir = "/var/spool/mailcrier"    # "" turns the spool off
drain_budget = "10s"            # queue run after the own message of a call
drain_max_messages = 20
run_budget = "60s"              # queue run of -q
queue_ttl = "168h"              # then queue/ -> failed/
hold_ttl = "168h"               # then hold/ -> failed/
failed_ttl = "720h"             # then deleted
max_queue_messages = 1000
max_queue_bytes = 268435456
max_queue_messages_per_uid = 200
max_queue_bytes_per_uid = 67108864

[target.api]
type = "http"
url_file = "/etc/mailcrier.d/api.url"
preset = "generic-json"
```

## Entries and retries

- Every message is written to the spool before its delivery: the raw
  message as `queue/<id>.eml` and its state per target as
  `queue/<id>.json`, each first written to `tmp/`, synced and moved into
  `queue/`. The state is rewritten after each target, and the entry
  removed once no target waits for it, or moved to `failed/` with the
  reason `internal error` when a target failed by a panic (see [Exit
  status](cli.md#exit-status)). A target that fails temporarily
  stays pending, and the call exits 0 with the warning `message queued` (or
  `message queued for target` when another target has the message): the
  message is accepted, as a classic MTA accepts it into its queue.
- A queue run retries a pending target one minute after its first failure,
  then after twice the previous delay, at most 24 hours, and never before
  the delay a service asked for with `Retry-After`; when a service asks for
  a delay, the rest of the run skips that target in the other entries. An
  entry queued longer than `queue_ttl`, counted from its release for a
  message that was held, moves to `failed/` with the error
  `message failed`, and after `failed_ttl` there it is deleted. Files
  older than one hour that a killed process left behind, in `tmp/` unless
  a live process still holds them and sidecars without their message
  elsewhere, are removed. A message file without its sidecar, which
  `mailq` lists as `unreadable`, is the trace of a removal cut short once
  no target waited for the message: the next run that comes to it deletes it, logged
  as `message without sidecar removed` with its id and area.
- An entry whose sidecar cannot be read stays where it is with the
  warning `spool entry unreadable, left alone` on every queue run, which
  then exits 74. A corrupt sidecar has lost the dates of the entry, so
  the message file stands in for them: written with the entry and never
  rewritten, once it is older than `queue_ttl` (`hold_ttl` in `hold/`)
  the entry moves to `failed/` with the reason `corrupt sidecar` and a
  new sidecar without targets, owned by the owner of the file. Owner and
  age are approximate: a message that `-q` copied from `hold/` of the
  default directory belongs to the user of that run and dates from the
  copy, and one released from `hold/` counts from the time it was held,
  not from its release. The message is kept as it was; when its headers
  name the recipients, `mailcrier -t < failed/<id>.eml` sends it again.
  A corrupt sidecar in
  `failed/` goes with its message `failed_ttl` after the file was
  written. An entry of another sidecar version is left alone, for the
  release that wrote it, with its own warning and exit status 0.
- A retry renders the stored message with the configuration and templates
  of the moment, to the targets fixed when it was queued. Renaming or
  removing a target drops its pending messages: they fail for that target
  with `target removed, message dropped for it`.

## Held messages

- A message whose configuration is rejected goes to `hold/` (of the
  default directory when the file cannot be read) and the call exits 78;
  the first queue run with a valid configuration routes it as a new
  message, see [Routes and suppression](routing.md). When the valid file names a
  `dir` other than the default, `-q` also moves what `hold/` of the
  default directory keeps into that queue. `hold_ttl` bounds the wait:
  while the file is rejected, `-q` moves expired entries to `failed/` and
  exits 78, in the `dir` of the file when it parses and only its targets
  are rejected, as the call held its message there, in the default
  directory otherwise.
  A message to be held, for a rejected configuration or without a route,
  whose entry cannot be created or written (`message lost, not held`)
  exits 73 or 74 instead of 78 or 64, as a temporary failure without a
  spool entry does: the message is not kept. Whether it is tried again is
  up to the caller: cron, PHP `mail()` and most scripts tell no exit status
  from another, while an MTA that runs mailcrier as a pipe transport
  defers only on 75 (Postfix) or on 75 and 73 (Exim) and bounces on the
  others.

## Queue runs

- Each call runs the queue for at most `drain_budget` and
  `drain_max_messages` entries after delivering its own message, only for
  the entries of its caller's uid, and not at all while another call of the
  same user is doing so (`locks/drain-<uid>.lock`). `mailcrier -q` runs it
  for at most `run_budget`: root, the `mailcrier` user and a process
  without the setgid bit take every entry, any other user only their own.
  An entry is locked while a process works on it, and a run skips locked
  entries, so parallel runs never deliver an entry twice.
- The packages run `mailcrier -q` as `mailcrier` every 5 minutes: the
  systemd timer `mailcrier-queue.timer` (its service stops a run after 3
  minutes, which covers `run_budget` of 60 seconds and the delivery under
  way; a larger `run_budget` needs a larger `TimeoutStartSec` in a
  drop-in), or where systemd is not running
  `/etc/cron.d/mailcrier`, or `/etc/crontabs/mailcrier` on Alpine, all
  without mail from cron since `-q` logs to syslog only (`MAILTO=` for
  BusyBox crond, which takes `MAILTO=""` for an address). Without a running cron
  daemon or timer, as in most containers, only the calls themselves run the
  queue; a container that sends rarely runs `mailcrier -q` from a
  scheduler such as supercronic, a sidecar or a health check.

## Limits and guarantees

- Limits: all entries in `tmp/`, `queue/` and `hold/` together stay within
  `max_queue_messages` and `max_queue_bytes`, those of one uid within the
  `_per_uid` limits; users other than root and `mailcrier` fill at most 80%
  of the totals, so root still gets its mail queued. Parallel calls never
  exceed a limit together, but near it they may all be refused where one
  would have fit. A message over a limit
  is not queued (`message not spooled` with `err="spool entry not created:
  quota exceeded: ..."`): it is delivered all the same, and a temporary
  failure then exits 73.
- Delivery is at least once: a process killed after a service accepted
  the message, or a timeout after the service accepted it, repeats that
  delivery on the next run, and so may a power failure, since the state
  written after a delivery is not synced to disk before the next step.
  A state that cannot be written at all (`spool entry not updated`, a full
  disk) leaves the entry as it was before the attempt: the rest of that
  call or run skips it, and the next run, in another process, finds it
  due and tries again without the delay of a retry.
- The spool must be on a local file system: the locks (`flock`) do not
  exclude processes over NFS. The directory must exist; the program creates
  `tmp`, `queue`, `hold`, `failed` and `locks` in it when missing, which
  suits a container. With the setgid install the packages create the
  directory and all five as `root:mailcrier 2770`: made by the program,
  they would belong to whichever user sent first. Entries are created with
  mode `0660`.

## Listing the queue

- `mailq` and `-bp` list the entries of `queue/`, `hold/` and `failed/`
  with the state, attempts, next attempt and last error of each target,
  for root, the `mailcrier` user and a process without the setgid bit;
  anyone else sees one line of counts; a spool directory that does not
  exist lists as `queue is empty`, the spool off (`dir = ""`) as `spool
  off`. An entry that cannot be read lists as `<id> <area> unreadable
  err="..."` with the cause a queue run logs, such as `corrupt sidecar:
  not a JSON object`. The last error of a target is shown under its
  class, `temp="status 503: Service Unavailable"`. `--status` prints one
  logfmt line for monitoring,
  `queued=1 held=0 failed=0 tmp=0 bytes=1432 oldest_age_seconds=75`, and
  exits 74 when the directory does not exist, and also, after printing the
  line, when this process cannot write one of its areas or the file
  system is read-only (`spool not writable`). Neither creates anything in
  the spool. `-q` on such a spool exits 74 without a run (`spool not
  writable, queue not run`): it would deliver entries it cannot record
  and deliver them again on the next run.
- Both also count, and `mailq` lists with the field `dir="..."`, what
  `hold/` of the default directory keeps when the file names another
  `dir`, which `-q` moves into that queue; `bytes` and `tmp` stay those
  of the `dir`, and a default directory the caller cannot read gives the
  warning `default spool not listed` and the rest of the output. `-q` of
  a user other than root and `mailcrier` without the permission to list
  it leaves that directory alone, with the same warning and exit status
  0, as a user without the setgid bit beside the spool of the packages
  cannot have held anything there; any other failure to list it, or one
  of root or `mailcrier`, exits 74. Under a
  rejected configuration they show the `dir` of the file when it parses
  and only its targets are rejected, since a call holds its message
  there, the default directory otherwise, and log `spool listed under a
  rejected configuration` with the directory. While only the targets are
  rejected, `-q` leaves the default directory alone: what its `hold/`
  keeps neither expires nor moves, and its `failed/` is not cleaned,
  until the targets load again.
- When `-q`, `mailq` or `--status` fails, or lists under a rejected
  configuration, stderr gets one line per cause besides the record in
  the log, interactive or not, such as `mailcrier: spool not opened:
  stat /var/spool/mailcrier: no such file or directory`, or `mailcrier:
  queue run incomplete, see the mail log` after a run with errors on
  single entries. A setgid-elevated caller gets the message without the
  cause, and without syslog only the records go to stderr. Under systemd
  the journal then holds the line besides the record; the cron file of
  the packages discards it (`MAILTO=`).

Russian: [spool.ru.md](spool.ru.md).
