#!/bin/sh
# The SELinux check of docs/selinux.md: run as root from a login session
# (ssh or console, so in unconfined_t) on a Rocky virtual machine with
# SELinux enforcing, with receiver.py beside this script:
#   sh check.sh ./mailcrier_<version>_linux_amd64.rpm ./mailcrier_<previous>_linux_amd64.rpm
# Lines "== ... ==" are the record selinux_test.go reads; the rest is for a
# person. The steps run whatever an earlier one gave, the exit status is not
# 0 only when the script itself could not go on.
#
# Step 1 runs the callers with postfix as the MTA: its denials are what the
# stock policy gives any MTA with these callers, the measure for those of
# mailcrier. Step 2 installs the previous release and queues a message,
# step 3 upgrades to the rpm under test, whose %posttrans installs the
# SELinux module, then the callers deliver to four targets (http on 80,
# 8080 and 2586, https by name on 8443) and the hook, which reports its
# domain and groups to the receiver.

# ausearch -ts takes the date in the format of the locale, as %x prints it.
# ausearch reads stdin when it is not a terminal, as under ssh, so every
# search names --input-logs.
export LC_ALL=C

rpm=$1
previous=$2
here=$(cd "$(dirname "$0")" && pwd)
receiver_pid=
smartd_dropin=/run/systemd/system/smartd.service.d
ports="80 8080 2586 8443"

for file in "$rpm" "$previous"; do
	case $file in
	*.rpm) [ -f "$file" ] || { echo "no such file: $file" >&2; exit 2; } ;;
	*) echo "usage: sh check.sh <mailcrier rpm> <previous mailcrier rpm>" >&2; exit 2 ;;
	esac
