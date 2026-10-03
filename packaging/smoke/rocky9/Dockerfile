FROM rockylinux/rockylinux:9@sha256:8101994123cf3d0a8fee517bee7f39e555c7d92bd2d9eb3303cc988a0eeed00f AS plain

# postfix holds the mta alternative that the package takes over and gives
# back on removal.
RUN dnf install -y cronie at strace util-linux postfix \
    && dnf clean all \
    && useradd -m alice \
    && useradd -m queuer
