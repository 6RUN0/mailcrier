FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS plain

# /etc/at.allow of Alpine admits root alone; the at job of a user is part
# of the test. ssmtp is fetched, not installed: the test installs it to
# check the conflict over /usr/sbin/sendmail.
RUN apk add --no-cache at strace setpriv \
    && echo alice >> /etc/at.allow \
    && mkdir /opt/ssmtp \
    && cd /opt/ssmtp \
    && apk fetch -R ssmtp \
    && adduser -D alice \
    && adduser -D queuer
