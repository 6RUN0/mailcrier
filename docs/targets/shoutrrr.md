# shoutrrr

The `shoutrrr` type sends through a service of the shoutrrr
library. Keys every target takes and how a failure is classified:
[Targets](../targets.md).

```toml
[target.bus]
type = "shoutrrr"
url_file = "/etc/mailcrier.d/bus.url"   # a service URL, gotify://host/token
```

A target of this type sends one message through a service of the shoutrrr
library (github.com/nicholas-fedor/shoutrrr v0.21.1), chosen by the scheme
of the URL: `gotify`, `matrix`, `teams`, `pushover`, `smtp`, `generic` and
the others its documentation lists. The text is that of the plain template
(subject, host, sender, body), without files and without a title; there
is no length limit of mailcrier's, each service cuts or splits a long text
as the library does. An unknown scheme or a URL the service rejects exits
78 when the configuration is loaded. `matrix` takes an access token only
(`matrix://:token@host`): with a user name the library logs in while the
configuration is loaded, outside `http_timeout` and `deadline`, so such
a URL exits 78 without contacting the server. A failure after an HTTP
answer outside 2xx is classified by its status, as for the other targets;
a redirect is not followed and fails permanently even where the library
would count it a success; any other failure is temporary and stays queued
until `queue_ttl`. The services that send over HTTP use `http_timeout`
and the [proxy rule](../configuration.md#timeouts-and-proxy); `smtp`,
`xmpp` and `mqtt` open their own connections without a proxy, bounded
only by `deadline`. Error texts of the library may quote the URL, which
is masked in the log as every URL is.
