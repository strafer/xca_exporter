XCA exporter
============

[English](README.md) | [Русский](README.ru.md)


Экспортёр метрик в формате Prometheus для мониторинга сертификатов XCA, хранящихся в SQL-СУБД.

[XCA](https://hohnstaedt.de/xca/) — бесплатное приложение с графическим интерфейсом для управления инфраструктурой открытых ключей.

В настоящий момент экспортёр предоставляет информацию о сроках окончания действия сертификатов.
Экспортёр не требует XCA для своей работы — он подключается напрямую к СУБД и способен работать с PostgreSQL, MySQL и SQLite.


## Развитие программы

На данный момент не планируется добавление новой функциональности, только исправление ошибок и обновление зависимостей при обнаружении уязвимостей.

Вы можете прислать Pull-request, но я не гарантирую его вливание. Но благодаря лицензии у вас всегда остаётся возможность создать и поддерживать свой собственный форк.


## Установка и использование

`xca_exporter` по умолчанию прослушивает HTTP-порт 9793. См. вывод справки посредством опции `--help` для изучения всех возможных параметров.

Конфигурация экспортёра производится (в порядке увеличения приоритета) посредством конфигурационного файла формата YAML, переменных окружения и аргументов командной строки.


### Способы запуска

#### Systemd-сервис

Пакет программы отсутствует в репозиториях дистрибутивов, поэтому в этом случае потребуется ручные загрузка бинарного файла и настройка сервиса.

Имеющийся в репозитории юнит-файл [xca_exporter.service](xca_exporter.service) может быть использован как основа для создания собственного сервиса.


#### Контейнер

Пример создания и запуска контейнера:

```console
podman run \
  --detach \
  --name xca-exporter \
  --restart=unless-stopped \
  --read-only \
  --publish 9793:9793 \
  ghcr.io/strafer/xca-exporter:<версия>
```

(тег `latest` не создаётся сознательно, т.к. его использование является плохой практикой).

При использовании SQLite потребуется также примонтировать для чтения в контейнер файл БД.


## Работа с СУБД

### Создание учётных записей для экспортёра

Это примеры, требущие изменения параметров под конкретные условия.

#### PostgreSQL

```sql
CREATE ROLE xca_exporter
  LOGIN
  PASSWORD '<password>';

GRANT CONNECT
  ON DATABASE xca
  TO xca_exporter;

\c xca

GRANT USAGE
  ON SCHEMA public
  TO xca_exporter;

GRANT SELECT
  ON TABLE
    public.xca_certs,
    public.xca_items
  TO xca_exporter;
```


#### MySQL

```sql
CREATE USER 'xca_exporter'@'<host>'
IDENTIFIED BY '<password>';

GRANT SELECT
  ON xca.xca_certs
  TO 'xca_exporter'@'<host>';

GRANT SELECT
  ON xca.xca_items
  TO 'xca_exporter'@'<host>';
```


### Драйвер

Строка подключения должна соответствовать формату, поддерживаемому используемым драйвером.
Экспортёр определяет используемый драйвер по префиксу строки подключения:

| Префикс                           | СУБД       | Драйвер                                                       |
|:---                               |---         | ---                                                           |
| `postgres://` или `postgresql://` | PostgreSQL | [pgx/v5](https://pkg.go.dev/github.com/jackc/pgx/v5)          |
| `mysql://`                        | MySQL      | [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) |
| `sqlite://`                       | SQLite     | [modernc.org/sqlite](https://modernc.org/sqlite)              |

Версии драйверов СУБД конкретной сборки бинарного файла можно получить с помощью опции `--versions`.


### Примеры строк подключения

#### PostgreSQL

```text
postgresql://xca@db.example/xca
```

#### MySQL

```text
mysql://xca:secret@tcp(db.example:3306)/xca
```

#### SQLite

```text
sqlite:///var/lib/xca/xca.xdb
```

или

```text
sqlite://file:/var/lib/xca/xca.xdb
```


### Проверка БД при запуске

По умолчанию экспортёр при запуске перед созданием HTTP-сервера выполняет проверку доступности СУБД и проверку возможности чтения таблиц с последующим выводом результатов проверки, но возникшие ошибки не приводят к завершению работы. Проверку можно отключить или изменить реакцию на её ошибки.


## Сбор метрик

При каждом запросе метрик выполняется сбор данных из БД. Если во время сбора поступают дополнительные запросы метрик, они ожидают его завершения и получают тот же результат. Результаты сбора не кешируются между последовательными запросами.

Ошибки подключения к СУБД или полный провал SQL-запроса приводят к HTTP-ответу с кодом 500.

Частичные проблемы с конкретными сертификатами не приводят к полному отказу отдачи метрик, но каждая встреченная проблема увеличивает счётчик метрики `xca_exporter_collect_error`, однако это не возрастающее в ходе всей работы экспортёра значение и оно сбрасывается при каждом новом сборе — его нулевое значение показывает, что в ходе данного сбора проблем не было обнаружено.

Из результатов сбора исключаются сертификаты, помеченные в БД как удалённые.


## Метрики

| Метрика                      | Описание                                                                                               |
|---                           |---                                                                                                     |
| `xca_exporter_collect_error` | Количество ошибок, возникших при обработке текущего запроса метрик.                                    |
| `xca_certificate_not_after`  | Время окончания действия сертификата в формате Unix timestamp (количество секунд с начала эпохи Unix). |


## Пример отдаваемых метрик

```prometheus
# HELP xca_exporter_collect_error Number of errors occurred during collection.
# TYPE xca_exporter_collect_error gauge
xca_exporter_collect_error 0

# HELP xca_certificate_not_after Certificate expiration timestamp.
# TYPE xca_certificate_not_after gauge
xca_certificate_not_after{xca_id="1",xca_name="Root CA",c_name="Intermediate CA",d_name="example.com"} 1798761600
xca_certificate_not_after{xca_id="2",xca_name="Root CA",c_name="Intermediate CA",d_name="api.example.com"} 1801440000
xca_certificate_not_after{xca_id="3",xca_name="Root CA",c_name="Intermediate CA",d_name="mail.example.com"} 1804032000
```


### Использование

#### Сбор через Prometheus

Пример статичной конфигурации сбора:

```yaml
scrape_configs:
  - job_name: xca
    static_configs:
      - targets:
          - localhost:9793
```


#### PromQL-запрос

Пример запроса сертификатов, срок действия которых уже истёк или истечёт в ближайшие 30 дней:

```promql
(xca_certificate_not_after - time()) < 30 * 24 * 60 * 60
```


## TLS

По умолчанию экспортёр работает по протоколу HTTP, но также поддерживает HTTPS (включая возможность использования mTLS для аутентификации клиента) через отдельный файл конфигурации.

Пример настройки:

```console
./xca_exporter --web.config.file=web-config.yml
```

См. [exporter-toolkit web-configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) для подробностей.
