# Templates

Templates of your own replace the built-in text of a target: the keys
`template` and `template_file`, the data and functions a template has,
and what happens when one fails or is too long. A worked example is
[`packaging/examples/templates.conf`](../packaging/examples/templates.conf).

```toml
[target.ops-telegram]
type = "telegram"
token_file = "/etc/mailcrier.d/telegram.token"
chat_id = -1001234567890
template = '''
<b>{{ .Subject | default .Strings.NoSubject | tgHTML }}</b>
<i>{{ .Hostname | tgHTML }}</i> {{ .Date | tz "Europe/Berlin" | date "15:04" }}
<pre>{{ .Body | default .Strings.EmptyBody | trimEnd | tgHTML }}</pre>'''

[target.api]
type = "http"
url = "https://api.example.org/notify"
template = '''
{"title": {{ toJson .Subject }}, "host": {{ toJson .Hostname }},
 "text": {{ toJson .Body }}}'''
```

- `template`, a Go `text/template` (<https://pkg.go.dev/text/template>),
  replaces the built-in template of a target of any type but `exec`;
  `template_file`, an absolute path, names a file that holds it, read when
  the configuration is loaded, without the line break at its end. Not both;
  a blank template or file exits 78. An `http` target takes exactly one of
  `preset` and a template, so `username` and `channel` are not available
  with a template.
- The template writes the markup of the target: Telegram HTML (the Bot
  API parse mode is HTML, there is no other), Discord markdown, Slack
  mrkdwn, the plain text of ntfy (the title stays the subject) and of
  `shoutrrr`, the JSON request body of `http`, sent with
  `Content-Type: application/json` unless a header replaces it. Everything
  from the message comes from whoever wrote it and goes through the
  escaper of that markup: `tgHTML`; `discordEscape`, `discordCode` inside a
  code block; `slackEscape`, `slackCode`; `mmEscape`, `mmCode` for
  Mattermost markdown; `toJson` for a JSON value.
- Secrets stay out of the `{{ }}` actions: an error quotes the action it
  failed in, and goes to the log. A token belongs in a header, in the URL,
  or in the template text outside the actions, which no error quotes.
- A template that does not parse, calls an unknown function or uses
  `define`, `template` or `block` rejects the configuration: exit status
  78, the message is held, and `configuration rejected` names the target
  and the line in the template.

## Data

The fields a template reads:

| Field | Content |
|---|---|
| `.Subject` | decoded Subject, empty when the message has none |
| `.From` | From address, else the envelope sender: `.From.Name`, `.From.Addr`, `.From.String` |
| `.Sender`, `.SenderName` | envelope sender and the name of `-F` |
| `.To`, `.Cc` | addresses of the headers, like `.From` |
| `.Recipients` | envelope recipients without the addresses only Bcc or Resent-Bcc named |
| `.Date`, `.ReceivedAt` | the Date header (the receive time without one) and the receive time |
| `.MessageID` | the Message-ID header |
| `.Headers` | all header fields but Bcc and Resent-Bcc: `.Headers.Get "X-Cron-Env"` (the first value), `.Headers.Values` (all), `.Headers.Has`, `.Headers.Names`, names in any case |
| `.Body`, `.BodyHTML` | the text, and the HTML part when there is one |
| `.Attachments`, `.MoreAttachments` | `.Name`, `.ContentType`, `.Size`, `.IsSkipped` (listed, not sent) of each attachment listed, and the number left out |
| `.Hostname`, `.Target`, `.Limit` | host name, target name, text limit of the target (0: none) |
| `.IsCollapsed` | the cut body belongs in a collapsed block (`on_long = "blockquote"`) |
| `.Strings` | the notices of `[strings]`: `.Strings.NoSubject`, `.Strings.EmptyBody`, `.Strings.Truncated`, `.Strings.MoreAttachments`, `.Strings.NotSent` |

## Functions

The functions, besides those of `text/template` (`printf`, `len`,
`index`, `urlquery` and the others):

| Function | Result |
|---|---|
| `toUpper`, `toLower`, `trimSpace`, `trimEnd`, `title` | the string in capitals, in small letters, without white space around it, without it at the end, with the first letter of every word capitalized and the rest kept |
| `default D V` | `V`, or `D` when `V` is empty or white space only |
| `join SEP LIST` | the strings or addresses of `LIST` joined by `SEP`: `{{ .To \| join ", " }}` |
| `match RE S`, `reReplaceAll RE REPL S` | whether the Go regular expression `RE` matches in `S`; `S` with every match replaced, `$1` in `REPL` for a group |
| `tz ZONE T`, `date LAYOUT T` | `T` in the IANA zone `ZONE`, read from the zone database of the system, without which any zone but `UTC` and `Local` fails the template, as an unknown one does; `T` in a Go layout: `{{ .Date \| tz "UTC" \| date "2006-01-02 15:04" }}` |
| `lines S`, `head N S`, `tail N S` | the lines of `S`, for `range`; its first and last `N` lines |
| `truncate N S` | `S` cut to `N` characters, the last three of them `...`; without them when `N` is 3 or less |
| `indent N S` | `N` spaces before every line of `S` |
| `humanizeBytes N` | a size: `1.5 KiB` |
| `toJson V` | `V` as a JSON value, always valid |
| `pathSegment S` | `S` escaped for one segment of a URL path, `/` included |
| `tgHTML`, `discordEscape`, `discordCode`, `slackEscape`, `slackCode`, `mmEscape`, `mmCode` | the escapers of the markups above |

## Cutting and limits

- The text is cut to the limit of the target as a built-in one is (see
  [Long messages](long-messages.md#how-a-text-is-cut)): the subject, the
  attachments and the body are cut and the template rendered again. That
  works for a template whose text grows with them; what it adds on its
  own is cut hard, and an `http` body that does not fit `max_text` at
  all fails the template.
- Cutting a long text renders the template again and again, up to a few
  dozen times. All the renderings of the text of one message for one
  target may take 2 s together, ending no later than `deadline`, and each
  one 1 s and 1 MiB of output. Past 1 MiB a text with a limit counts as
  too long and its body is cut, with the warning `template output limit
  reached`; without a limit (`http` without
  `max_text`, `shoutrrr`) the template fails. A template stopped while
  rendering, by either time limit, stays failed for the rest of the call
  or queue run, so that each further message gets the built-in template at
  once. A rendering due after the time is spent is not started: that
  message gets the built-in template, and the template stays in use.

## When a template fails

- A template that fails while rendering a message (an error of a function,
  the time or size limit, a text of white space only, an `http` body over
  `max_text`) leaves that message the built-in template of the target, and
  the log records `template failed, built-in used` (warning) with `target`
  and `err`; the exit status follows the delivery, 0 when the target took
  it. A Telegram target that answers 400 to the text of the template gets
  the text of the built-in template once, with the same record, before the
  full text goes as a file alone (see [Long messages](long-messages.md)).
- The path, query and header templates of an `http` target (see [HTTP and
  Mattermost](targets/http.md)) are rendered under the same limits, from the
  same budget as the text; each may render nothing, which leaves the value
  empty. They have no built-in template to fall back to: when one fails,
  the target fails permanently, gets nothing for that message, and the
  log records `request template failed, message not sent` (warning) with
  `target` and `err`. A parse error of one of them names `<target>.path`,
  `<target>.query.<name>` or `<target>.headers.<name>` and its line.
