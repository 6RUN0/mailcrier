#!/bin/sh
# Group and user slendmail, created before the files are unpacked so that the
# configuration and the spool get the group from the package. Both stay after
# removal.
set -e

if [ -e /usr/sbin/nologin ]; then
	nologin=/usr/sbin/nologin
else
	nologin=/sbin/nologin
fi

if ! getent group slendmail >/dev/null; then
	if command -v groupadd >/dev/null; then
		groupadd -r slendmail
	else
		addgroup -S slendmail
	fi
fi

if ! getent passwd slendmail >/dev/null; then
	if command -v useradd >/dev/null; then
		useradd -r -g slendmail -d /var/spool/slendmail -M -s "$nologin" slendmail
	else
		adduser -S -D -H -h /var/spool/slendmail -s "$nologin" -G slendmail slendmail
	fi
fi
