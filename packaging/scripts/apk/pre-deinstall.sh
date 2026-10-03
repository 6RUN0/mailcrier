#!/bin/sh
# apk runs it on removal only. It deletes the queue run locks of a spool
# without messages, so that apk can remove the empty spool; a spool with
# messages stays as it is.
set -e

has_mail=
for f in /var/spool/slendmail/queue/*.eml /var/spool/slendmail/hold/*.eml \
	/var/spool/slendmail/failed/*.eml; do
	if [ -e "$f" ]; then has_mail=yes; fi
done
if [ -z "$has_mail" ]; then
	rm -f /var/spool/slendmail/locks/*.lock || true
fi
