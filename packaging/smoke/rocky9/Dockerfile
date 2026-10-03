FROM rockylinux/rockylinux:9@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f AS plain

# postfix holds the mta alternative that the package takes over and gives
# back on removal.
RUN dnf install -y cronie at strace util-linux postfix \
    && dnf clean all \
    && useradd -m alice \
    && useradd -m queuer

FROM plain AS systemd

RUN dnf install -y systemd \
    && dnf clean all

# Docker mounts the cgroup tree read-only unless the container is
# privileged; with CAP_SYS_ADMIN and without an AppArmor profile, whose
# docker-default denies mount, the container remounts its own cgroup
# namespace writable, which systemd needs as PID 1.
CMD ["sh", "-c", "mount -o remount,rw /sys/fs/cgroup && exec /usr/sbin/init"]
