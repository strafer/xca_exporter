package main

import (
  "context"
  "crypto/x509"
  "encoding/base64"
  "errors"
  "fmt"
  "io"
  "log/slog"
  "runtime/debug"
  "strconv"
  "strings"
  "regexp"

  "database/sql"

  _ "github.com/go-sql-driver/mysql"
  _ "github.com/jackc/pgx/v5/stdlib"
  "github.com/prometheus/client_golang/prometheus"
  "golang.org/x/sync/singleflight"
  _ "modernc.org/sqlite"
)


// SpecificCLI contains only options belonging to the concrete exporter.
// These fields are also available through --config.file and environment
// variables because main.go embeds SpecificCLI into its Kong grammar.
type SpecificCLI struct {
  Web struct {
    ListenAddress string `help:"Address to listen on for the HTTP server and metrics."  default:":9793"`
  } `embed:"" prefix:"web."`

  Database struct {
    ConnectionString string `help:"XCA database connection string. Supported prefixes: postgresql://, mysql:// and sqlite://; after the prefix the backend-native connection string is passed through unchanged."  default:""`

    TablePrefix string `help:"Prefix used by XCA for its database tables."  default:"xca_"`

    TestOnStart struct {
      Enabled bool `help:"Test database connectivity and read access to the XCA certificates table startup test (default: ${default})."  default:"true"`

      Timeout NonNegativeDuration `help:"Database connectivity and read access startup test timeout."  default:"10s"`

      ExitOnFailure bool `help:"Exit with a non-zero status if the startup database test fails (default: ${default})."  default:"false"`
    } `embed:"" prefix:"test-on-start."`
  } `embed:"" prefix:"database."`

  Versions bool `help:"Print versions of all database driver libraries used by the exporter and exit."  env:"-" configfile:"false"`
}


// exporterDefinition is the concrete exporter definition.
var exporterDefinition = ExporterDefinition{
  Namespace:            "xca",
  Name:                 "xca_exporter",
  Description:          "Prometheus exporter for certificates stored in an XCA database.\n\nProject home page: https://github.com/strafer/xca_exporter",
  DefaultListenAddress: ":9793",
  NewCollector:         NewSpecificCollector,
}


// certificateData stores information about one certificate
// retrieved from the database.
type certificateData struct {
  xcaID      int64
  xcaName    string
  commonName string
  dn         string
  notAfter   int64
}


// collectionResult stores the result of processing
// the entire set of certificates from the database.
type collectionResult struct {
  certificates []certificateData
  decodeErrors float64
}


// specificCollector contains all the dependencies and settings
// required for the exporter to work.
type specificCollector struct {
  logger *slog.Logger
  db     *sql.DB

  tables map[string]string

  query string

  collectErrorDesc *prometheus.Desc
  notAfterDesc     *prometheus.Desc

  collectGroup singleflight.Group
}


// moduleVersion extracts information about the version of a specified library
// from the metadata of a compiled binary file.
func moduleVersion(
  path string,
) string {
  info, ok := debug.ReadBuildInfo()
  if !ok {
    return "unknown"
  }

  for _, dep := range info.Deps {
    if dep.Path != path {
      continue
    }
    if dep.Replace != nil {
      if dep.Replace.Version != "" {
        return dep.Replace.Version
      }
      return dep.Replace.Path
    }
    if dep.Version != "" {
      return dep.Version
    }
    return "unknown"
  }

  return "unknown"
}


// HandleSpecialArgs in this exporter reports the versions of the libraries
// used to connect to the DBMS.
func (c SpecificCLI) HandleSpecialArgs(
  w io.Writer,
) (
  bool,
  error,
) {
  if !c.Versions {
    return false, nil
  }

  versions := []struct {
    name   string
    module string
  }{
    {
      name:   "pgx",
      module: "github.com/jackc/pgx/v5",
    },
    {
      name:   "mysql driver",
      module: "github.com/go-sql-driver/mysql",
    },
    {
      name:   "sqlite",
      module: "modernc.org/sqlite",
    },
  }
  
  for _, version := range versions {
      if _, err := fmt.Fprintf(w, "%s: %s\n", version.module, moduleVersion(version.module)); err != nil {
          return true, fmt.Errorf("write %s version: %w", version.name, err)
      }
  }

  return true, nil
}


