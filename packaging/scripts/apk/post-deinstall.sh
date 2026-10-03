#!/bin/sh
# cron.update makes BusyBox crond drop the removed /etc/crontabs/slendmail.
# Names a spool that still holds messages; configuration, group and user stay.
set -e

touch /etc/crontabs/cron.update >/dev/null || true
if [ -d /var/spool/slendmail ]; then
	echo "slendmail: /var/spool/slendmail is left in place, it holds messages"
fi
