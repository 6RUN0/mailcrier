# ntfy

The `ntfy` type publishes to a topic of an ntfy server. Keys every target
takes and how a failure is classified: [Targets](../targets.md); a text
over the limit: [Long messages](../long-messages.md).

```toml
[target.phone]
type = "ntfy"
# https://ntfy.sh/<topic>, user:password@ allowed
url_file = "/etc/mailcrier.d/ntfy.url"
max_file_size = 2000000         # optional, ntfy.sh takes files of 2 MB
```

A target of this type makes a JSON publish to the server root with the
topic, the subject as title (line breaks as spaces, at most 1 KB) and the
text of at most 4095 bytes, then one PUT per attachment to the topic with
`filename` and `title` in the query; up to 10 files of at most 15 MiB each,
the defaults of an ntfy server. ntfy.sh takes files of at most 2 MB, so
a target there sets `max_file_size`. The URL must end with the topic,
otherwise exit status 78; user information and a query such as `auth`
are sent with every request.

Russian: [ntfy.ru.md](ntfy.ru.md).
