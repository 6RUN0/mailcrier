# SELinux check

The rpm package installs `/usr/sbin/mailcrier` setgid and is called from
confined domains: user and system cron jobs, atd, smartd, the queue
service. A container cannot show whether the policy of the distribution
denies any of that, because the policy belongs to the kernel of the host.
This procedure runs the same callers on a virtual machine with SELinux
enforcing and collects the denials. The result goes into "Result" below,
one run per Rocky major version.

## Procedure

As root on a Rocky 9 or Rocky 10 virtual machine (not a container), with
the rpm of `dist/` copied to the current directory. Nothing of the machine
is lost: postfix stays installed, `smartd.conf` is saved and put back, the
dontaudit rules are switched back on; step 3 overwrites the configuration
of the package, and step 11 deletes the `.rpmsave` copy `rpm -e` leaves.
The daemons run as services, so they run in their own domains (`crond_t`,
`atd_t`, `fsdaemon_t`), and `ps -eZ` records each domain: an empty
`ausearch` proves nothing without them. The waits for the minute of cron
and for a message are done by watching the log of the receiver.

```sh
# 0. Environment; -DB turns the dontaudit rules off, so no denial hides
getenforce                      # Enforcing
sestatus
semodule -DB
# ausearch -ts takes the date in the format of the locale, as %x prints it;
# run step 10 in the same locale
start=$(date '+%x %H:%M:%S')

# 1. Install, labels, domains of the services
dnf install -y ./mailcrier_*_linux_amd64.rpm at cronie smartmontools s-nail python3
systemctl enable --now crond atd
ls -lZ /usr/sbin/mailcrier /usr/sbin/sendmail /etc/alternatives/mta \
  /etc/mailcrier.conf /var/spool/mailcrier /var/spool/mailcrier/*
matchpathcon /usr/sbin/mailcrier /var/spool/mailcrier
systemctl is-enabled mailcrier-queue.timer; systemctl is-active mailcrier-queue.timer
ps -eZ | grep -E ' (crond|atd)$'

# 2. Receiver on 127.0.0.1:80 (http_port_t) with a log of the requests
cat > /root/receiver.py <<'EOF'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def handle_one(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        print(self.command, self.path, body[:200], flush=True)
        self.send_response(200); self.end_headers(); self.wfile.write(b'{}')
    do_POST = do_PUT = handle_one
http.server.HTTPServer(('127.0.0.1', 80), H).serve_forever()
EOF
python3 /root/receiver.py > /root/receiver.log 2>&1 &
receiver=$!

# 3. Configuration: ntfy to the receiver; a hook, chosen by recipient,
#    writes its domain and groups
cat > /etc/mailcrier.conf <<'EOF'
[target.local]
type = "ntfy"
url = "http://127.0.0.1/selinux"

[target.hook]
type = "exec"
argv = ["/usr/local/bin/selinux-hook"]

[[route]]
recipient = "hook@example.org"
targets = ["hook"]

[[route]]
targets = ["local"]
EOF
printf '#!/bin/sh\n{ id -Z; id -G; } >> /tmp/selinux-hook.log\n' > /usr/local/bin/selinux-hook
chmod 0755 /usr/local/bin/selinux-hook
mailcrier --check-config

# 4. Call as root (unconfined_t)
printf 'Subject: root call\n\nbody\n' | /usr/sbin/sendmail -i root

# 5. Cron job of a user (cronjob_t); the message to the hook comes from a
#    file, so neither the crontab line nor the subject of the cron mail
#    names the address of the hook
useradd -m smoketest
printf 'To: hook@example.org\nSubject: cron hook\n\nx\n' > /home/smoketest/hook.eml
chown smoketest: /home/smoketest/hook.eml
printf '%s\n' '* * * * * echo user-cron-output' \
  '* * * * * /usr/sbin/sendmail -t < /home/smoketest/hook.eml' \
  | crontab -u smoketest -
# wait for user-cron-output in /root/receiver.log and a line from the hook:
grep -c user-cron-output /root/receiver.log; cat /tmp/selinux-hook.log
crontab -r -u smoketest

# 6. System cron job (system_cronjob_t)
printf '* * * * * root echo system-cron-output\n' > /etc/cron.d/selinux-smoke
# wait for system-cron-output in /root/receiver.log:
grep -c system-cron-output /root/receiver.log
rm /etc/cron.d/selinux-smoke

# 7. at (atd_t, a job of a user)
su - smoketest -c 'echo "echo at-output" | at now'
# wait for at-output in /root/receiver.log before step 8, or the mail of
# atd goes to the stopped receiver and into the queue:
grep -c at-output /root/receiver.log

# 8. Queue run by the service (unconfined_service_t)
kill "$receiver"
printf 'Subject: queued\n\nbody\n' | su - smoketest -c '/usr/sbin/sendmail -i root'
mailq
python3 /root/receiver.py >> /root/receiver.log 2>&1 &
receiver=$!
# 60 seconds or more after the call:
systemctl start mailcrier-queue.service
systemctl show -p Result mailcrier-queue.service
grep -c queued /root/receiver.log; mailq

# 9. Confined domain: smartd as a service (fsdaemon_t) mails through mail
cp -p /etc/smartmontools/smartd.conf /etc/smartmontools/smartd.conf.orig
echo 'DEVICESCAN -m root -M test' > /etc/smartmontools/smartd.conf
systemctl restart smartd
ps -eZ | grep ' smartd$'        # fsdaemon_t
# wait for the mail of smartd in /root/receiver.log:
grep -c -i smart /root/receiver.log
# without a device with SMART (smartd does not start or sends nothing),
# record the output of systemctl status smartd and mark "smartd domain
# not checked"; a SATA or IDE disk of QEMU has SMART

# 10. Denials
ausearch -m avc,user_avc,selinux_err -ts $start
ausearch -m avc -ts $start -c mailcrier

# 11. Clean up
mv /etc/smartmontools/smartd.conf.orig /etc/smartmontools/smartd.conf
systemctl restart smartd
semodule -B
kill "$receiver"; rpm -e mailcrier; userdel -r smoketest
rm -f /usr/local/bin/selinux-hook /tmp/selinux-hook.log /root/receiver.py \
  /etc/mailcrier.conf.rpmsave
```

Expected: step 10 prints `<no matches>` for both searches. A denial holds
the first release until it is decided how to answer it: a policy module of
our own, a `semanage fcontext` rule, or a documented workaround; whether
the binary is labelled `bin_t` or `sendmail_exec_t` is part of what the
run shows.

## Result

Not run yet. For each run: the Rocky version, the output of `sestatus` and
the output of steps 1, 5, 9 and 10. No host names, addresses or paths
outside the virtual machine.
