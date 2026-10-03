#!/bin/sh
# Group and user mailcrier, created before the files are unpacked so that the
# configuration and the spool get the group from the package. Both stay after
# removal.
set -e

if [ -e /usr/sbin/nologin ]; then
	nologin=/usr/sbin/nologin
else
	nologin=/sbin/nologin
fi

if ! getent group mailcrier >/dev/null; then
	if command -v groupadd >/dev/null; then
		groupadd -r mailcrier
	else
		addgroup -S mailcrier
	fi
fi

if ! getent passwd mailcrier >/dev/null; then
	if command -v useradd >/dev/null; then
		useradd -r -g mailcrier -d /var/spool/mailcrier -M -s "$nologin" mailcrier
	else
		adduser -S -D -H -h /var/spool/mailcrier -s "$nologin" -G mailcrier mailcrier
	fi
fi
