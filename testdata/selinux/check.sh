#!/bin/sh
# The SELinux check of docs/selinux.md: run as root from a login session
# (ssh or console, so in unconfined_t) on a Rocky virtual machine with
# SELinux enforcing, with receiver.py beside this script:
#   sh check.sh ./mailcrier_<version>_linux_amd64.rpm
# Lines "== ... ==" are the record selinux_test.go reads; the rest is for a
# person. Steps 4 to 10 run whatever an earlier one gave, the exit status is
# not 0 only when the script itself could not go on. The exit trap puts the
# machine back as it was (step 11).

# ausearch -ts takes the date in the format of the locale, as %x prints it.
# ausearch reads stdin when it is not a terminal, as under ssh, so every
# search names --input-logs.
export LC_ALL=C

rpm=$1
here=$(cd "$(dirname "$0")" && pwd)
receiver_pid=
hook_fifo=/tmp/selinux-hook.fifo
smartd_dropin=/run/systemd/system/smartd.service.d

case $rpm in
*.rpm) [ -f "$rpm" ] || { echo "no such file: $rpm" >&2; exit 2; } ;;
*) echo "usage: sh check.sh <mailcrier rpm>" >&2; exit 2 ;;
esac
case $rpm in /*) ;; *) rpm=$PWD/$rpm ;; esac

mark() { echo "== $* =="; }

# must stops the check when a step it depends on fails.
must() {
	"$@" || { mark "failed: $*"; exit 1; }
}

# wait_receiver waits up to $3 seconds for a request carrying $2.
wait_receiver() {
	if curl -fsS -o /dev/null --max-time "$(($3 + 10))" \
		"http://127.0.0.1/wait?for=$2&timeout=$3"; then
		mark "wait $1: ok"
	else
		mark "wait $1: timeout"
	fi
}

start_receiver() {
	python3 "$here/receiver.py" >> /root/receiver.log 2>&1 &
	receiver_pid=$!
	# The receiver is up once it answers; it starts within a second.
	i=0
	until curl -s -o /dev/null --max-time 1 http://127.0.0.1/; do
		i=$((i + 1))
		[ $i -lt 30 ] || { mark "failed: receiver did not start"; exit 1; }
		sleep 1
	done
}

stop_receiver() {
	[ -n "$receiver_pid" ] || return 0
	kill "$receiver_pid"
	wait "$receiver_pid" 2>/dev/null
	receiver_pid=
}

cleanup() {
	mark "step 11"
	stop_receiver
	crontab -r -u smoketest 2>/dev/null
	rm -f /etc/cron.d/selinux-smoke
	if [ -f /etc/smartmontools/smartd.conf.orig ]; then
		mv /etc/smartmontools/smartd.conf.orig /etc/smartmontools/smartd.conf
		rm -rf "$smartd_dropin"
		systemctl daemon-reload
		systemctl restart smartd
	fi
	semodule -B
	rpm -q mailcrier >/dev/null 2>&1 && rpm -e mailcrier
	id smoketest >/dev/null 2>&1 && userdel -r smoketest
	rm -f /usr/local/bin/selinux-hook "$hook_fifo" /root/hook.out /etc/mailcrier.conf.rpmsave
}

mark "step 0"
must dnf install -y at cronie smartmontools s-nail python3 curl audit policycoreutils
must dnf upgrade -y 'selinux-policy*'
mark "context $(id -Z)"
mark "enforce $(getenforce)"
sestatus
mark "auditd $(systemctl is-active auditd)"
mark "policy $(rpm -q selinux-policy-targeted)"
mark "cron_userdomain_transition $(getsebool cron_userdomain_transition | awk '{print $NF}')"
start=$(date '+%x %H:%M:%S')
start_date=${start% *}
start_time=${start#* }
trap cleanup EXIT
trap 'exit 1' INT TERM HUP
# -DB turns the dontaudit rules off, so no denial hides; its policy load
# is the record step 10 must find to trust an empty search.
must semodule -DB

mark "step 1"
must dnf install -y "$rpm"
mark "package $(rpm -q mailcrier)"
must systemctl enable crond atd
# crond started without /usr/sbin/sendmail logs the output of jobs instead
# of mailing it until it starts again.
must systemctl restart crond atd
ls -lZ /usr/sbin/mailcrier /usr/sbin/sendmail /etc/alternatives/mta \
	/etc/mailcrier.conf /var/spool/mailcrier /var/spool/mailcrier/*
matchpathcon /usr/sbin/mailcrier /var/spool/mailcrier
mark "timer $(systemctl is-enabled mailcrier-queue.timer) $(systemctl is-active mailcrier-queue.timer)"
# A run of the timer while the receiver of step 8 is down would double the
# backoff of the queued message.
must systemctl stop mailcrier-queue.timer
for daemon in crond atd; do
	mark "domain $daemon $(ps -eZ | awk -v d="$daemon" '$NF == d { print $1; exit }')"
done
mark "mailcrier gid $(getent group mailcrier | cut -d: -f3)"

mark "step 2"
start_receiver

mark "step 3"
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
# The hook writes its domain and groups into a FIFO step 5 reads, so the
# wait ends when it ran.
rm -f "$hook_fifo"
must mkfifo -m 0666 "$hook_fifo"
printf '#!/bin/sh\n{ id -Z; id -G; } > %s\n' "$hook_fifo" > /usr/local/bin/selinux-hook
chmod 0755 /usr/local/bin/selinux-hook
must mailcrier --check-config

mark "step 4"
printf 'Subject: selinux-root-call\n\nbody\n' | /usr/sbin/sendmail -i root
mark "call root: exit $?"
wait_receiver root selinux-root-call 30

mark "step 5"
must useradd -m smoketest
printf 'To: hook@example.org\nSubject: cron hook\n\nx\n' > /home/smoketest/hook.eml
chown smoketest: /home/smoketest/hook.eml
# The message to the hook comes from a file, so neither the crontab line
# nor the subject of the cron mail names the address of the hook.
printf '%s\n' '* * * * * echo user-cron-output' \
	'* * * * * /usr/sbin/sendmail -t < /home/smoketest/hook.eml' |
	crontab -u smoketest -
# The hook blocks on the FIFO until it is read: a reader started after the
# wait below would leave it to the deadline of the call.
timeout 180 cat "$hook_fifo" > /root/hook.out &
hook_reader=$!
wait_receiver user-cron user-cron-output 180
if wait "$hook_reader"; then
	mark "wait hook: ok"
	cat /root/hook.out
	mark "hook context $(sed -n 1p /root/hook.out)"
	mark "hook groups $(sed -n 2p /root/hook.out)"
else
	mark "wait hook: timeout"
fi
crontab -r -u smoketest
# Calls of the next minute would block on the FIFO and stay in the queue of
# step 8.
printf '#!/bin/sh\nexit 0\n' > /usr/local/bin/selinux-hook

mark "step 6"
printf '* * * * * root echo system-cron-output\n' > /etc/cron.d/selinux-smoke
wait_receiver system-cron system-cron-output 180
rm -f /etc/cron.d/selinux-smoke

mark "step 7"
su - smoketest -c 'echo "echo at-output" | at now'
# Before step 8, or the mail of atd goes to the stopped receiver.
wait_receiver at at-output 120

mark "step 8"
stop_receiver
printf 'Subject: selinux-queued\n\nbody\n' | su - smoketest -c '/usr/sbin/sendmail -i root'
mark "call queued: exit $?"
mailq
start_receiver
# The queued message is due 60 seconds after the call; a run before that
# sends nothing, so the service starts again until the message arrives.
i=0
while :; do
	systemctl start mailcrier-queue.service
	if curl -fsS -o /dev/null --max-time 30 'http://127.0.0.1/wait?for=selinux-queued&timeout=20'; then
		mark "wait queued: ok"
		break
	fi
	i=$((i + 1))
	[ $i -lt 9 ] || { mark "wait queued: timeout"; break; }
done
mark "queue service $(systemctl show -p Result --value mailcrier-queue.service)"
mailq

mark "step 9"
smartctl -i /dev/sda
if smartctl -i /dev/sda | grep -q 'SMART support is: Available'; then
	mark "smart available"
else
	mark "smart unavailable"
fi
cp -p /etc/smartmontools/smartd.conf /etc/smartmontools/smartd.conf.orig
echo 'DEVICESCAN -m root -M test' > /etc/smartmontools/smartd.conf
# The unit of smartd skips a virtual machine (ConditionVirtualization=no).
mkdir -p "$smartd_dropin"
printf '[Unit]\nConditionVirtualization=\n' > "$smartd_dropin/selinux-check.conf"
systemctl daemon-reload
systemctl restart smartd
mark "domain smartd $(ps -eZ | awk '$NF == "smartd" { print $1; exit }')"
wait_receiver smartd EmailTest 120

mark "step 10"
if ausearch --input-logs -m MAC_POLICY_LOAD -ts "$start_date" "$start_time" >/dev/null 2>&1; then
	mark "policy load found"
else
	mark "policy load missing"
fi
mark "denials"
ausearch --input-logs -i -m avc,user_avc,selinux_err -ts "$start_date" "$start_time" 2>&1
mark "denials of mailcrier"
ausearch --input-logs -i -m avc -ts "$start_date" "$start_time" -x /usr/sbin/mailcrier 2>&1
mark "end"
