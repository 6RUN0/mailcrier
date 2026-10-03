#!/bin/sh
# The prerm-systemd-restart fragment of debhelper 13.24.2: the timer stops
# on removal only, an upgrade leaves it running.
set -e

if [ -z "$DPKG_ROOT" ] && [ "$1" = remove ] && [ -d /run/systemd/system ] ; then
	deb-systemd-invoke stop slendmail-queue.timer >/dev/null || true
fi
