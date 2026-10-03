#!/bin/sh
# apk unpacks the binary as root:root 0755 on install and upgrade alike.
# chgrp comes first because it clears the setgid bit. cron.update makes
# BusyBox crond read /etc/crontabs/slendmail again.
set -e

chgrp slendmail /usr/sbin/slendmail
chmod 2755 /usr/sbin/slendmail
touch /etc/crontabs/cron.update >/dev/null || true
