# HTTP and Mattermost

The `http` type sends one request per message: a Mattermost or Slack
incoming webhook, a JSON API, or a GET ping. Keys every target takes and
how a failure is classified: [Targets](../targets.md); its body, path,
query and header templates: [Templates](../templates.md).

```toml
[target.mm]
type = "http"
preset = "mattermost"           # mattermost | slack-webhook | generic-json
url_file = "/etc/mailcrier.d/mm.url"
username = "mailcrier"          # optional, mattermost only
channel = "alerts"              # optional, mattermost only

[target.api]
type = "http"
preset = "generic-json"
url = "https://api.example.org/notify"
method = "PUT"                  # optional: POST (default), GET, PUT, PATCH

[target.api.headers]            # optional, values are templates
Authorization = "Bearer api-token"

[target.ping]
type = "http"
method = "GET"
url = "https://status.example.org/api/push/abc"
path = "/{{ pathSegment .Hostname }}"   # optional, after the path of url

[target.ping.query]             # optional, values are templates
status = "down"
msg = "{{ .Subject | truncate 200 }}"
```

A target of this type sends one request of the JSON document of its preset,
or of its template (see [Templates](../templates.md)), with `Content-Type:
application/json` and no files. Exactly one of `preset`, `template` and
`template_file` is set. `method` is `POST`, the default, `GET`, `PUT` or
`PATCH`, in capitals; any other value exits 78. A `GET` sends no body and
no Content-Type, so it takes none of `preset`, `template`, `template_file`,
`max_text` and `max_lines`; the message reaches the service through `path`,
`query` and the headers.
`slack-webhook` is cut to 40000 characters, the length Slack keeps;
`mattermost` has no limit, since Mattermost splits a long text into
several posts, nor has `generic-json`. `mattermost` posts
`{"text": ...}` with subject, host, sender and body in Markdown,
`@channel`, `@all` and `@here` disarmed, and `username` and `channel`
added when set; a set `channel` must not be blank, white space around
it is dropped. `max_text` and `max_lines` (see "Long messages") apply to
every preset. `slack-webhook` posts `{"text": ...}` in Slack mrkdwn
with `&`, `<` and `>` escaped; `generic-json` posts
`{"subject": ..., "body": ..., "hostname": ...}`.
`[target.<name>.headers]` adds request headers after the Content-Type,
so a `Content-Type` there replaces it; a header name that is not an
HTTP token, a value with a line break or NUL, `Host`,
`Content-Length`, `Transfer-Encoding` or `Trailer`, which the HTTP
client sets itself and would drop, and `Connection`, `Keep-Alive`,
`Proxy-Connection` or `Upgrade`, which HTTP/2 does not carry, exit 78.
`path` is appended to the path of `url`, and `[target.<name>.query]`
adds its values to the query of `url`, encoded, after the query `url`
has. The values of `headers` and `query` and `path` are templates (see
[Templates](../templates.md)) rendered for every message; scheme, host
and port come from `url` or `url_file` alone. A path that renders empty
appends nothing, and the request goes to `url` as it is. A rendered path
that does not start with a single `/`, holds `?`, `#`, a backslash,
a control character or a `.` or `..` segment (escaped once or twice,
or followed by a `;` parameter), or a rendered header value with a line
break or NUL fails the target permanently before any request; an escaped
`/` in the path stays escaped. What `path` writes before its first `{{
}}` action is checked by these rules when the configuration is loaded,
exit status 78. A value from the message goes into a path segment through
`pathSegment`, which escapes `/` as well, but not dots: a value that is
`.` or `..`, or holds `/../`, still fails the target for that message. The
text of header and query values outside the `{{ }}` actions, 8 characters
or more, and the credential after a scheme such as `Bearer`, are masked in
the log like tokens, and so is every piece of the text of `path` outside
the actions between two slashes of 16 characters or more, as written and
unescaped. A redirect is not followed and counts as a permanent failure,
`redirect to https://host not followed` with only the scheme and host
of the Location: its path repeats that of the target, where a topic or
key is a secret of any length. The same holds for every target type,
`shoutrrr` included.

Russian: [http.ru.md](http.ru.md).
