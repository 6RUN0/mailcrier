# SELinux

What the rpm package does about SELinux, what that changes for its
callers and its hooks, and the check that measures it on Rocky. The deb
and apk packages do nothing about SELinux.

## The module of the rpm

Without a policy of its own, `/usr/sbin/mailcrier` is `bin_t` and runs in
the domain of its caller. A confined caller loses the mail: smartd runs it
in `fsdaemon_t` (Rocky 9) or `smartdwarn_t` (Rocky 10), where neither the
spool nor the network is allowed. The rpm therefore ships
[`mailcrier.cil`](../packaging/selinux/mailcrier.cil) as
`/usr/share/selinux/packages/mailcrier/mailcrier.cil`, which gives
mailcrier the types of a stock MTA:

| Rule | Why |
|---|---|
| `/usr/sbin/mailcrier` and `/usr/bin/mailcrier` are `sendmail_exec_t` | a confined caller that may send mail moves into `system_mail_t`; Rocky 10 looks paths under `/usr/sbin` up as `/usr/bin` |
| `/var/spool/mailcrier(/.*)?` is `mqueue_spool_t` | `system_mail_t` and `sendmail_t` may not write `var_spool_t` |
| `init_t` may enter `sendmail_t` under NoNewPrivileges | `mailcrier-queue.service` runs with `NoNewPrivileges=yes` |
| `system_mail_t` may call `setpgid` | a hook runs in a process group of its own |

`%posttrans` installs the module at priority 200, the priority of modules
of packages, on every install and upgrade where `/etc/selinux/config`
exists, then relabels the binary and the spool, messages of an older
version included, when SELinux is enabled. Where `semodule` is missing or
fails it prints

```text
mailcrier: SELinux module not installed; to install it run: semodule -X 200 -i /usr/share/selinux/packages/mailcrier/mailcrier.cil
```

and the install goes on: the binary keeps `bin_t`, and confined callers
lose their mail as without the module. A host without
`/etc/selinux/config` (a container) gets nothing. `rpm -e` removes the
module at priority 200 only.

A module of the administrator named `mailcrier` at the default priority
400 replaces the one of the package instead of adding to it: name your own
rules otherwise, `mailcrier_local` below. To remove the module by hand,
`semodule -X 200 -r mailcrier` and then
`restorecon -RF /usr/sbin/mailcrier /var/spool/mailcrier`; mail from
confined callers is lost again from then on.

A spool directory other than `/var/spool/mailcrier` (`[spool] dir`) is not
covered by the module and has to be labelled like it
(`semanage` is in `policycoreutils-python-utils`):

```sh
semanage fcontext -a -e /var/spool/mailcrier /srv/mailcrier-spool
restorecon -R /srv/mailcrier-spool
```

## Domains of the callers

Measured on Rocky 9.8 (`selinux-policy-targeted-38.1.75-2.el9_8.1`) and
Rocky 10.2 (`selinux-policy-targeted-42.1.18-4.el10_2.4`) with the module
installed, by the check below; re-measured with every record under
"Result".

| Caller | Domain of mailcrier and of its hook |
|---|---|
| root in a shell, a user crontab | `unconfined_t`, as without the module |
| the mail crond sends with the output of a job, smartd | `system_mail_t` |
| `mailcrier-queue.service`, a service without NoNewPrivileges | `sendmail_t` |
| a service with `NoNewPrivileges=yes` | its own domain: the kernel refuses the transition, as for postfix, and the call works |

In `system_mail_t` and `sendmail_t` mailcrier reaches any TCP port, reads
the certificate authorities in `/etc/pki` and resolves names: measured with
http on ports 80 (`http_port_t`), 8080 (`http_cache_port_t`) and 2586
(`unreserved_port_t`) and with https by a name from `/etc/hosts`.

Changing the domain resets the signal dispositions of the caller (the
`siginh` permission is not granted between domains, the policy only
hides the denial): a call from a caller that ignores SIGHUP, as `nohup`
does, does not ignore it in mailcrier. The same holds for every MTA
labelled `sendmail_exec_t`.

### Hooks

