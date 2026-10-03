#!/bin/sh
# The prerm-systemd-restart fragment of debhelper 13.24.2: the timer stops
# on removal only, an upgrade leaves it running. Removal then deletes the
# queue run locks of a spool without messages, so that dpkg can remove the
# empty spool; a spool with messages stays as it is.
set -e

if [ -z "$DPKG_ROOT" ] && [ "$1" = remove ] && [ -d /run/systemd/system ] ; then
	deb-systemd-invoke stop mailcrier-queue.timer >/dev/null || true
fi

if [ -z "$DPKG_ROOT" ] && [ "$1" = remove ]; then
	has_mail=
	for f in /var/spool/mailcrier/queue/*.eml /var/spool/mailcrier/hold/*.eml \
		/var/spool/mailcrier/failed/*.eml; do
		if [ -e "$f" ]; then has_mail=yes; fi
	done
	if [ -z "$has_mail" ]; then
		rm -f /var/spool/mailcrier/locks/*.lock || true
	fi
fi
