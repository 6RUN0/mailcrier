#!/bin/sh
# apk runs it on removal only. It deletes the queue run locks of a spool
# without messages, so that apk can remove the empty spool; a spool with
# messages stays as it is.
set -e

has_mail=
for f in /var/spool/mailcrier/queue/*.eml /var/spool/mailcrier/hold/*.eml \
	/var/spool/mailcrier/failed/*.eml; do
	if [ -e "$f" ]; then has_mail=yes; fi
done
if [ -z "$has_mail" ]; then
	rm -f /var/spool/mailcrier/locks/*.lock || true
fi
