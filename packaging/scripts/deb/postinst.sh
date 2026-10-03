#!/bin/sh
# The setgid bit through dpkg-statoverride, which dpkg applies to every later
# version as well; an override set by the administrator is kept. The timer
# parts are the postinst-systemd-enable and postinst-systemd-restart
# fragments of debhelper 13.24.2.
set -e

if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ] ; then
	dpkg-statoverride --list /usr/sbin/slendmail >/dev/null ||
		dpkg-statoverride --update --add root slendmail 2755 /usr/sbin/slendmail
fi

if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ] ; then
	deb-systemd-helper unmask slendmail-queue.timer >/dev/null || true

	# was-enabled defaults to true, so new installations run enable.
	if deb-systemd-helper --quiet was-enabled slendmail-queue.timer; then
		# Enables the unit on first installation, creates new
		# symlinks on upgrades if the unit file has changed.
		deb-systemd-helper enable slendmail-queue.timer >/dev/null || true
	else
		# Update the statefile to add new symlinks (if any), which need to be
		# cleaned up on purge. Also remove old symlinks.
		deb-systemd-helper update-state slendmail-queue.timer >/dev/null || true
	fi
fi

if [ "$1" = "configure" ] || [ "$1" = "abort-upgrade" ] || [ "$1" = "abort-deconfigure" ] || [ "$1" = "abort-remove" ] ; then
	if [ -d /run/systemd/system ]; then
		systemctl --system daemon-reload >/dev/null || true
		if [ -n "$2" ]; then
			_dh_action=restart
		else
			_dh_action=start
		fi
		deb-systemd-invoke $_dh_action slendmail-queue.timer >/dev/null || true
	fi
fi