A hook (`type = "exec"`) runs in the domain of the call that delivers, so
the same hook may work from a shell and fail when cron or smartd sends the
message, or when the queue service retries it. In `system_mail_t` and
`sendmail_t` it may use the network and run programs labelled `bin_t`
(`/usr/bin`, `/usr/local/bin`): a hook running curl to an http receiver
was measured from every caller of the table. The stock policy of these
domains does not let it write files outside the mail spools (`/var/log`,
`/home`, `/srv`), manage services or reach the sockets of other services;
that part was not measured hook by hook.

A hook refused there fails its delivery: the log line names the target
(`target failed ... target=<name> ... err="permanent failure: hook
..."`), and the audit log has the denial with the `comm` of the
program the hook ran. To let it through, write a module of your own from
those denials, as root, after the hook failed from cron or smartd:

```sh
ausearch -m avc -ts recent -c <program> --raw | audit2allow -M mailcrier_local
semodule -i mailcrier_local.pp
```

`audit2allow` is in `policycoreutils-python-utils`. Check what the module
allows before installing it (`mailcrier_local.te`): a rule for
`system_mail_t` holds for every mail from a confined caller, not only for
mailcrier.

## The check

The check runs the callers on a virtual machine with SELinux enforcing and
collects the denials; a container cannot show them, because the policy
belongs to the kernel of the host. Its result goes into "Result" below,
one run per Rocky major version.

### Running it

```sh
make snapshot
make -j2 selinux-rocky9 selinux-rocky10
```

It needs docker and a readable and writable `/dev/kvm` on an x86-64-v3
host (Rocky 10), and network: the first run fetches the cloud images of
Rocky into `.e2e/selinux` (about 600 MB each, checked against the sums in
the `Makefile`) and the packages of the release `SMOKE_PREVIOUS` (`make
smoke-previous`), and the virtual machine installs its packages from the
mirrors of Rocky. Each target boots the image under qemu in the container
of `testdata/selinux`, with a throwaway overlay disk, an IDE disk with
SMART for smartd and 2 GB of memory, logs in as root over ssh, runs
[`check.sh`](../testdata/selinux/check.sh) on the rpm of `dist/` and the
rpm of the previous release and powers the machine off; a run takes 15 to
20 minutes. `TestSELinux` (`selinux_test.go`, tag `selinux`) then reads
`.e2e/selinux/<distro>/check.log`. A failure to boot or to reach sshd is
reported by `run.sh` with the end of `console.log`; the overlay disk
`disk.qcow2` stays beside it until the next run.

Without docker or KVM, `check.sh` runs by hand as root on a Rocky virtual
machine (not a container), from a login session, with
[`receiver.py`](../testdata/selinux/receiver.py) beside it:

```sh
sh check.sh ./mailcrier_<version>_linux_amd64.rpm \
  ./mailcrier_<previous>_linux_amd64.rpm 2>&1 | tee check.log
go test -tags selinux -run '^TestSELinux$' . -args -selinux-log check.log
```

It changes the machine: it adds a user `smoketest`, a certificate
authority to `/etc/pki/ca-trust` and `selinux.test` to `/etc/hosts`,
overwrites the configuration of the package, and leaves at, cronie,
smartmontools, s-nail, python3, openssl and the updated `selinux-policy`
installed. The exit trap removes mailcrier, postfix, the user, the hook
and the cron entries, puts `smartd.conf` back and the dontaudit rules on
again, also after a failed step. It lifts `ConditionVirtualization=no` of
the smartd unit through a drop-in in `/run`, and restarts crond and atd
after each change of the MTA (a crond started before `/usr/sbin/sendmail`
existed logs the output of jobs instead of mailing it). On a machine
without a disk with SMART, smartd sends nothing, and the test fails on it.

### What it checks

`check.sh` updates `selinux-policy` first, so the run checks the policy an
administrator gets on that day, and records its version. It turns the
dontaudit rules off (`semodule -DB`), so no denial hides, then:

1. runs the mail of crond for a user and a system job, smartd and two
   transient services, with and without NoNewPrivileges, through postfix;
   its denials are the measure of what the stock policy gives any MTA;
