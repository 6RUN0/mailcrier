#!/bin/sh
# Acts on erase only ($1 = 0); an upgrade runs it with 1 after the new %post.
# Erase deletes the queue run locks of a spool without messages, so that rpm
# can remove the empty spool; a spool with messages stays as it is.
set -e

if [ "$1" = 0 ]; then
	if command -v systemctl >/dev/null; then
		systemctl disable slendmail-queue.timer >/dev/null || true
		if [ -d /run/systemd/system ]; then
			systemctl stop slendmail-queue.timer >/dev/null || true
		fi
	fi
	has_mail=
	for f in /var/spool/slendmail/queue/*.eml /var/spool/slendmail/hold/*.eml \
		/var/spool/slendmail/failed/*.eml; do
		if [ -e "$f" ]; then has_mail=yes; fi
	done
	if [ -z "$has_mail" ]; then
		rm -f /var/spool/slendmail/locks/*.lock || true
	fi
	alternatives --remove mta /usr/sbin/slendmail >/dev/null || true
fi
