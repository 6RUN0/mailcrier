#!/bin/sh
# cron.update makes BusyBox crond drop the removed /etc/crontabs/slendmail.
# Names a spool that still holds messages; configuration, group and user stay.
set -e

touch /etc/crontabs/cron.update >/dev/null || true
has_mail=
for f in /var/spool/slendmail/queue/*.eml /var/spool/slendmail/hold/*.eml \
	/var/spool/slendmail/failed/*.eml; do
	if [ -e "$f" ]; then has_mail=yes; fi
done
if [ -n "$has_mail" ]; then
	echo "slendmail: /var/spool/slendmail is left in place, it holds messages"
fi
