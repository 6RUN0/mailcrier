# SELinux check

The rpm package installs `/usr/sbin/mailcrier` setgid and is called from
confined domains: user and system cron jobs, atd, smartd, the queue
service. A container cannot show whether the policy of the distribution
denies any of that, because the policy belongs to the kernel of the host.
The check runs the same callers on a virtual machine with SELinux
enforcing and collects the denials; its result goes into "Result" below,
one run per Rocky major version.

## Running it

```sh
make snapshot
make -j2 selinux-rocky9 selinux-rocky10
```

It needs docker and a readable and writable `/dev/kvm` on an x86-64-v3
host (Rocky 10), and network: the first run fetches the cloud images of
Rocky into `.e2e/selinux` (about 600 MB each, checked against the sums in
the `Makefile`), and the virtual machine installs its packages from the
mirrors of Rocky. Each target boots the image under qemu in the container
of `testdata/selinux`, with a throwaway overlay disk, an IDE disk with
SMART for smartd and 2 GB of memory, logs in as root over ssh, runs
[`check.sh`](../testdata/selinux/check.sh) on the rpm of `dist/` and powers
the machine off; a run takes 10 to 15 minutes. `TestSELinux`
(`selinux_test.go`, tag `selinux`) then reads
`.e2e/selinux/<distro>/check.log`. A failure to boot or to reach sshd is
reported by `run.sh` with the end of `console.log`; the overlay disk
`disk.qcow2` stays beside it until the next run.

Without docker or KVM, `check.sh` runs by hand as root on a Rocky virtual
machine (not a container), from a login session, with
[`receiver.py`](../testdata/selinux/receiver.py) beside it:

```sh
sh check.sh ./mailcrier_<version>_linux_amd64.rpm 2>&1 | tee check.log
go test -tags selinux -run '^TestSELinux$' . -args -selinux-log check.log
```

Nothing of the machine is lost: the exit trap removes the package, the
user, the hook and the cron entries, puts `smartd.conf` back and the
dontaudit rules on again, also after a failed step; the packages it
installs (at, cronie, smartmontools, s-nail, python3) and the updated
`selinux-policy` stay. Step 3 overwrites the configuration of the package,
step 1 restarts crond and atd (a crond started before `/usr/sbin/sendmail`
existed logs the output of jobs instead of mailing it), and step 9 lifts
`ConditionVirtualization=no` of the smartd unit through a drop-in in
`/run`. On a machine without a disk with SMART, smartd sends nothing, and
the test fails on it: record the output of step 9 and mark "smartd domain
not checked".

## What it checks

`check.sh` updates `selinux-policy` first, so the run checks the policy an
administrator gets on that day, and records its version. It turns the
dontaudit rules off (`semodule -DB`), so no denial hides, installs the rpm
after the start time, so its scriptlets are inside the search, and calls
mailcrier as root (`unconfined_t`), from a user and a system cron job, from
at, from the queue service after a failed delivery, and through mail from
smartd (`fsdaemon_t`), each delivering to a local receiver. The test fails
when:

- the script did not run in `unconfined_t`, SELinux is not enforcing, or
  crond, atd or smartd run outside `crond_t`, `atd_t` and `fsdaemon_t`;
- a caller did not deliver within its wait, or a call exited non-zero;
- the hook ran with the group `mailcrier`;
- no `MAC_POLICY_LOAD` record since the start was found, so an empty
  search would prove nothing;
- a denial names mailcrier, its paths or a caller of the check (comm
  `sendmail`, `exe` after the re-exec, `mail`, `s-nail`, `selinux-hook`).

A denial of any other process is printed by the test, not failed: with the
dontaudit rules off dnf, sshd and systemd give their own. A denial of
mailcrier holds the first release until it is decided how to answer it: a
policy module of our own, a `semanage fcontext` rule, or a documented
workaround; whether the binary is labelled `bin_t` or `sendmail_exec_t` is
part of what the run shows.

## Result

Not run yet. For each run: the Rocky version, the output of `rpm -q
mailcrier` and the commit the rpm was built from, the lines `policy`,
`cron_userdomain_transition` and `hook context` of `check.log`, the output
of steps 1 and 10, and the denials of other processes the test printed. No
host names, addresses or paths outside the virtual machine.