// NewSpecificCollector creates and prepares the exporter.
func NewSpecificCollector(
  config SpecificCLI,
  logger *slog.Logger,
) (
  SpecificCollector,
  error,
) {

  if config.Database.ConnectionString == "" {
    return nil, errors.New("database.connection-string must not be empty")
  }

  // Verify the specified table prefix, as it will be inserted into SQL queries.
  if err := validateTablePrefix(config.Database.TablePrefix); err != nil {
    return nil, err
  }

  // Identify the required driver and construct
  // the connection string it expects.
  driverName, nativeConnectionString, err := driverAndConnectionString(config.Database.ConnectionString)
  if err != nil {
    return nil, err
  }

  // Create a database connection object.
  db, err := sql.Open(driverName, nativeConnectionString)
  if err != nil {
    return nil, fmt.Errorf("open database connection: %w", err)
  }

  // A single XCA exporter scrape needs only one database operation at a time:
  // concurrent scrapes are coalesced by singleflight below. Keeping one
  // database connection also prevents accidental pool growth.
  db.SetMaxOpenConns(1)
  db.SetMaxIdleConns(1)

  // Creates the main structure of the exporter.
  c := &specificCollector{
    logger: logger,
    db:     db,
    tables: map[string]string{
      "certs": config.Database.TablePrefix + "certs",
      "items": config.Database.TablePrefix + "items",
    },
    collectErrorDesc: prometheus.NewDesc(
      "xca_exporter_collect_error",
      "Number of certificate decoding errors encountered during the current scrape.",
      nil,
      nil,
    ),
    notAfterDesc: prometheus.NewDesc(
      "xca_certificate_not_after",
      "Certificate expiration time as a Unix timestamp in UTC.",
      []string{"xca_id", "xca_name", "c_name", "d_name"},
      nil,
    ),
  }

  // XCA marks deleted objects in items.del instead of physically deleting
  // them. Exclude such objects from monitoring.
  c.query = fmt.Sprintf(`
SELECT
    c.item,
    i.name,
    c.cert
FROM %s AS c
JOIN %s AS i
  ON i.id = c.item
WHERE i.del = 0
`, c.tables["certs"], c.tables["items"])

  // Check the availability of the DBMS, if specified in the configuration.
  if config.Database.TestOnStart.Enabled {
    if err := c.testDatabase(context.Background(), config.Database.TestOnStart.Timeout); err != nil {
      logger.Error(
        "database startup test failed",
        "error", err,
        "exit_on_failure", config.Database.TestOnStart.ExitOnFailure,
      )

      // Terminate execution if the initial database availability check fails,
      // provided this is specified in the configuration.
      if config.Database.TestOnStart.ExitOnFailure {
        if closeErr := db.Close(); closeErr != nil {
          err = errors.Join(
            err,
            fmt.Errorf("close database after startup test failure: %w", closeErr),
          )
        }
        return nil, err
      }
    } else {
      logger.Info(
        "database startup test succeeded",
        "driver", driverName,
      )
    }
  }

  return c, nil
}


// testDatabase connects to the database and checks whether the required tables
// can be read.
func (c *specificCollector) testDatabase(
  parent context.Context,
  testOnStartTimeout NonNegativeDuration,
) error {
  ctx := context.Background()

  if testOnStartTimeout > 0 {
    var cancel context.CancelFunc
    ctx, cancel = context.WithTimeout(
      ctx,
      testOnStartTimeout.Duration(),
    )
    defer cancel()
  }

  // Define a simple SQL query template to perform a initial check
  // of DBMS availability and the ability to read the required tables.
  const testQueryTemplate = "SELECT 1 FROM %s LIMIT 1"

  for _, table := range c.tables {
    var discard any

    err := c.db.QueryRowContext(
      ctx,
      fmt.Sprintf(testQueryTemplate, table),
    ).Scan(&discard)

    if err != nil && !errors.Is(err, sql.ErrNoRows) {
      return fmt.Errorf("test table %q: %w", table, err)
    }
  }

  return nil
}


// newCertificateMetric receives a metric description and data for
// a single certificate, then creates a Prometheus metric for it.
func newCertificateMetric(
  desc *prometheus.Desc,
  xcaID int64,
  xcaName string,
  commonName string,
  dn string,
  notAfter int64,
) prometheus.Metric {
  return prometheus.MustNewConstMetric(
    desc,
    prometheus.GaugeValue,
    float64(notAfter),
    strconv.FormatInt(xcaID, 10),
    xcaName,
    commonName,
    dn,
  )
}


