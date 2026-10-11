# Установка

Откуда берутся пакеты, что они устанавливают и меняют на хосте, как их
обновить и удалить и как собрать исполняемый файл. Первая
настройка описана на странице [Конфигурация](configuration.ru.md).

## Пакеты

К каждому релизу на <https://github.com/6RUN0/mailcrier/releases>
приложены пакеты deb, rpm и apk полной сборки для amd64, arm64 и armv7 без
подписей (`apk add --allow-untrusted`) и `checksums.txt` с SHA-256 каждого
файла. На каждый перечисленный там файл есть и аттестация происхождения
сборки от рабочего процесса релиза, которую проверяет GitHub CLI:

```sh
sha256sum --ignore-missing -c checksums.txt
gh attestation verify --repo 6RUN0/mailcrier \
  --signer-workflow 6RUN0/mailcrier/.github/workflows/release.yml \
  ./mailcrier_<version>_linux_amd64.deb
apt install ./mailcrier_<version>_linux_amd64.deb
dnf install ./mailcrier_<version>_linux_amd64.rpm
apk add --allow-untrusted ./mailcrier_<version>_linux_amd64.apk
```

| Путь | Владелец и права |
|---|---|
| `/usr/sbin/mailcrier` | `root:mailcrier 2755`, их ставят скрипты после распаковки |
| `/usr/sbin/sendmail`, `/usr/lib/sendmail`, `/usr/bin/mailq`, `/usr/bin/newaliases` | ссылки на него; в rpm - альтернатива `mta` |
| `/etc/mailcrier.conf` | `root:mailcrier 0640`, без цели |
| `/etc/mailcrier.d/` | `root:mailcrier 0750`, для `token_file` и подобных |
| `/var/spool/mailcrier/` | `root:mailcrier 0750`; `tmp`, `queue`, `hold`, `failed`, `locks` в нём - `2770` |
| `mailcrier-queue.timer` и `.service`, `/etc/cron.d/mailcrier` | прогон очереди, deb и rpm |
| `/etc/crontabs/mailcrier` | прогон очереди, apk |
| `/usr/share/doc/mailcrier/examples/` | примеры конфигураций, см. [Конфигурация](configuration.ru.md#примеры) |
| `/usr/share/doc/mailcrier/README.md` и `docs/` | эти страницы; их ссылки на `packaging/examples` там не открываются, примеры лежат рядом в `examples/` |
| `/usr/share/selinux/packages/mailcrier/mailcrier.cil` | модуль SELinux, rpm, см. [SELinux](selinux.ru.md) |

Установка создаёт системные группу и пользователя `mailcrier` (домашний
каталог `/var/spool/mailcrier`, без оболочки входа), если их нет. Дальше от
root:

1. Впишите цель в `/etc/mailcrier.conf`; как - показывают его комментарии
   и примеры. Секрет кладётся отдельно в файл в `/etc/mailcrier.d`,
   `root:mailcrier 0640`.
2. `mailcrier --check-config` сообщает об ошибках и предупреждениях, код
   выхода 0, если файл загружается.
3. `mailcrier --probe` отправляет тестовое письмо в каждую цель.

При обновлении rpm и apk возвращают `/etc/mailcrier.conf` владельца и
права `root:mailcrier 0640`, если содержимое файла совпадает с пакетным;
dpkg их сохраняет. Поэтому секрету место в файле в `/etc/mailcrier.d/`,
где пакет не поставляет и не меняет ни одного файла.

Пока цель не задана, каждое письмо задерживается в `hold/` с кодом выхода
78, а `mailcrier-queue.service` каждые 5 минут завершается с кодом 78 и
попадает в `systemctl --failed`; первый прогон очереди после исправления
конфигурации доставляет задержанные письма.

## Дистрибутивы

Таймер включается при установке и запускается там, где работает systemd;
где его нет, очередь запускает файл cron. Скрипты пакета никогда не
запускают mailcrier и никогда не трогают конфигурацию.

- Debian и Ubuntu: пакет предоставляет `mail-transport-agent`,
  конфликтует с ним и заменяет его, поэтому `apt install` удаляет прежний
  MTA (postfix, exim4, msmtp-mta, ...), а удаление mailcrier его не
  возвращает. `dpkg -i` ничего не удаляет и отказывается ставить пакет,
  пока установлен другой MTA: пакет ставится командой
  `apt install ./<file>.deb`. Бит setgid - запись `dpkg-statoverride`,
  которую dpkg применяет ко всем последующим версиям; запись, заданная
  администратором раньше, сохраняется.
- RHEL, Rocky, Alma, Fedora: ссылки - альтернатива `mta` с приоритетом
  100, выше postfix (60) и sendmail (90), поэтому после установки активен
  mailcrier; `alternatives --config mta` выбирает другой. После удаления
  снова активен оставшийся MTA. Пакет устанавливает модуль SELinux,
  который помечает mailcrier как штатный MTA, чтобы ограниченные
  вызывающие программы вроде smartd могли отправлять почту; хук при этом
  работает в почтовом домене своей вызывающей программы, см.
  [SELinux](selinux.ru.md). cronie при запуске решает, отправлять ли
  почтой вывод заданий: crond, запущенный, когда MTA не было, пишет этот
  вывод в журнал до `systemctl restart crond`, а установка, пока crond
  работает, выводит напоминание.
- Alpine: пакет заменяет ссылку BusyBox `/usr/sbin/sendmail`, и BusyBox
  возвращает её после удаления. ssmtp, dma и opensmtpd тоже владеют
  `/usr/sbin/sendmail`: apk отказывается его перезаписать (`trying to
  overwrite usr/sbin/sendmail owned by ...`), поэтому сначала удалите их.
  Очередь запускается из crond BusyBox, который Alpine по умолчанию не
  запускает: `rc-update add crond && rc-service crond start`. Без него
  недоставленное письмо повторяет только следующий вызов того же
  пользователя.

## Бит setgid в rpm и apk

В rpm и apk исполняемый файл распаковывается как `root:root 0755` при
установке и при каждом обновлении, а затем скрипт установки даёт ему
группу и бит setgid. Последствия:

- `rpm -V mailcrier` сообщает `.M....G..  /usr/sbin/mailcrier`, а `apk audit
  --check-permissions` сообщает `M usr/sbin/mailcrier`; так и должно быть.
- Во время обновления, между распаковкой и скриптом, вызов пользователя,
  отличного от root, не может прочитать конфигурацию, и `hold/` для него
  тоже закрыт на запись: письмо теряется (`message lost, not held`), а вызов
  завершается с кодом 73.
- `rpm --setperms`, `rpm --setugids`, `rpm --restore`, установка или
  обновление с `--noscripts`, `apk fix` и `apk add --no-scripts` снимают
  бит насовсем, с тем же результатом для каждого такого вызова, пока root
  его не восстановит:
  `chgrp mailcrier /usr/sbin/mailcrier && chmod 2755 /usr/sbin/mailcrier`
  (`chgrp` первым: он сбрасывает бит).

## Удаление

Удаление (`dpkg -r`, `rpm -e`, `apk del`) останавливает таймер. Оно
сохраняет пользователя, группу и изменённую конфигурацию (`dpkg -P` её
удаляет, `rpm -e` сохраняет как `/etc/mailcrier.conf.rpmsave`). Спул без
писем в `queue/`, `hold/` и `failed/` удаляется вместе с пакетом; спул с
письмами остаётся, и пакет любого формата выводит его путь при удалении
(deb - при `dpkg -r` и при `dpkg -P`). Вызов во время удаления может
оставить в спуле только `locks/`, а файлы, оставленные в `tmp/` прерванным
вызовом, тоже сохраняют спул (dpkg сообщает, что каталог не пуст); в
таком каталоге нет писем, и его можно удалить. Debian хранит
`/etc/cron.d/mailcrier` до `dpkg -P`; его задание ничего не делает, пока
исполняемого файла нет.

## Сборка

```sh
go build -o mailcrier ./cmd/mailcrier
```

С `-tags noshoutrrr` в исполняемый файл не входят библиотека shoutrrr и
12 модулей, которые она подключает, - около 3 MB из 13,7 MB (linux/amd64,
`-trimpath -ldflags='-s -w'`, go1.27.1); цель `shoutrrr` тогда отклоняет
конфигурацию с кодом выхода 78 и `built without shoutrrr` в журнале.
Релизы содержат обе сборки, меньшая называется `mailcrier-minimal`.

Исполняемый файл ставится в `/usr/sbin/mailcrier`, а `/usr/sbin/sendmail`,
`/usr/lib/sendmail`, `/usr/bin/newaliases` и `/usr/bin/mailq` - ссылки на
него; владелец и права описаны на странице [Привилегии](privileges.ru.md).
Страница руководства - `docs/mailcrier.8`.

English: [install.md](install.md).
