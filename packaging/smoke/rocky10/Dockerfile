FROM rockylinux/rockylinux:10@sha256:827d37bc128288ccf160ee318bb3cb92d591164cb217e92f8bc61e3982ae1834 AS plain

# postfix holds the mta alternative that the package takes over and gives
# back on removal.
RUN dnf install -y cronie at strace util-linux postfix \
    && dnf clean all \
    && useradd -m alice \
    && useradd -m queuer
