# Mailcrier

Начинался как форк github.com/iggy/slendmail.

Замена sendmail для машин, которые не отправляют почту: письма от cron,
at, sudo, mdadm, smartd, fail2ban и любой другой программы, передающей
письмо в `/usr/sbin/sendmail`, приходят вместо этого в Telegram, Discord,
Slack, ntfy, на HTTP-вебхук, в сервис библиотеки shoutrrr или в
собственную программу.

- Принимает командные строки привычных вызывающих программ (`-t`, `-i`,
  `-f`, `-F`, `-oi`, `-bi`, `-q` и остальные) и читает MIME-письма:
  закодированные слова, quoted-printable, base64, любую кодировку,
  HTML-части и вложения.
- Отправляет во все настроенные цели одновременно, каждой - в разметке её
  сервиса, с экранированием всего, что пришло из письма, с обрезкой до
  длины, которую принимает сервис, и с теми вложениями, которые он может
  принять.
- Хранит письмо в спуле, пока его не получит каждая цель: недоступный
  сервис получит его при одном из следующих прогонов очереди.
- Один статический исполняемый файл, установленный с битом setgid: задания
  любого пользователя могут отправлять письма, а токены читает только
  root; журнал пишется в syslog.

## Быстрый старт

Почта хоста в чат Telegram, на Debian или Ubuntu с пакетом релиза; у rpm и
apk отличается пакет в шагах 1 и 2, см. [Установка](docs/install.ru.md).
Каждый шаг выполняется от root.

1. Укажите версию последнего релиза со страницы
   <https://github.com/6RUN0/mailcrier/releases> и скачайте его пакет:

   ```sh
   VERSION=0.1.0-rc5
   curl -fLO "https://github.com/6RUN0/mailcrier/releases/download/v$VERSION/mailcrier_${VERSION}_linux_amd64.deb"
   ```

2. Установите его; пакет заменяет MTA хоста:

   ```sh
   apt install "./mailcrier_${VERSION}_linux_amd64.deb"
   ```

3. Запишите токен бота в файл, который читают только root и группа
   `mailcrier`; как создать бота и узнать id его чата, описано в разделе
   [Telegram](docs/targets/telegram.ru.md#бот-и-id-чата):

   ```sh
   install -m 0640 -o root -g mailcrier /dev/null /etc/mailcrier.d/telegram.token
   printf '%s\n' '123456:ABC-your-token' > /etc/mailcrier.d/telegram.token
   ```

4. Добавьте цель в конец `/etc/mailcrier.conf`, указав id чата:

   ```toml
   [target.ops-telegram]
   type = "telegram"
   token_file = "/etc/mailcrier.d/telegram.token"
   chat_id = -1001234567890
   ```

5. Проверьте файл. Ожидаемый вывод - одна строка в stderr, код выхода 0:

   ```sh
   mailcrier --check-config
   ```

   ```text
   /etc/mailcrier.conf: 0 errors, 0 warnings
   ```

   Ошибка называет строку, которую нужно исправить, см.
   [Проверка конфигурации](docs/checking.ru.md).

6. Отправьте тестовое письмо через цель. Ожидаемый вывод - одна строка в
   stdout, код выхода 0 и сообщение в чате:

   ```sh
   mailcrier --probe
   ```

   ```text
   target=ops-telegram class=ok
   ```

   `class=perm status=400` с `chat not found` означает неверный `chat_id`
   или бота, которого нет в чате.

7. Отправьте письмо так, как это делает cron, и прочитайте его записи в
   журнале:

   ```sh
   printf 'Subject: test\n\nhello\n' | sendmail root
   journalctl -t mailcrier --since -5min
   ```

   Записи `message received` и `target delivered` с `target=ops-telegram`
   подтверждают доставку; без journald они лежат в `/var/log/mail.log`
   (Debian, Ubuntu) или `/var/log/maillog` (RHEL, Rocky), см.
   [Журнал](docs/logging.ru.md#как-найти-письмо).

До шага 4 каждое письмо задерживается в спуле с кодом выхода 78; первый
прогон очереди после того, как файл загрузится, доставляет их, см.
[Спул и очередь](docs/spool.ru.md#задержанные-письма).

## Документация

| Страница | На что отвечает |
|---|---|
| [Установка](docs/install.ru.md) | пакеты, что они меняют на хосте, обновление, удаление, сборка |
| [Конфигурация](docs/configuration.ru.md) | файл, его строгая проверка, термины, примеры, `[general]`, `[strings]`, таблица ключей целей |
| [Цели](docs/targets.ru.md) | назначения, по странице на тип: [Telegram](docs/targets/telegram.ru.md), [Discord](docs/targets/discord.ru.md), [Slack](docs/targets/slack.ru.md), [ntfy](docs/targets/ntfy.ru.md), [HTTP и Mattermost](docs/targets/http.ru.md), [хуки exec](docs/targets/exec.ru.md), [shoutrrr](docs/targets/shoutrrr.ru.md) |
| [Длинные письма](docs/long-messages.ru.md) | как режется текст сверх лимита, что идёт вместе с ним файлом |
| [Шаблоны](docs/templates.ru.md) | собственный текст для каждой цели: данные, функции, сбои |
| [Маршруты и подавление](docs/routing.ru.md) | какая цель получает какое письмо, отсев шума, личные чаты |
| [Спул и очередь](docs/spool.ru.md) | повторы, задержанные и неотправленные письма, ограничения, `mailq` |
| [Проверка конфигурации](docs/checking.ru.md) | `--check-config` и `--probe` |
| [Журнал](docs/logging.ru.md) | куда идут записи, их поля, маскирование секретов |
| [Привилегии](docs/privileges.ru.md) | установка с битом setgid и что разрешено вызову с повышенными правами |
| [Контейнеры](docs/containers.ru.md) | запуск без бита setgid, память |
| [Командная строка](docs/cli.ru.md) | флаги sendmail, получатели, разбор письма, коды выхода |
| [SELinux](docs/selinux.ru.md) | модуль пакета rpm |
| [mailcrier(8)](docs/mailcrier.8) | страница руководства, её тоже устанавливают пакеты |

English: [README.md](README.md).