// Collect creates metrics from certificates obtained from the database.
func (c *specificCollector) Collect(
  ctx context.Context,
) (
  []prometheus.Metric,
  error,
) {
  // Access the database only if there isn't already another access request
  // in progress, otherwise wait for the its result and use it.
  value, err, _ := c.collectGroup.Do("certificates", func() (any, error) {
    return c.collectFromDatabase(ctx)
  })
  if err != nil {
    return nil, err
  }

  result, ok := value.(collectionResult)
  if !ok {
    return nil, errors.New("unexpected internal collection result type")
  }

  // Create an empty slice of metrics to avoid the inefficiencies
  // of incremental growth.
  metrics := make([]prometheus.Metric, 0, len(result.certificates)+1)

  // Iterate over the certificates obtained from the database,
  // converting each one into a metric.
  for _, certificate := range result.certificates {
    metrics = append(metrics, newCertificateMetric(
      c.notAfterDesc,
      certificate.xcaID,
      certificate.xcaName,
      certificate.commonName,
      certificate.dn,
      certificate.notAfter,
    ))
  }

  // This is deliberately a Gauge rather than a Counter. Its value describes
  // the current scrape: 0 means no certificate decode errors occurred in this
  // scrape; N means N certificates were rejected during this scrape.
  metrics = append(metrics, prometheus.MustNewConstMetric(
    c.collectErrorDesc,
    prometheus.GaugeValue,
    result.decodeErrors,
  ))

  return metrics, nil
}


// decodeCertificate converts the certificate from Base64 to X.509.
func decodeCertificate(
  encoded string,
) (
  *x509.Certificate,
  error,
) {
  der, err := base64.StdEncoding.DecodeString(encoded)
  if err != nil {
    return nil, fmt.Errorf("decode certificate base64: %w", err)
  }

  cert, err := x509.ParseCertificate(der)
  if err != nil {
    return nil, fmt.Errorf("parse X.509 certificate: %w", err)
  }

  return cert, nil
}


// collectFromDatabase retrieves certificate data from the database.
func (c *specificCollector) collectFromDatabase(
  ctx context.Context,
) (
  result collectionResult,
  retErr error,
) {
  rows, err := c.db.QueryContext(ctx, c.query)
  if err != nil {
    return collectionResult{}, fmt.Errorf("query XCA certificates: %w", err)
  }

  defer func() {
    if closeErr := rows.Close(); closeErr != nil {
      retErr = errors.Join(
        retErr,
        fmt.Errorf("close certificate rows: %w", closeErr),
      )
    }
  }()

  for rows.Next() {
    var (
      xcaID   int64
      xcaName string
      certB64 string
    )

    if err := rows.Scan(&xcaID, &xcaName, &certB64); err != nil {
      return collectionResult{}, fmt.Errorf("scan XCA certificate row: %w", err)
    }

    certificate, err := decodeCertificate(certB64)
    if err != nil {
      result.decodeErrors++
      continue
    }

    result.certificates = append(result.certificates, certificateData{
      xcaID:      xcaID,
      xcaName:    xcaName,
      commonName: certificate.Subject.CommonName,
      dn:         certificate.Subject.String(),
      notAfter:   certificate.NotAfter.UTC().Unix(),
    })
  }

  if err := rows.Err(); err != nil {
    return collectionResult{}, fmt.Errorf("read XCA certificate rows: %w", err)
  }

  return result, nil
}


// Close closes the connection to the database.
func (c *specificCollector) Close() error {
  if c.db == nil {
    return nil
  }
  return c.db.Close()
}


// tablePrefixPattern is a regular expression for validating a table prefix.
var tablePrefixPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)


// validateTablePrefix checks the validity of the passed table prefix.
func validateTablePrefix(prefix string) error {
  if prefix == "" || tablePrefixPattern.MatchString(prefix) {
    return nil
  }

  return fmt.Errorf(
    "database.table-prefix %q must be empty or start with an ASCII letter or underscore and contain only ASCII letters, digits and underscores",
    prefix,
  )
}


// driverAndConnectionString identifies the database connection driver
// and extracts the actual connection string.
func driverAndConnectionString(connectionString string) (driverName, nativeConnectionString string, err error) {
  scheme, rest, found := strings.Cut(connectionString, "://")
  if !found || scheme == "" {
    return "", "", fmt.Errorf("database.connection-string must start with postgresql://, mysql:// or sqlite://")
  }

  switch strings.ToLower(scheme) {
    case "postgresql", "postgres":
      if rest == "" {
        return "", "", fmt.Errorf("empty PostgreSQL connection string")
      }
      // pgx accepts the PostgreSQL URI itself, so keep it byte-for-byte intact.
      return "pgx", connectionString, nil

    case "mysql":
      if rest == "" {
        return "", "", fmt.Errorf("empty MySQL connection string")
      }
      return "mysql", rest, nil

    case "sqlite":
      if rest == "" {
        return "", "", fmt.Errorf("empty SQLite connection string")
      }
      return "sqlite", rest, nil

    default:
      return "", "", fmt.Errorf("unsupported database connection-string scheme %q", scheme)
  }
}
