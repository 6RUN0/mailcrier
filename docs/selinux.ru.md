# SELinux

Что пакет rpm делает для SELinux, что это меняет для вызывающих программ
и хуков и как это измеряет проверка на Rocky. Пакеты deb и apk для SELinux
ничего не делают.

## Модуль пакета rpm

Без собственной политики `/usr/sbin/mailcrier` имеет тип `bin_t` и
работает в домене вызывающей программы. Ограниченная вызывающая программа
теряет письма: smartd запускает его в `fsdaemon_t` (Rocky 9) или
`smartdwarn_t` (Rocky 10), где запрещены и спул, и сеть. Поэтому rpm
поставляет [`mailcrier.cil`](../packaging/selinux/mailcrier.cil) как
`/usr/share/selinux/packages/mailcrier/mailcrier.cil`, и mailcrier
получает типы штатного MTA:

| Правило | Зачем |
|---|---|
| `/usr/sbin/mailcrier` и `/usr/bin/mailcrier` - `sendmail_exec_t` | ограниченная вызывающая программа, которой разрешено отправлять почту, переходит в `system_mail_t`; Rocky 10 ищет пути под `/usr/sbin` как `/usr/bin` |
| `/var/spool/mailcrier(/.*)?` - `mqueue_spool_t` | `system_mail_t` и `sendmail_t` не могут писать в `var_spool_t` |
| `init_t` может войти в `sendmail_t` при NoNewPrivileges | `mailcrier-queue.service` работает с `NoNewPrivileges=yes` |
| `system_mail_t` может вызывать `setpgid` | хук работает в собственной группе процессов |

`%posttrans` устанавливает модуль с приоритетом 200, приоритетом модулей
пакетов, при каждой установке и каждом обновлении, где есть
`/etc/selinux/config`, а затем, если SELinux включён, переразмечает
исполняемый файл и спул, письма старой версии тоже. Если `semodule` нет
или он завершился ошибкой, скрипт выводит

```text
mailcrier: SELinux module not installed; to install it run: semodule -X 200 -i /usr/share/selinux/packages/mailcrier/mailcrier.cil
```

и установка продолжается: исполняемый файл остаётся `bin_t`, и
ограниченные вызывающие программы теряют письма, как без модуля. Хост без
`/etc/selinux/config` (контейнер) не получает ничего. `rpm -e` удаляет
модуль только с приоритетом 200.

Модуль администратора с именем `mailcrier` и приоритетом по умолчанию 400
заменяет модуль пакета, а не дополняет его: собственные правила называются
иначе, ниже - `mailcrier_local`. Удалить модуль вручную -
`semodule -X 200 -r mailcrier`, затем
`restorecon -RF /usr/sbin/mailcrier /var/spool/mailcrier`; с этого момента
письма от ограниченных вызывающих программ снова теряются.

Каталог спула, отличный от `/var/spool/mailcrier` (`[spool] dir`), модуль
не покрывает, и его нужно разметить так же
(`semanage` входит в `policycoreutils-python-utils`):

```sh
semanage fcontext -a -e /var/spool/mailcrier /srv/mailcrier-spool
restorecon -R /srv/mailcrier-spool
```

## Домены вызывающих программ

Измерено на Rocky 9.8 (`selinux-policy-targeted-38.1.75-2.el9_8.1`) и
Rocky 10.2 (`selinux-policy-targeted-42.1.18-4.el10_2.4`) с установленным
модулем проверкой ниже; перемеряется с каждой записью в разделе
"Результат".

| Вызывающая программа | Домен mailcrier и его хука |
|---|---|
| root в оболочке, пользовательский crontab | `unconfined_t`, как без модуля |
| почта, которую crond отправляет с выводом задания, smartd | `system_mail_t` |
| `mailcrier-queue.service`, служба без NoNewPrivileges | `sendmail_t` |
| служба с `NoNewPrivileges=yes` | её собственный домен: ядро отказывает в переходе, как и для postfix, и вызов работает |

В `system_mail_t` и `sendmail_t` mailcrier подключается к любому порту
TCP, читает удостоверяющие центры в `/etc/pki` и разрешает имена:
измерено с http на портах 80 (`http_port_t`), 8080 (`http_cache_port_t`)
и 2586 (`unreserved_port_t`) и с https по имени из `/etc/hosts`.

Смена домена сбрасывает обработку сигналов, заданную вызывающей программой
(разрешение `siginh` между доменами не выдаётся, политика лишь скрывает
отказ): вызов из программы, которая игнорирует SIGHUP, как `nohup`, в
mailcrier его не игнорирует. То же верно для любого MTA с меткой
`sendmail_exec_t`.

### Хуки

