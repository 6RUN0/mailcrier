FROM rockylinux/rockylinux:10@sha256:827d37bc128288ccf160ee318bb3cb92d591164cb217e92f8bc61e3982ae1834 AS plain

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
