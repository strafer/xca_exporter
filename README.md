# XCA exporter

[English](README.md) | [Русский](README.ru.md)


A Prometheus metrics exporter for monitoring XCA certificates stored in SQL databases.

[XCA](https://hohnstaedt.de/xca/) is a free application with a graphical user interface for managing public key infrastructures.

Currently, the exporter provides information about certificate expiration dates.
The exporter does not require XCA to operate — it connects directly to the database and supports PostgreSQL, MySQL and SQLite.


## Project development

No new features are currently planned. Only bug fixes and dependency updates in response to discovered vulnerabilities are planned.

You are welcome to submit a Pull Request, but I cannot guarantee that it will be merged. However, the license always allows you to create and maintain your own fork.


## Installation and usage

By default, `xca_exporter` listens on HTTP port 9793. See the help output using the `--help` option for a list of all available parameters.

The exporter can be configured using a YAML configuration file, environment variables and command-line arguments (in increasing order of precedence).


### Running the exporter

#### Systemd service

The application is not available in distribution repositories, so manual downloading of the binary and service configuration are required in this case.

The [xca_exporter.service](xca_exporter.service) unit file included in the repository can be used as a starting point for creating your own service.


#### Container

Example of creating and running a container:

```console
podman run \
  --detach \
  --name xca-exporter \
  --restart=unless-stopped \
  --read-only \
  --publish 9793:9793 \
  ghcr.io/strafer/xca-exporter:<version>
```

(The `latest` tag is deliberately not published, as its use is considered bad practice.)

When using SQLite, the database file must also be mounted into the container as read-only.


## Working with databases

### Creating database accounts for the exporter

These are examples that require adjusting the parameters to match your environment.

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


### Driver

The connection string must follow the format supported by the respective driver.
The exporter determines which driver to use based on the connection string prefix:

| Prefix                            | Database   | Driver                                                        |
|:---                               |---         | ---                                                           |
| `postgres://` or `postgresql://`  | PostgreSQL | [pgx/v5](https://pkg.go.dev/github.com/jackc/pgx/v5)          |
| `mysql://`                        | MySQL      | [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) |
| `sqlite://`                       | SQLite     | [modernc.org/sqlite](https://modernc.org/sqlite)              |

The versions of the database drivers included in a particular binary build can be obtained using the `--versions` option.


### Connection string examples

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

or

```text
sqlite://file:/var/lib/xca/xca.xdb
```


### Database check on startup

By default, before starting the HTTP server, the exporter checks database availability and verifies that the required tables can be read, then prints the results of these checks. However, any errors encountered do not cause the exporter to exit. The check can be disabled, or the behavior on check failure can be changed.


## Metrics collection

Each metrics request triggers a fresh data collection from the database. If additional metrics requests arrive while collection is in progress, they wait for it to finish and receive the same result. Collection results are not cached between consecutive requests.

Database connection errors or complete SQL query failures result in an HTTP 500 response.

Problems with individual certificates do not cause the entire metrics request to fail, but each encountered problem increments the `xca_exporter_collect_error` metric. However, this value does not accumulate over the lifetime of the exporter and is reset on each new collection — a value of zero indicates that no problems were detected during that collection.

Certificates marked as deleted in the database are excluded from collection results.


## Metrics

| Metric                        | Description                                                                                              |
|---                            |---                                                                                                      |
| `xca_exporter_collect_error`  | Number of errors encountered while processing the current metrics request.                              |
| `xca_certificate_not_after`   | Certificate expiration time as a Unix timestamp (number of seconds since the Unix epoch).               |


## Example metrics output

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


### Usage

#### Scraping with Prometheus

Example of a static scrape configuration:

```yaml
scrape_configs:
  - job_name: xca
    static_configs:
      - targets:
          - localhost:9793
```


#### PromQL query

Example query for certificates that have already expired or will expire within the next 30 days:

```promql
(xca_certificate_not_after - time()) < 30 * 24 * 60 * 60
```


## TLS

By default, the exporter uses HTTP, but it also supports HTTPS (including mTLS for client authentication) through a separate configuration file.

Example configuration:

```console
./xca_exporter --web.config.file=web-config.yml
```

See [exporter-toolkit web-configuration](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md) for details.