Хук (`type = "exec"`) работает в домене вызова, который доставляет письмо,
поэтому один и тот же хук может работать из оболочки и не срабатывать,
когда письмо отправляет cron или smartd или когда его повторяет служба
очереди. В `system_mail_t` и `sendmail_t` ему доступны сеть и запуск
программ с меткой `bin_t` (`/usr/bin`, `/usr/local/bin`): хук, который
запускает curl к приёмнику http, проверен с каждой вызывающей программой
из таблицы.
Штатная политика этих доменов не даёт ему писать файлы вне почтовых спулов
(`/var/log`, `/home`, `/srv`), управлять службами или обращаться к сокетам
других служб; эта часть не измерялась для каждого хука по отдельности.

Если хуку там отказано, доставка не удаётся: строка журнала называет
цель (`target failed ... target=<name> class=perm err="hook ..."`), а в
журнале аудита есть отказ с `comm` программы, которую запустил хук. Чтобы
пропустить его, напишите собственный модуль по этим отказам, от root,
после того как хук не сработал из cron или smartd:

```sh
ausearch -m avc -ts recent -c <program> --raw | audit2allow -M mailcrier_local
semodule -i mailcrier_local.pp
```

`audit2allow` входит в `policycoreutils-python-utils`. Перед установкой
проверьте, что модуль разрешает (`mailcrier_local.te`): правило для
`system_mail_t` действует для любой почты от ограниченной вызывающей
программы, а не только для mailcrier.

## Проверка

Проверка запускает вызывающие программы на виртуальной машине с SELinux в
режиме enforcing и собирает отказы; контейнер их показать не может, потому
что политика принадлежит ядру хоста. Её результат попадает в раздел
"Результат" ниже, по одному прогону на мажорную версию Rocky.

### Запуск

```sh
make snapshot
make -j2 selinux-rocky9 selinux-rocky10
```

Нужны docker, доступный на чтение и запись `/dev/kvm` на хосте x86-64-v3
(Rocky 10) и сеть: первый запуск скачивает облачные образы Rocky в
`.e2e/selinux` (около 600 MB каждый, сверяются с суммами в `Makefile`) и
пакеты релиза `SMOKE_PREVIOUS` (`make smoke-previous`), а виртуальная
машина ставит свои пакеты с зеркал Rocky. Каждая цель make загружает
образ под qemu в контейнере `testdata/selinux` с одноразовым
диском-оверлеем, диском IDE со SMART для smartd и 2 GB памяти, входит
как root по ssh,
запускает [`check.sh`](../testdata/selinux/check.sh) на rpm из `dist/` и
rpm предыдущего релиза и выключает машину; прогон занимает от 15 до 20
минут. Затем `TestSELinux` (`selinux_test.go`, тег `selinux`) читает
`.e2e/selinux/<distro>/check.log`. Если машина не загрузилась или sshd
недоступен, `run.sh` сообщает об этом с концом `console.log`; диск-оверлей
`disk.qcow2` остаётся рядом с ним до следующего запуска.

Без docker или KVM `check.sh` запускается вручную от root на виртуальной
машине Rocky (не в контейнере), из сеанса входа, с
[`receiver.py`](../testdata/selinux/receiver.py) рядом:

```sh
sh check.sh ./mailcrier_<version>_linux_amd64.rpm \
  ./mailcrier_<previous>_linux_amd64.rpm 2>&1 | tee check.log
go test -tags selinux -run '^TestSELinux$' . -args -selinux-log check.log
```

Скрипт меняет машину: добавляет пользователя `smoketest`, удостоверяющий
центр в `/etc/pki/ca-trust` и `selinux.test` в `/etc/hosts`, перезаписывает
конфигурацию пакета и оставляет установленными at, cronie, smartmontools,
s-nail, python3, openssl и обновлённый `selinux-policy`. Ловушка выхода
удаляет mailcrier, postfix, пользователя, хук и записи cron, возвращает
`smartd.conf` и снова включает правила dontaudit, в том числе после
неудачного шага. Скрипт снимает `ConditionVirtualization=no` с юнита
smartd через drop-in в `/run` и перезапускает crond и atd после каждой
смены MTA (crond, запущенный до появления `/usr/sbin/sendmail`, пишет
вывод заданий в журнал, а не отправляет его почтой). На машине без диска
со SMART smartd ничего не отправляет, и тест на этом падает.

### Что проверяется

`check.sh` сначала обновляет `selinux-policy`, так что прогон проверяет
политику, которую администратор получает в этот день, и записывает её
версию. Он выключает правила dontaudit (`semodule -DB`), чтобы ни один
отказ не был скрыт, затем:

1. запускает почту crond для пользовательского и системного задания,
   smartd и две временные службы, с NoNewPrivileges и без, через postfix;
   её отказы - мера того, что штатная политика даёт любому MTA;
