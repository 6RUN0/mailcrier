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
символов, в том числе вне BMP; до 10 файлов не больше 20 MiB каждый и
25 MiB вместе уходят в том же запросе.

## URL вебхука

Цели нужен URL вебхука канала. Шаги следуют статье Discord о вебхуках
(<https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks>)
и справке по Execute Webhook
(<https://docs.discord.com/developers/resources/webhook#execute-webhook>).

1. Откройте **Server Settings**, вкладку **Integrations**, и нажмите
   **Create Webhook**; для этого нужно право управлять вебхуками
   (`MANAGE_WEBHOOKS`). Выберите текстовый канал в выпадающем меню и
   назовите вебхук: сообщения показывают это имя как автора.
2. Нажмите **Copy Webhook URL**. URL,
   `https://discord.com/api/webhooks/<id>/<token>`, - единственный
   секрет: любой, у кого он есть, может писать в канал. Он кладётся в
   файл, который читают только root и группа `mailcrier`, как токен в
   разделе [Быстрый старт](../../README.ru.md#быстрый-старт):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/discord.url
   printf '%s\n' 'https://discord.com/api/webhooks/<id>/<token>' > /etc/mailcrier.d/discord.url
   ```

3. Чтобы писать в ветку канала, допишите к URL `?thread_id=<thread id>`;
   ветку в архиве Discord из архива вернёт. Канал-форум или медиаканал
   принимает сообщение вебхука только с `thread_id` или с `thread_name`,
   который создаёт ветку; mailcrier `thread_name` не отправляет, поэтому
   вебхуку такого канала нужен `thread_id`.
4. Проверьте командой `mailcrier --probe -- <target name>`: `class=ok` и
   пробное письмо в канале. Удалённый вебхук или обрезанный URL дают
   `class=perm` с HTTP-статусом в `status` и его текстом в `err`.
   Причина, которую Discord называет в теле ответа, `Unknown webhook` или
   `Invalid webhook token provided`, в журнал не пишется, потому что тело
   может повторять запрос; статус 429 и 5xx - временные ошибки.

English: [discord.md](discord.md).
