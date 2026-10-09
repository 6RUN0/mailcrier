#!/bin/sh
# Runs on install ($1 = 1) and upgrade ($1 = 2): rpm unpacks the binary as
# root:root 0755 every time. chgrp comes first because it clears the setgid
# bit. Priority 100 of the mta alternative is above postfix (60) and sendmail
# (90), so the installed mailcrier becomes the active one.
set -e

chgrp mailcrier /usr/sbin/mailcrier
chmod 2755 /usr/sbin/mailcrier
alternatives --install /usr/sbin/sendmail mta /usr/sbin/mailcrier 100 \
	--slave /usr/bin/mailq mta-mailq /usr/sbin/mailcrier \
	--slave /usr/bin/newaliases mta-newaliases /usr/sbin/mailcrier \
	--slave /usr/lib/sendmail mta-sendmail /usr/sbin/mailcrier

# enable, not preset: the preset policy of the distribution turns the timer
# off, and the queue would never be run.
if [ "$1" = 1 ] && command -v systemctl >/dev/null; then
	systemctl enable mailcrier-queue.timer >/dev/null || true
	if [ -d /run/systemd/system ]; then
		systemctl start mailcrier-queue.timer >/dev/null || true
	fi
fi
# cronie decides at its start whether it mails the output of jobs, and
# without a sendmail it logs the output from then on.
if [ "$1" = 1 ]; then
	if [ -d /run/systemd/system ]; then
		systemctl --quiet is-active crond && crond_running=yes
	elif [ -e /run/crond.pid ]; then
		crond_running=yes
	fi
	if [ -n "${crond_running:-}" ]; then
		echo "mailcrier: restart crond if it started before any MTA was installed, or it logs the output of jobs instead of mailing it"
	fi
fi
if [ "$1" = 2 ] && [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null || true
fi
