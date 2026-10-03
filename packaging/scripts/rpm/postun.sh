#!/bin/sh
# Acts on erase only ($1 = 0) and names a spool that still holds messages.
# Configuration, group and user stay.
set -e

if [ "$1" = 0 ]; then
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload >/dev/null || true
	fi
	has_mail=
	for f in /var/spool/mailcrier/queue/*.eml /var/spool/mailcrier/hold/*.eml \
		/var/spool/mailcrier/failed/*.eml; do
		if [ -e "$f" ]; then has_mail=yes; fi
	done
	if [ -n "$has_mail" ]; then
		echo "mailcrier: /var/spool/mailcrier is left in place, it holds messages"
	fi
fi
