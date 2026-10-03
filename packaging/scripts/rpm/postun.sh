#!/bin/sh
# Acts on erase only ($1 = 0) and names a spool that still holds messages.
# Configuration, group and user stay.
set -e

if [ "$1" = 0 ]; then
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload >/dev/null || true
	fi
	if [ -d /var/spool/slendmail ]; then
		echo "slendmail: /var/spool/slendmail is left in place, it holds messages"
	fi
fi
