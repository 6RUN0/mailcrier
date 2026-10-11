# Discord

Тип `discord` публикует через вебхук канала Discord. Ключи, которые
принимает любая цель, и как классифицируется ошибка:
[Цели](../targets.ru.md); текст длиннее лимита:
[Длинные письма](../long-messages.ru.md).

```toml
[target.ops-discord]
type = "discord"
url_file = "/etc/mailcrier.d/discord.url"  # https://discord.com/api/webhooks/<id>/<token>
```

Цель этого типа вызывает вебхук один раз на письмо с `wait=true`, чтобы
Discord ответил после сохранения сообщения, и с `allowed_mentions` с
пустым списком `parse`, чтобы `@everyone`, `@here` и упоминания
пользователей или ролей в письме никого не оповещали. Параметры запроса
в URL, например `thread_id`, сохраняются. Текст - не больше 2000
символов (в единицах UTF-16); до 10 файлов не больше 20 MiB каждый и
25 MiB вместе уходят в том же запросе.

English: [discord.md](discord.md).