2. installs the previous release and queues a message;
3. upgrades to the rpm under test, whose `%posttrans` installs the module;
4. calls mailcrier as root, from a user crontab, from the mail of crond,
   from at, from smartd and from both services, each to four targets
   (http on 80, 8080 and 2586, https by name on 8443) and to the hook;
5. lets the queue service deliver the message of the previous release and
   one queued while the receiver was down;
6. removes the package and looks for the module.

The test fails when:

- the script did not run in `unconfined_t`, SELinux is not enforcing, or
  crond, atd or smartd run outside `crond_t` (atd too on Rocky, whose
  policy labels it `crond_exec_t`), `atd_t` and `fsdaemon_t`;
- postfix or mailcrier did not deliver a message within its wait, or a
  call exited non-zero;
- the binary, the spool or the queued message of the previous release is
  not labelled as above, the module is not at priority 200, or it is left
  after the removal;
- the hook ran in another domain than the table above shows, or with the
  group `mailcrier`;
- no `MAC_POLICY_LOAD` record since the start was found, so an empty
  search would prove nothing;
- a denial names mailcrier, its paths or a caller of the check (comm
  `sendmail`, `exe` after the re-exec, `mail`, `s-nail`, `selinux-hook`,
  `curl`) and postfix did not get the same denial (the same types of
  source and target, class and permission) in step 1.

A denial of any other process, and one postfix gets as well, is printed by
the test, not failed: with the dontaudit rules off, dnf, sshd and systemd
give their own, and the transition into `system_mail_t` gives
`noatsecure`, `rlimitinh` and `siginh` for any MTA.

## Result

For each run: the Rocky version, the output of `rpm -q mailcrier` and the
commit the rpm was built from, the lines `policy`,
`cron_userdomain_transition`, `module` and `hook` of `check.log`, and the
denials the test printed. No host names, addresses or paths outside the
virtual machine.

### 960edf2, 2026-10-09

`make -j2 selinux-rocky9 selinux-rocky10` passed on both, every one of the
48 waits `ok`; the rpm `mailcrier-0.1.0~rc1791525644-1.x86_64`, upgraded
from `mailcrier-0.1.0~rc.2-1.x86_64`, module `200 cil`, absent after
`rpm -e`; `cron_userdomain_transition on`.

| | Rocky 9.8 | Rocky 10.2 |
|---|---|---|
| `policy` | `selinux-policy-targeted-38.1.75-2.el9_8.1` | `selinux-policy-targeted-42.1.18-4.el10_2.4` |
| crond, atd | `crond_t` | `crond_t` |
| smartd | `fsdaemon_t` | `fsdaemon_t` |

The hook ran as in the table of "Domains of the callers" on both: the
mail of crond and smartd in `system_mail_t`, the queue service (group
`mailcrier`) and the service without NoNewPrivileges in `sendmail_t`, the
service with it in `initrc_t`, root and the user crontab in
`unconfined_t`.

Denials of mailcrier and its callers, each one postfix got as well in step
1:

| Source -> target | Class and permission | Rocky |
|---|---|---|
| `fsdaemon_t` -> `system_mail_t` | `process` `noatsecure`, `rlimitinh`, `siginh` | 9 |
| `smartdwarn_t` -> `system_mail_t` | `process` `noatsecure`, `rlimitinh`, `siginh` | 10 |
| `system_mail_t` -> `init_t` | `unix_stream_socket` `read write` (stdout of crond, comm `sendmail`) | 9, 10 |
| `initrc_t` -> `sendmail_t` | `process2` `nnp_transition` (service with NoNewPrivileges) | 9, 10 |
| `smartdwarn_t` -> `fs_t` | `filesystem` `getattr` (comm `mail`, s-nail) | 10 |

Denials of other processes, with the dontaudit rules off: `setroubleshootd_t`
writing `rpm_var_lib_t` (comm `rpm`), `systemd_logind_t` (9) or
`systemd_user_runtimedir_t` (10) asking for `net_admin`, `init_t` reading
`shadow_t` (9), `systemd_gpt_generator_t` asking for `sys_admin` and
`kdump_dep_generator_t` reading `passwd_file_t` (10).
