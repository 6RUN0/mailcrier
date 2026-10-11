# Slack

Тип `slack` публикует с токеном бота Slack Web API. Ключи, которые
принимает любая цель, и как классифицируется ошибка:
[Цели](../targets.ru.md); текст длиннее лимита:
[Длинные письма](../long-messages.ru.md).

```toml
[target.prod-slack]
type = "slack"
token_file = "/etc/mailcrier.d/slack.token"  # токен бота, xoxb-...
channel = "#alerts"                          # имя или ID канала
```

`channel` обязателен и не может быть пустым; пробельные символы вокруг
него отбрасываются. Письмо уходит через `chat.postMessage` Web API с
токеном бота как bearer-токеном и выключенным разворачиванием ссылок,
затем на каждое вложение - `files.getUploadURLExternal` и загрузка, и
один `files.completeUploadExternal`, который публикует файлы в ветке
сообщения; боту нужны `chat:write` и `files:write`, и он должен быть
участником приватного канала. `&`, `<` и `>` письма экранируются, так что
`<!channel>` или `<@U123>` остаются текстом. Текст - не больше 40000
символов; отправляется до 10 файлов. Ответ с `"ok": false` - ошибка;
`ratelimited`, `internal_error`, `fatal_error`, `service_unavailable` и
`request_timeout` - временные ошибки. Несколько целей `slack` могут
публиковать с разными токенами в разные каналы.

English: [slack.md](slack.md).
