#!/bin/sh
# apk unpacks the binary as root:root 0755 on install and upgrade alike.
# chgrp comes first because it clears the setgid bit. cron.update makes
# BusyBox crond read /etc/crontabs/mailcrier again.
set -e

chgrp mailcrier /usr/sbin/mailcrier
chmod 2755 /usr/sbin/mailcrier
touch /etc/crontabs/cron.update >/dev/null || true
