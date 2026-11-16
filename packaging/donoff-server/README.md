# donoff-server

Локальный сервер розеток donoff на NAPI (Armbian, Ubuntu, Debian; arm64 и amd64):

- **брокер** mosquitto на порту 1883 для локальной сети. avahi объявляет его как `_mqtt._tcp`
  с `donoff=1`, розетки donoff2 находят его сами (при `mqttlocal=1`, по умолчанию);
- **история** InfluxDB 2 (пакет `influxdb2` из repo.napilab.ru), org и bucket `donoff`,
  настраивается при установке;
- **панель** donoff-panel: плитки розеток, команды, группы, журнал и графики истории на
  `http://<плата>:8090`.

## Установка

```sh
apt install donoff-server
```

apt спросит пользователя брокера (по умолчанию `donoff`) и его пароль: тот, что задан в
розетках. Под этим же пользователем подключается панель: у donoff корень топиков равен имени
пользователя (`/donoff/<розетка>/...`). Пустой пароль: задать потом `donoff-server passwd`,
до этого панель не запускается.

## Команды

```sh
donoff-server show                      # службы, адрес панели, пользователи, мост
donoff-server passwd [user]             # пароль пользователя брокера (по умолчанию тот, под кем панель)
donoff-server deluser <user>
donoff-server bridge on tav1 lamp1      # мост на mqtt.lab240.ru:8883 (TLS) для этих розеток
donoff-server bridge on all             # или все /donoff/#
donoff-server bridge off
donoff-server influx-setup              # первая настройка InfluxDB (делает пакет)
```

Мост нужен, чтобы центральная панель на lab240 тоже видела розетки; по умолчанию выключен.
`bridge on` спрашивает пароль на lab240 (`--user`, `--host` меняют пользователя и адрес).

## Файлы

| Файл | Что |
|---|---|
| `/etc/default/donoff-panel` | флаги панели: порт, опрос, доверенные сети |
| `/etc/donoff-server/mqtt.creds` | пользователь и пароль панели в брокере (тот же, что у розеток) |
| `/etc/donoff-server/influx.creds` | токен панели в InfluxDB |
| `/etc/donoff-server/influx-admin` | вход в веб InfluxDB (`admin`), только root |
| `/etc/mosquitto/conf.d/donoff-server.conf` | listener 1883, без анонимов |
| `/etc/mosquitto/conf.d/donoff-bridge.conf` | мост (пишет `bridge on`) |
| `/etc/mosquitto/donoff.passwd` | пользователи брокера |
| `/var/lib/donoff-panel/` | состояние панели: устройства, группы, комментарии, журнал |

Если порт 8086 занят другой программой (на некоторых NAPI его держит `lic_host` EliCont),
InfluxDB ставится на следующий свободный, 8087 и дальше: `/etc/systemd/system/influxdb.service.d/donoff-port.conf`,
`donoff-server show` показывает порт.

InfluxDB ужата под 512 МБ памяти (`/usr/lib/systemd/system/influxdb.service.d/donoff-server.conf`).
`apt purge donoff-server` удаляет настройки и состояние панели. Данные InfluxDB остаются, а с ними
и её токен (`influx.creds`, `influx-admin`): новая установка пишет в ту же историю.

## Сборка

```sh
packaging/donoff-server/build-deb.sh build arm64 amd64
```