2. устанавливает предыдущий релиз и ставит письмо в очередь;
3. обновляется до проверяемого rpm, чей `%posttrans` устанавливает модуль;
4. вызывает mailcrier от root, из пользовательского crontab, из почты
   crond, из at, из smartd и из обеих служб, каждый раз к четырём целям
   (http на 80, 8080 и 2586, https по имени на 8443) и к хуку;
5. даёт службе очереди доставить письмо предыдущего релиза и письмо,
   поставленное в очередь, пока приёмник не работал;
6. удаляет пакет и ищет модуль.

Тест падает, когда:

- скрипт работал не в `unconfined_t`, SELinux не в режиме enforcing или
  crond, atd или smartd работают вне `crond_t` (на Rocky и atd:
  политика размечает его как `crond_exec_t`), `atd_t` и `fsdaemon_t`;
- postfix или mailcrier не доставил письмо за время своего ожидания или
  вызов завершился с ненулевым кодом;
- исполняемый файл, спул или письмо предыдущего релиза в очереди размечены
  не так, как выше, модуль стоит не с приоритетом 200 или остаётся после
  удаления;
- хук работал не в том домене, который показывает таблица выше, или с
  группой `mailcrier`;
- не найдено ни одной записи `MAC_POLICY_LOAD` с начала прогона, так что
  пустой поиск ничего бы не доказал;
- отказ называет mailcrier, его пути или вызывающую программу проверки
  (comm `sendmail`, `exe` после повторного запуска, `mail`, `s-nail`,
  `selinux-hook`, `curl`), а postfix не получил того же отказа (те же
  типы источника и цели, класс и разрешение) на шаге 1.

Отказ любого другого процесса, а также отказ, который получает и postfix,
тест выводит, но не считает ошибкой: с выключенными правилами dontaudit
свои отказы дают dnf, sshd и systemd, а переход в `system_mail_t` даёт
`noatsecure`, `rlimitinh` и `siginh` для любого MTA.

## Результат

Для каждого прогона: версия Rocky, вывод `rpm -q mailcrier` и коммит, из
которого собран rpm, строки `policy`, `cron_userdomain_transition`,
`module` и `hook` из `check.log` и отказы, которые вывел тест. Никаких
имён хостов, адресов или путей вне виртуальной машины.

### 960edf2, 2026-10-09

`make -j2 selinux-rocky9 selinux-rocky10` прошёл на обеих версиях, все 48
ожиданий `ok`; rpm `mailcrier-0.1.0~rc1791525644-1.x86_64`, обновлён с
`mailcrier-0.1.0~rc.2-1.x86_64`, модуль `200 cil`, после `rpm -e`
отсутствует; `cron_userdomain_transition on`.

| | Rocky 9.8 | Rocky 10.2 |
|---|---|---|
| `policy` | `selinux-policy-targeted-38.1.75-2.el9_8.1` | `selinux-policy-targeted-42.1.18-4.el10_2.4` |
| crond, atd | `crond_t` | `crond_t` |
| smartd | `fsdaemon_t` | `fsdaemon_t` |

Хук работал, как в таблице раздела "Домены вызывающих программ", на
обеих версиях: почта crond и smartd в `system_mail_t`, служба очереди
(группа `mailcrier`) и служба без NoNewPrivileges в `sendmail_t`, служба
с ним в `initrc_t`, root и пользовательский crontab в `unconfined_t`.

Отказы mailcrier и его вызывающих программ, каждый из которых postfix
тоже получил на шаге 1:

| Источник -> цель | Класс и разрешение | Rocky |
|---|---|---|
| `fsdaemon_t` -> `system_mail_t` | `process` `noatsecure`, `rlimitinh`, `siginh` | 9 |
| `smartdwarn_t` -> `system_mail_t` | `process` `noatsecure`, `rlimitinh`, `siginh` | 10 |
| `system_mail_t` -> `init_t` | `unix_stream_socket` `read write` (stdout процесса crond, comm `sendmail`) | 9, 10 |
| `initrc_t` -> `sendmail_t` | `process2` `nnp_transition` (служба с NoNewPrivileges) | 9, 10 |
| `smartdwarn_t` -> `fs_t` | `filesystem` `getattr` (comm `mail`, s-nail) | 10 |

Отказы других процессов при выключенных правилах dontaudit:
`setroubleshootd_t` пишет в `rpm_var_lib_t` (comm `rpm`),
`systemd_logind_t` (9) или `systemd_user_runtimedir_t` (10) запрашивает
`net_admin`, `init_t` читает `shadow_t` (9), `systemd_gpt_generator_t`
запрашивает `sys_admin`, а `kdump_dep_generator_t` читает
`passwd_file_t` (10).

English: [selinux.md](selinux.md).