done
case $rpm in /*) ;; *) rpm=$PWD/$rpm ;; esac
case $previous in /*) ;; *) previous=$PWD/$previous ;; esac

mark() { echo "== $* =="; }

# must stops the check when a step it depends on fails.
must() {
	"$@" || { mark "failed: $*"; exit 1; }
}

now() { date '+%x %H:%M:%S'; }

start_receiver() {
	python3 "$here/receiver.py" /root/tls/server.pem /root/tls/server.key >> /root/receiver.log 2>&1 &
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

# wait_request name seconds text...: waits for one request that carries
# every text.
wait_request() {
	name=$1
	seconds=$2
	shift 2
	query=
	for text in "$@"; do query="$query&for=$text"; done
	if curl -fsS -o /dev/null --max-time "$((seconds + 10))" \
		"http://127.0.0.1/wait?timeout=$seconds$query"; then
		mark "wait $name: ok"
	else
		mark "wait $name: timeout"
	fi
}

# wait_targets caller text seconds: the message on each target.
wait_targets() {
	for port in $ports; do
		wait_request "$1 $port" "$3" "port${port}x" "$2"
	done
}

# wait_mailbox name file text seconds: a message postfix delivered locally.
wait_mailbox() {
	i=0
	until grep -qs "$3" "$2"; do
		i=$((i + 2))
		[ $i -lt "$4" ] || { mark "wait $1: timeout"; return; }
		sleep 2
	done
	mark "wait $1: ok"
}

# denials title start end: the audit records of the window.
denials() {
	mark "$1"
	# The dates are "<date> <time>", two words each.
	# shellcheck disable=SC2086
	ausearch --input-logs -i -m avc,user_avc,selinux_err -ts $2 ${3:+-te $3} 2>&1
}

# unit_call name nnp [recipient]: a message from a transient service with
# NoNewPrivileges=<nnp>, recording the domain of the service and the exit
# status of the call.
unit_call() {
	systemd-run --wait -q -p "NoNewPrivileges=$2" /bin/sh -c "id -Z > /root/$1.ctx
printf 'Subject: $1\\n\\n$1\\n' | /usr/sbin/sendmail -i root ${3:-}
echo \$? >> /root/$1.ctx"
	mark "unit $1 $(tr '\n' ' ' < "/root/$1.ctx")"
}

cleanup() {
	mark "cleanup"
	stop_receiver
	crontab -r -u smoketest 2>/dev/null
	rm -f /etc/cron.d/selinux-*
	if [ -f /etc/smartmontools/smartd.conf.orig ]; then
		mv /etc/smartmontools/smartd.conf.orig /etc/smartmontools/smartd.conf
		rm -rf "$smartd_dropin"
		systemctl daemon-reload
		systemctl restart smartd
	fi
	semodule -B
	rpm -q mailcrier >/dev/null 2>&1 && rpm -e mailcrier
	rpm -q postfix >/dev/null 2>&1 && dnf remove -y -q postfix
	id smoketest >/dev/null 2>&1 && userdel -r smoketest
	rm -f /usr/local/bin/selinux-hook /etc/mailcrier.conf.rpmsave
}

mark "step 0"
must dnf install -y at cronie smartmontools s-nail python3 curl audit policycoreutils postfix openssl
must dnf upgrade -y 'selinux-policy*'
mark "context $(id -Z)"
mark "enforce $(getenforce)"
sestatus
mark "auditd $(systemctl is-active auditd)"
mark "policy $(rpm -q selinux-policy-targeted)"
mark "cron_userdomain_transition $(getsebool cron_userdomain_transition | awk '{print $NF}')"
# The https target checks a name and a certificate authority of the host,
# so the domain of the call must read /etc/hosts and /etc/pki.
mkdir -p /root/tls
openssl req -x509 -newkey rsa:2048 -nodes -keyout /root/tls/ca.key -out /root/tls/ca.pem \
	-days 2 -subj /CN=selinux-check-ca 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout /root/tls/server.key -out /root/tls/server.csr \
	-subj /CN=selinux.test 2>/dev/null
printf 'subjectAltName=DNS:selinux.test\n' > /root/tls/ext
must openssl x509 -req -in /root/tls/server.csr -CA /root/tls/ca.pem -CAkey /root/tls/ca.key \
	-CAcreateserial -out /root/tls/server.pem -days 2 -extfile /root/tls/ext
cp /root/tls/ca.pem /etc/pki/ca-trust/source/anchors/selinux-check-ca.pem
must update-ca-trust
grep -q selinux.test /etc/hosts || echo '127.0.0.1 selinux.test' >> /etc/hosts
must useradd -m smoketest
# The unit of smartd skips a virtual machine (ConditionVirtualization=no).
mkdir -p "$smartd_dropin"
printf '[Unit]\nConditionVirtualization=\n' > "$smartd_dropin/selinux-check.conf"
cp -p /etc/smartmontools/smartd.conf /etc/smartmontools/smartd.conf.orig
systemctl daemon-reload
must systemctl enable crond atd
start=$(now)
trap cleanup EXIT
trap 'exit 1' INT TERM HUP
# -DB turns the dontaudit rules off, so no denial hides; its policy load
# is the record the last step must find to trust an empty search.
must semodule -DB

mark "step 1"
control_start=$(now)
must alternatives --set mta /usr/sbin/sendmail.postfix
must systemctl start postfix
# crond started without /usr/sbin/sendmail logs the output of jobs instead
# of mailing it until it starts again.
must systemctl restart crond atd
for daemon in crond atd; do
	mark "domain $daemon $(ps -eZ | awk -v d="$daemon" '$NF == d { print $1; exit }')"
done
echo '* * * * * echo control-user-cron' | crontab -u smoketest -
printf '* * * * * root echo control-system-cron\n' > /etc/cron.d/selinux-control
echo 'DEVICESCAN -m root -M test' > /etc/smartmontools/smartd.conf
systemctl restart smartd
unit_call control-nnp yes
unit_call control-service no
wait_mailbox "control user-cron" /var/spool/mail/smoketest control-user-cron 180
wait_mailbox "control system-cron" /var/spool/mail/root control-system-cron 180
wait_mailbox "control smartd" /var/spool/mail/root EmailTest 60
wait_mailbox "control nnp" /var/spool/mail/root control-nnp 30
wait_mailbox "control service" /var/spool/mail/root control-service 30
crontab -r -u smoketest
rm -f /etc/cron.d/selinux-control
denials "control denials" "$control_start" "$(now)"
systemctl stop postfix
must dnf remove -y -q postfix

mark "step 2"
denials_start=$(now)
must dnf install -y "$previous"
mark "previous $(rpm -q mailcrier)"
mark "timer $(systemctl is-enabled mailcrier-queue.timer) $(systemctl is-active mailcrier-queue.timer)"
# A run of the timer while the receiver is down would double the backoff
# of a queued message; the upgrade leaves the timer as it is.
must systemctl stop mailcrier-queue.timer
cat > /etc/mailcrier.conf <<'EOF'
[target.p80]
type = "ntfy"
url = "http://127.0.0.1/p80"

[target.p8080]
type = "ntfy"
url = "http://127.0.0.1:8080/p8080"

[target.p2586]
type = "ntfy"
url = "http://127.0.0.1:2586/p2586"

[target.p8443]
type = "ntfy"
url = "https://selinux.test:8443/p8443"

[target.hook]
type = "exec"
argv = ["/usr/local/bin/selinux-hook"]

[[route]]
recipient = "hook@example.org"
targets = ["hook"]

[[route]]
targets = ["p80", "p8080", "p2586", "p8443"]
EOF
# The hook reports its domain and groups with the subject it got; it asks
# for a retry while the receiver is down.
cat > /usr/local/bin/selinux-hook <<'EOF'
#!/bin/sh
subject=$(sed -n 's/^Subject: //p' | head -n 1 | tr ' ' _)
curl -fsS --max-time 10 -o /dev/null \
	--data-binary "hook-done subject=$subject context=$(id -Z) groups=$(id -G | tr ' ' ,)" \
	http://127.0.0.1/hook || exit 75
EOF
chmod 0755 /usr/local/bin/selinux-hook
printf 'Subject: upgrade-queued\n\nupgrade-queued\n' | su - smoketest -c '/usr/sbin/sendmail -i root'
mark "call upgrade: exit $?"

mark "step 3"
must dnf upgrade -y "$rpm"
mark "package $(rpm -q mailcrier)"
ls -lZ /usr/sbin/mailcrier /usr/sbin/sendmail /etc/alternatives/mta \
	/etc/mailcrier.conf /var/spool/mailcrier /var/spool/mailcrier/* /var/spool/mailcrier/queue/*
matchpathcon /usr/sbin/mailcrier /usr/bin/mailcrier /var/spool/mailcrier
mark "label binary $(stat -c %C /usr/sbin/mailcrier)"
mark "label spool $(stat -c %C /var/spool/mailcrier/queue)"
for entry in /var/spool/mailcrier/queue/*.eml; do
	mark "label entry $(stat -c %C "$entry")"
	break
done
semodule -lfull | grep mailcrier
mark "module $(semodule -lfull | awk '$2 == "mailcrier" { print $1, $3; exit }')"
mark "timer after upgrade $(systemctl is-enabled mailcrier-queue.timer)"
must systemctl restart crond atd
mark "mailcrier gid $(getent group mailcrier | cut -d: -f3)"
must mailcrier --check-config
start_receiver

mark "step 4"
printf 'Subject: root-call\n\nroot-call\n' | /usr/sbin/sendmail -i root hook@example.org
mark "call root: exit $?"
printf 'To: hook@example.org\nSubject: user-crontab-hook\n\nx\n' > /home/smoketest/hook.eml
chown smoketest: /home/smoketest/hook.eml
# The message to the hook comes from a file, so neither the crontab line
# nor the subject of the cron mail names the address of the hook.
printf '%s\n' '* * * * * echo user-cron-output' \
	'* * * * * /usr/sbin/sendmail -t < /home/smoketest/hook.eml' |
	crontab -u smoketest -
printf '* * * * * root echo system-cron-output\n' > /etc/cron.d/selinux-system
printf 'MAILTO=hook@example.org\n* * * * * root echo cron-mail-hook\n' > /etc/cron.d/selinux-hook
su - smoketest -c 'echo "echo at-output" | at now'
smartctl -i /dev/sda
if smartctl -i /dev/sda | grep -q 'SMART support is: Available'; then
	mark "smart available"
else
	mark "smart unavailable"
fi
echo 'DEVICESCAN -m root,hook@example.org -M test' > /etc/smartmontools/smartd.conf
systemctl restart smartd
mark "domain smartd $(ps -eZ | awk '$NF == "smartd" { print $1; exit }')"
unit_call nnp-call yes hook@example.org
unit_call service-call no hook@example.org
wait_targets root root-call 30
wait_targets service service-call 30
wait_targets nnp nnp-call 30
wait_targets smartd EmailTest 60
wait_targets user-cron user-cron-output 180
wait_targets system-cron system-cron-output 180
wait_targets at at-output 120
wait_request "hook root" 30 hook-done subject=root-call
wait_request "hook service" 30 hook-done subject=service-call
wait_request "hook nnp" 30 hook-done subject=nnp-call
wait_request "hook smartd" 60 hook-done EmailTest
wait_request "hook user-crontab" 180 hook-done subject=user-crontab-hook
wait_request "hook cron-mail" 180 hook-done cron-mail-hook
crontab -r -u smoketest
rm -f /etc/cron.d/selinux-system /etc/cron.d/selinux-hook

mark "step 5"
stop_receiver
printf 'Subject: queued-call\n\nqueued-call\n' | su - smoketest -c '/usr/sbin/sendmail -i root hook@example.org'
mark "call queued: exit $?"
mailq
start_receiver
# A queued message is due 60 seconds after its call; a run before that
# sends nothing, so the service starts again until both messages arrived.
i=0
while :; do
	systemctl start mailcrier-queue.service
	if curl -fsS -o /dev/null --max-time 30 'http://127.0.0.1/wait?for=port80x&for=queued-call&timeout=20' &&
		curl -fsS -o /dev/null --max-time 5 'http://127.0.0.1/wait?for=port80x&for=upgrade-queued&timeout=1'; then
		break
	fi
	i=$((i + 1))
	[ $i -lt 9 ] || break
done
wait_targets queued queued-call 5
wait_targets upgrade upgrade-queued 5
wait_request "hook queued" 30 hook-done subject=queued-call
mark "queue service $(systemctl show -p Result --value mailcrier-queue.service)"
mailq
grep hook-done /root/receiver.log | sort -u | while read -r _ _ _ _ subject context groups; do
	mark "hook ${subject#subject=} ${context#context=} ${groups#groups=}"
done

mark "step 6"
end=$(now)
stop_receiver
rpm -e mailcrier
if semodule -lfull | awk '$2 == "mailcrier"' | grep -q .; then
	mark "module after erase: present"
else
	mark "module after erase: absent"
fi
# shellcheck disable=SC2086
if ausearch --input-logs -m MAC_POLICY_LOAD -ts $start >/dev/null 2>&1; then
	mark "policy load found"
else
	mark "policy load missing"
fi
denials "denials" "$denials_start" "$end"
mark "denials of mailcrier"
# shellcheck disable=SC2086
ausearch --input-logs -i -m avc -ts $denials_start -te $end -x /usr/sbin/mailcrier 2>&1
mark "end"
