#!/bin/sh
# The postrm-systemd-reload-only and postrm-systemd fragments of debhelper
# 13.24.2; purge drops the setgid override. Removal and purge name a spool
# that still holds messages. Group and user stay.
set -e

if [ "$1" = remove ] && [ -d /run/systemd/system ] ; then
	systemctl --system daemon-reload >/dev/null || true
fi

if [ "$1" = "purge" ]; then
	if [ -x "/usr/bin/deb-systemd-helper" ]; then
		deb-systemd-helper purge mailcrier-queue.timer >/dev/null || true
	fi
fi

if [ "$1" = "purge" ]; then
	dpkg-statoverride --quiet --remove /usr/sbin/mailcrier || true
fi

if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
	has_mail=
	for f in /var/spool/mailcrier/queue/*.eml /var/spool/mailcrier/hold/*.eml \
		/var/spool/mailcrier/failed/*.eml; do
		if [ -e "$f" ]; then has_mail=yes; fi
	done
	if [ -n "$has_mail" ]; then
		echo "mailcrier: /var/spool/mailcrier is left in place, it holds messages"
	fi
fi
