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
участником канала. `&`, `<` и `>` письма экранируются, так что
`<!channel>` или `<@U123>` остаются текстом. Текст - не больше 40000
символов; отправляется до 10 файлов. Ответ с `"ok": false` - ошибка;
`ratelimited`, `internal_error`, `fatal_error`, `service_unavailable` и
`request_timeout` - временные ошибки. Несколько целей `slack` могут
публиковать с разными токенами в разные каналы.

## Приложение и токен бота

Цели нужен токен бота приложения Slack, установленного в рабочее
пространство, и бот в канале. Шаги следуют краткому руководству по
приложениям Slack
(<https://docs.slack.dev/app-management/quickstart-app-settings>) и
справке по `chat.postMessage`
(<https://docs.slack.dev/reference/methods/chat.postMessage>).

1. На странице приложений, <https://api.slack.com/apps?new_app=1>,
   нажмите **Create an app**, выберите **Blank app** и **Continue**,
   введите **App Name**, выберите **Workspace** и нажмите **Create**.
2. В **OAuth & Permissions**, в разделе **Scopes**, **Bot Token Scopes**,
   нажмите **Add an OAuth Scope** и добавьте `chat:write` для текста и
   `files:write` для вложений. Других методов mailcrier не вызывает, и
   других прав ему не нужно.
3. Вверху **OAuth & Permissions** нажмите **Install to Workspace**, затем
   **Allow**. Токен, `xoxb-...`, появляется в разделе **OAuth Tokens** и
   кладётся в файл, который читают только root и группа `mailcrier`, как
   токен в разделе [Быстрый старт](../../README.ru.md#быстрый-старт):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/slack.token
   printf '%s\n' 'xoxb-your-token' > /etc/mailcrier.d/slack.token
   ```

   Право, добавленное после установки, действует, когда приложение
   переустановлено: **Reinstall to Workspace** на странице **Install App**.
4. В канале отправьте `/invite @<app name>`: боту вне канала Slack
   отвечает `not_in_channel`.
5. Задайте в `channel` имя канала, `#alerts`, или его ID: `C`, за которой
   идут буквы и цифры. ID не меняется, когда канал переименован или
   сделан приватным (<https://docs.slack.dev/faq>), имя меняется.
6. Проверьте командой `mailcrier --probe -- <target name>`. Отклонённый
   вызов Slack отвечает статусом 200 и кодом ошибки, поэтому в строке нет
   `status`, а код стоит в `err`, например
   `target=prod-slack class=perm err="not_in_channel"`:
   - `not_in_channel`: бота нет в канале, шаг 4;
   - `channel_not_found`: неверное имя или ID;
   - `missing_scope`: нет `chat:write`, или право добавлено без
     переустановки из шага 3;
   - `invalid_auth`: неверный токен;
   - `is_archived`: канал в архиве.

Пробное письмо без вложений, поэтому `files:write` оно не проверяет. Без
этого права письмо с вложением приходит одним текстом и считается
доставленным; в журнале - `attachments not delivered` с `missing_scope` в
`err`.

English: [slack.md](slack.md).
