#!/bin/sh
# Acts on erase only ($1 = 0) and names a spool that still holds messages.
# Configuration, group and user stay.
set -e

if [ "$1" = 0 ]; then
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload >/dev/null || true
	fi
	has_mail=
	for f in /var/spool/slendmail/queue/*.eml /var/spool/slendmail/hold/*.eml \
		/var/spool/slendmail/failed/*.eml; do
		if [ -e "$f" ]; then has_mail=yes; fi
	done
	if [ -n "$has_mail" ]; then
		echo "slendmail: /var/spool/slendmail is left in place, it holds messages"
	fi
fi
