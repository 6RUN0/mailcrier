#!/bin/sh
# cron.update makes BusyBox crond drop the removed /etc/crontabs/mailcrier.
# Names a spool that still holds messages; configuration, group and user stay.
set -e

touch /etc/crontabs/cron.update >/dev/null || true
has_mail=
for f in /var/spool/mailcrier/queue/*.eml /var/spool/mailcrier/hold/*.eml \
	/var/spool/mailcrier/failed/*.eml; do
	if [ -e "$f" ]; then has_mail=yes; fi
done
if [ -n "$has_mail" ]; then
	echo "mailcrier: /var/spool/mailcrier is left in place, it holds messages"
fi
