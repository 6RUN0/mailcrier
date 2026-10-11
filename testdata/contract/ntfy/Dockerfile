FROM binwiederhier/ntfy:v2.28.0@sha256:6ef4b819f722fccdc036af611c4774cfdc2de821ab74fdd48bbf4c9d6f8973da

# Without an attachment cache ntfy refuses a PUT of a file, and without a
# base URL it refuses to start with one; the host name only prefixes the
# attachment URLs, which the tests read by their path. The in-memory
# message cache answers concurrent publishes with "database table is
# locked" (500) and loses messages, so the cache is a file.
ENV NTFY_BASE_URL=http://ntfy.invalid \
    NTFY_ATTACHMENT_CACHE_DIR=/var/cache/ntfy/attachments \
    NTFY_CACHE_FILE=/var/cache/ntfy/cache.db

RUN mkdir -p /var/cache/ntfy

CMD ["serve"]
