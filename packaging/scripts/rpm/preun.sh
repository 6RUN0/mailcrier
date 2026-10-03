#!/bin/sh
# Acts on erase only ($1 = 0); an upgrade runs it with 1 after the new %post.
set -e

if [ "$1" = 0 ]; then
	if command -v systemctl >/dev/null; then
		systemctl disable slendmail-queue.timer >/dev/null || true
		if [ -d /run/systemd/system ]; then
			systemctl stop slendmail-queue.timer >/dev/null || true
		fi
	fi
	alternatives --remove mta /usr/sbin/slendmail >/dev/null || true
fi
