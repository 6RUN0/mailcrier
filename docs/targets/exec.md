# Exec hooks

The `exec` type runs a program of your own once per message, with the
message on stdin. Keys every target takes: [Targets](../targets.md).

```toml
[target.run]
type = "exec"
argv = ["/usr/local/bin/notify", "--channel", "ops"]
timeout = "30s"                 # optional, the default
```

A target of this type runs a program once per message. `argv` is the
command line, run without a shell; `argv[0]` must be an absolute path,
and no element may hold a NUL, else exit status 78. Nothing of the message
goes into `argv`.

- stdin: the message as it was read, without its Bcc and Resent-Bcc
  fields. The hook gets no text and no files, so the keys about text
  and files are not valid for it.
- Environment: only `PATH=/usr/sbin:/usr/bin:/sbin:/bin`,
  `LANG=C.UTF-8`, `TZ` when the caller set it (a setgid call keeps only
  a zone name), and `MAILCRIER_SUBJECT` (the decoded subject),
  `MAILCRIER_FROM` (the address of From, or the envelope sender),
  `MAILCRIER_TO` (the envelope recipients joined by `, `, without the
  addresses only Bcc or Resent-Bcc named), `MAILCRIER_HOSTNAME`,
  `MAILCRIER_TARGET` (the target name), `MAILCRIER_MSGID` (the
  Message-ID, empty when there is none) and `MAILCRIER_SIZE` (the bytes
  on stdin). In each value CR, LF and NUL become spaces, and the value
  is cut at a character to 4096 bytes; the whole environment stays
  within 64 KiB. The values come from whoever wrote the message: a hook
  quotes them (`"$MAILCRIER_SUBJECT"`) and never hands them to `eval`
  or `sh -c`.
- Outcome: exit status 0 is a delivery; 75 (`EX_TEMPFAIL`) a temporary
  failure, queued and retried; any other status, or death by a signal,
  a permanent failure. A hook that cannot be started (missing file, no
  execute permission, `E2BIG`) fails permanently; a script whose `#!`
  names a missing interpreter says so (`interpreter /usr/bin/python3
  of /usr/local/bin/hook not found`) rather than report the script
  missing, as the kernel does.
- Time: `timeout`, a positive Go duration, 30 s by default, bounds one
  run; so do `deadline` and the budget of a queue run, whichever ends
  first. Past it the process group of the hook (each hook starts in
  its own) gets `SIGKILL` and the run is a temporary failure; a child
  that left the group with `setsid` survives. After the hook exits, the
  call waits at most 2 s for children that still hold its stdout or
  stderr, and then goes on without them. `SIGINT`, `SIGTERM` or
  `SIGHUP` to mailcrier end the run the same way: the group is killed,
  the failure is temporary and the message stays queued (a second
  signal ends mailcrier at once). This holds once the message is read;
  a signal while it is still coming in, such as Ctrl-C while it is
  typed, ends mailcrier and discards the message, as sendmail does. A
  signal that mailcrier was started with ignored, as under `nohup`,
  stays ignored. When mailcrier dies by `SIGKILL`, the kernel kills the
  hook itself (parent death signal), but children the hook started
  survive and finish on their own.
- Output: stdout and stderr together, the first 4096 bytes, go into one
  `hook output` record with `target`, `output` and `output_size` (all
  bytes printed), the secrets of the configuration masked.
- Ids: a setgid call starts the hook with the real uid and gid of the
  caller and the caller's supplementary groups, without the `mailcrier`
  group, so the hook reads neither the configuration nor the spool. A
  queue run as the `mailcrier` user (the systemd timer or the cron file)
  starts the hooks of queued messages as `mailcrier` with the group
  `mailcrier`, which reads the configuration with all tokens and every
  spool entry: the hook program must be trusted that far. The working
  directory is that of the caller.
- Secrets: `argv` is visible to every local user through `ps` and
  `/proc/<pid>/cmdline` while the hook runs, and mailcrier does not
  mask it in the log, so it holds no token. A hook that needs one reads
  it itself; started by a setgid call it runs as the caller, so the
  secret is readable by every user who can run `sendmail`. Where that is
  not acceptable, the hook has to refuse to run as anyone but
  `mailcrier` (exit 75 keeps the message queued for the queue run of
  that user), and then reads what `mailcrier` reads, as said above.

Russian: [exec.ru.md](exec.ru.md).
