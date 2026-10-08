FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

RUN apt-get update \
    && apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils genisoimage openssh-client \
    && rm -rf /var/lib/apt/lists/*
