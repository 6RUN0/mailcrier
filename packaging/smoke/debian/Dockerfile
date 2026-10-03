FROM debian:trixie@sha256:9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c AS plain

# cron and at recommend an MTA; without --no-install-recommends exim4 comes
# along, and the Conflicts of the package keeps it from installing offline.
RUN apt-get update \
    && apt-get install -y --no-install-recommends cron at strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m alice \
    && useradd -m queuer
