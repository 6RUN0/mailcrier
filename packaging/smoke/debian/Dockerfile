FROM debian:trixie@sha256:9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c AS plain

# cron and at recommend an MTA; without --no-install-recommends exim4 comes
# along, and the Conflicts of the package keeps it from installing offline.
RUN apt-get update \
    && apt-get install -y --no-install-recommends cron at strace util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m alice \
    && useradd -m queuer

FROM plain AS systemd

# The policy-rc.d of the Docker image would keep deb-systemd-invoke from
# starting the timer. Docker mounts the cgroup tree read-only unless the
# container is privileged; with CAP_SYS_ADMIN and without an AppArmor
# profile, whose docker-default denies mount, the container remounts its
# own cgroup namespace writable, which systemd needs as PID 1.
RUN apt-get update \
    && apt-get install -y --no-install-recommends systemd systemd-sysv \
    && rm -rf /var/lib/apt/lists/* /usr/sbin/policy-rc.d

CMD ["sh", "-c", "mount -o remount,rw /sys/fs/cgroup && exec /sbin/init"]
