FROM binwiederhier/ntfy:v2.28.0@sha256:6ef4b819f722fccdc036af611c4774cfdc2de821ab74fdd48bbf4c9d6f8973da

# Without an attachment cache ntfy refuses a PUT of a file, and without a
# base URL it refuses to start with one; the host name only prefixes the
# attachment URLs, which the tests read by their path.
ENV NTFY_BASE_URL=http://ntfy.invalid \
    NTFY_ATTACHMENT_CACHE_DIR=/var/cache/ntfy/attachments

CMD ["serve"]
