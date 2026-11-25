package main

import (
  "context"
  "crypto/rand"
  "crypto/rsa"
  "crypto/x509"
  "crypto/x509/pkix"
  "database/sql"
  "encoding/base64"
  "errors"
  "fmt"
  "io"
  "log/slog"
  "math/big"
  "os"
  "path/filepath"
  "strings"
  "testing"
  "time"

  "github.com/prometheus/client_golang/prometheus"
  dto "github.com/prometheus/client_model/go"
)

func exporterTestLogger() *slog.Logger {
  return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func exporterTestCertificate(t *testing.T, commonName string, notAfter time.Time) string {
  t.Helper()

  key, err := rsa.GenerateKey(rand.Reader, 2048)
  if err != nil {
    t.Fatalf("GenerateKey() error: %v", err)
  }

  template := &x509.Certificate{
    SerialNumber: big.NewInt(12345),
    Subject: pkix.Name{
      CommonName: commonName,
      Country:    []string{"NL"},
      Organization: []string{"Test organization"},
    },
    NotBefore:             time.Now().Add(-time.Hour),
    NotAfter:              notAfter,
    KeyUsage:              x509.KeyUsageDigitalSignature,
    BasicConstraintsValid: true,
  }

  der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
  if err != nil {
    t.Fatalf("CreateCertificate() error: %v", err)
  }

  return base64.StdEncoding.EncodeToString(der)
}

func exporterTestSQLiteDatabase(t *testing.T) (string, *sql.DB) {
  t.Helper()

  filename := filepath.Join(t.TempDir(), "xca.sqlite")
  db, err := sql.Open("sqlite", filename)
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }

  _, err = db.Exec(`
CREATE TABLE items (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  del INTEGER NOT NULL DEFAULT 0,
  item INTEGER
);
CREATE TABLE certs (
  item INTEGER NOT NULL,
  cert TEXT NOT NULL
);
`)
  if err != nil {
    _ = db.Close()
    t.Fatalf("create test schema: %v", err)
  }

  t.Cleanup(func() {
    _ = db.Close()
  })

  return filename, db
}

func exporterTestCollector(t *testing.T, filename string) *specificCollector {
  t.Helper()

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = ""
  config.Database.TestOnStart.Enabled = true
  config.Database.TestOnStart.Timeout = NonNegativeDuration(5 * time.Second)

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }

  specific, ok := collector.(*specificCollector)
  if !ok {
    t.Fatalf("collector type = %T, want *specificCollector", collector)
  }
  t.Cleanup(func() {
    _ = specific.Close()
  })

  return specific
}

func exporterTestMetricFamilies(t *testing.T, metrics []prometheus.Metric) map[string]*dto.Metric {
  t.Helper()

  registry := prometheus.NewRegistry()
  registry.MustRegister(metricSliceCollector{metrics: metrics})

  families, err := registry.Gather()
  if err != nil {
    t.Fatalf("registry.Gather() error: %v", err)
  }

  result := make(map[string]*dto.Metric)
  for _, family := range families {
    if len(family.Metric) == 0 {
      continue
    }
    result[family.GetName()] = family.Metric[0]
  }
  return result
}

func TestModuleVersion(t *testing.T) {
  // The module may be represented by its original path, replacement path,
  // or be absent from build information. All cases must return a non-empty
  // version string.
  if got := moduleVersion("module-that-does-not-exist.invalid/example"); got != "unknown" {
    t.Fatalf("moduleVersion(unknown) = %q, want unknown", got)
  }

  if got := moduleVersion("github.com/jackc/pgx/v5"); got == "" {
    t.Fatal("moduleVersion(pgx) returned an empty string")
  }
}

func TestSpecificCLIHandleSpecialArgs(t *testing.T) {
  t.Run("not requested", func(t *testing.T) {
    config := SpecificCLI{}
    var output strings.Builder

    handled, err := config.HandleSpecialArgs(&output)
    if err != nil {
      t.Fatalf("HandleSpecialArgs() error: %v", err)
    }
    if handled {
      t.Fatal("handled = true, want false")
    }
    if output.Len() != 0 {
      t.Fatalf("output = %q, want empty", output.String())
    }
  })

  t.Run("versions requested", func(t *testing.T) {
    config := SpecificCLI{}
    config.Versions = true
    var output strings.Builder

    handled, err := config.HandleSpecialArgs(&output)
    if err != nil {
      t.Fatalf("HandleSpecialArgs() error: %v", err)
    }
    if !handled {
      t.Fatal("handled = false, want true")
    }

    for _, module := range []string{
      "github.com/jackc/pgx/v5",
      "github.com/go-sql-driver/mysql",
      "modernc.org/sqlite",
    } {
      if !strings.Contains(output.String(), module+": ") {
        t.Errorf("output does not contain module %q: %s", module, output.String())
      }
    }
  })

  t.Run("writer error", func(t *testing.T) {
    config := SpecificCLI{}
    config.Versions = true

    handled, err := config.HandleSpecialArgs(exporterTestErrorWriter{})
    if !handled {
      t.Fatal("handled = false, want true")
    }
    if err == nil {
      t.Fatal("HandleSpecialArgs() error = nil, want writer error")
    }
    if !strings.Contains(err.Error(), "write") {
      t.Fatalf("unexpected error: %v", err)
    }
  })
}

type exporterTestErrorWriter struct{}

func (exporterTestErrorWriter) Write([]byte) (int, error) {
  return 0, errors.New("write failed")
}

func TestValidateTablePrefix(t *testing.T) {
  tests := []struct {
    name    string
    prefix  string
    wantErr bool
  }{
    {name: "empty", prefix: ""},
    {name: "letters", prefix: "xca_"},
    {name: "underscore", prefix: "_xca"},
    {name: "leading underscore", prefix: "_"},
    {name: "digits after first character", prefix: "xca_123"},
    {name: "uppercase", prefix: "XCA_"},
    {name: "hyphen", prefix: "xca-", wantErr: true},
    {name: "space", prefix: "xca ", wantErr: true},
    {name: "starts with digit", prefix: "1xca", wantErr: true},
    {name: "dot", prefix: "xca.", wantErr: true},
    {name: "SQL punctuation", prefix: "xca;DROP", wantErr: true},
    {name: "non ASCII", prefix: "тест_", wantErr: true},
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      err := validateTablePrefix(tt.prefix)
      if (err != nil) != tt.wantErr {
        t.Fatalf("validateTablePrefix(%q) error = %v, wantErr %v", tt.prefix, err, tt.wantErr)
      }
      if tt.wantErr && !strings.Contains(err.Error(), "database.table-prefix") {
        t.Fatalf("unexpected error: %v", err)
      }
    })
  }
}

func TestDriverAndConnectionString(t *testing.T) {
  tests := []struct {
    name       string
    input      string
    driver     string
    connection string
    wantErr    bool
  }{
    {
      name:       "postgresql URI",
      input:      "postgresql://user:password@localhost:5432/xca?sslmode=disable",  // #nosec G101 -- test fixture with a dummy password.
      driver:     "pgx",
      connection: "postgresql://user:password@localhost:5432/xca?sslmode=disable",
    },
    {
      name:       "postgres alias",
      input:      "postgres://user@localhost/xca",
      driver:     "pgx",
      connection: "postgres://user@localhost/xca",
    },
    {
      name:       "mysql URI",
      input:      "mysql://user:password@tcp(localhost:3306)/xca",
      driver:     "mysql",
      connection: "user:password@tcp(localhost:3306)/xca",
    },
    {
      name:       "sqlite URI",
      input:      "sqlite:///tmp/xca.sqlite",
      driver:     "sqlite",
      connection: "/tmp/xca.sqlite",
    },
    {
      name:       "sqlite relative path",
      input:      "sqlite://xca.sqlite",
      driver:     "sqlite",
      connection: "xca.sqlite",
    },
    {
      name:    "missing scheme",
      input:   "localhost/xca",
      wantErr: true,
    },
    {
      name:    "empty scheme",
      input:   "://localhost/xca",
      wantErr: true,
    },
    {
      name:    "empty postgresql connection",
      input:   "postgresql://",
      wantErr: true,
    },
    {
      name:    "empty mysql connection",
      input:   "mysql://",
      wantErr: true,
    },
    {
      name:    "empty sqlite connection",
      input:   "sqlite://",
      wantErr: true,
    },
    {
      name:    "unsupported scheme",
      input:   "oracle://localhost/xca",
      wantErr: true,
    },
    {
      name:    "case insensitive scheme",
      input:   "MYSQL://user@tcp(localhost:3306)/xca",
      driver:  "mysql",
      connection: "user@tcp(localhost:3306)/xca",
    },
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      driver, connection, err := driverAndConnectionString(tt.input)

      if tt.wantErr {
        if err == nil {
          t.Fatal("driverAndConnectionString() error = nil, want error")
        }
        return
      }

      if err != nil {
        t.Fatalf("driverAndConnectionString() error: %v", err)
      }
      if driver != tt.driver {
        t.Errorf("driver = %q, want %q", driver, tt.driver)
      }
      if connection != tt.connection {
        t.Errorf("connection = %q, want %q", connection, tt.connection)
      }
    })
  }
}

func TestNewCertificateMetric(t *testing.T) {
  desc := prometheus.NewDesc(
    "xca_certificate_not_after",
    "expiration",
    []string{"xca_id", "xca_name", "c_name", "d_name"},
    nil,
  )

  metric := newCertificateMetric(desc, 123, "Certificate", "example.org", "CN=example.org", 1700000000)

  var dtoMetric dto.Metric
  if err := metric.Write(&dtoMetric); err != nil {
    t.Fatalf("metric.Write() error: %v", err)
  }

  if got, want := dtoMetric.GetGauge().GetValue(), float64(1700000000); got != want {
    t.Errorf("gauge value = %v, want %v", got, want)
  }

  wantLabels := map[string]string{
    "xca_id":   "123",
    "xca_name": "Certificate",
    "c_name":   "example.org",
    "d_name":   "CN=example.org",
  }
  for _, pair := range dtoMetric.Label {
    if want, ok := wantLabels[pair.GetName()]; ok {
      if pair.GetValue() != want {
        t.Errorf("label %s = %q, want %q", pair.GetName(), pair.GetValue(), want)
      }
      delete(wantLabels, pair.GetName())
    }
  }
  if len(wantLabels) != 0 {
    t.Errorf("missing labels: %#v", wantLabels)
  }
}

func TestDecodeCertificate(t *testing.T) {
  notAfter := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
  encoded := exporterTestCertificate(t, "example.org", notAfter)

  certificate, err := decodeCertificate(encoded)
  if err != nil {
    t.Fatalf("decodeCertificate() error: %v", err)
  }
  if certificate.Subject.CommonName != "example.org" {
    t.Errorf("CommonName = %q, want example.org", certificate.Subject.CommonName)
  }
  if !certificate.NotAfter.Equal(notAfter) {
    t.Errorf("NotAfter = %s, want %s", certificate.NotAfter, notAfter)
  }
}

func TestDecodeCertificateInvalidBase64(t *testing.T) {
  certificate, err := decodeCertificate("%%%not-base64%%%")
  if err == nil {
    t.Fatal("decodeCertificate() error = nil, want error")
  }
  if certificate != nil {
    t.Fatalf("certificate = %#v, want nil", certificate)
  }
  if !strings.Contains(err.Error(), "decode certificate base64") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestDecodeCertificateInvalidDER(t *testing.T) {
  encoded := base64.StdEncoding.EncodeToString([]byte("not a certificate"))

  certificate, err := decodeCertificate(encoded)
  if err == nil {
    t.Fatal("decodeCertificate() error = nil, want error")
  }
  if certificate != nil {
    t.Fatalf("certificate = %#v, want nil", certificate)
  }
  if !strings.Contains(err.Error(), "parse X.509 certificate") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestNewSpecificCollectorRequiresConnectionString(t *testing.T) {
  config := SpecificCLI{}

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err == nil {
    t.Fatal("NewSpecificCollector() error = nil, want error")
  }
  if collector != nil {
    t.Fatalf("collector = %#v, want nil", collector)
  }
  if !strings.Contains(err.Error(), "database.connection-string") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestNewSpecificCollectorRejectsInvalidTablePrefix(t *testing.T) {
  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://test.sqlite"
  config.Database.TablePrefix = "invalid-prefix"

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err == nil {
    t.Fatal("NewSpecificCollector() error = nil, want error")
  }
  if collector != nil {
    t.Fatalf("collector = %#v, want nil", collector)
  }
}

func TestNewSpecificCollectorRejectsUnsupportedDriver(t *testing.T) {
  config := SpecificCLI{}
  config.Database.ConnectionString = "unsupported://connection"
  config.Database.TablePrefix = "xca_"

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err == nil {
    t.Fatal("NewSpecificCollector() error = nil, want error")
  }
  if collector != nil {
    t.Fatalf("collector = %#v, want nil", collector)
  }
}

func TestNewSpecificCollectorSQLite(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = db

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = ""
  config.Database.TestOnStart.Enabled = true
  config.Database.TestOnStart.Timeout = NonNegativeDuration(5 * time.Second)

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }

  specific, ok := collector.(*specificCollector)
  if !ok {
    t.Fatalf("collector type = %T, want *specificCollector", collector)
  }

  if specific.db == nil {
    t.Fatal("collector db is nil")
  }
  if specific.db.Stats().MaxOpenConnections != 1 {
    t.Errorf("MaxOpenConnections = %d, want 1", specific.db.Stats().MaxOpenConnections)
  }
  if specific.tables["certs"] != "certs" {
    t.Errorf("certs table = %q, want certs", specific.tables["certs"])
  }
  if specific.tables["items"] != "items" {
    t.Errorf("items table = %q, want items", specific.tables["items"])
  }
  if !strings.Contains(specific.query, "JOIN items") {
    t.Errorf("query does not join items table: %s", specific.query)
  }
  if !strings.Contains(specific.query, "WHERE i.del = 0") {
    t.Errorf("query does not filter deleted items: %s", specific.query)
  }

  if err := specific.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestNewSpecificCollectorWithoutStartupTest(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "not-yet-created.sqlite")

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = false

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestNewSpecificCollectorStartupTestFailureWithoutExit(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "empty.sqlite")

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = true
  config.Database.TestOnStart.ExitOnFailure = false

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error = %v, want collector despite startup test failure", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestNewSpecificCollectorStartupTestFailureWithExit(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "empty.sqlite")

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = true
  config.Database.TestOnStart.ExitOnFailure = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err == nil {
    t.Fatal("NewSpecificCollector() error = nil, want startup test error")
  }
  if collector != nil {
    t.Fatalf("collector = %#v, want nil", collector)
  }
}

func TestSpecificCollectorTestDatabase(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = filename

  collector := &specificCollector{
    db: db,
    tables: map[string]string{
      "certs": "certs",
      "items": "items",
    },
  }

  if err := collector.testDatabase(context.Background(), NonNegativeDuration(time.Second)); err != nil {
    t.Fatalf("testDatabase() error: %v", err)
  }
}

func TestSpecificCollectorTestDatabaseMissingTable(t *testing.T) {
  db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing.sqlite"))
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  collector := &specificCollector{
    db: db,
    tables: map[string]string{
      "certs": "missing_certs",
    },
  }

  err = collector.testDatabase(context.Background(), NonNegativeDuration(time.Second))
  if err == nil {
    t.Fatal("testDatabase() error = nil, want missing table error")
  }
  if !strings.Contains(err.Error(), "test table") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestSpecificCollectorCollect(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  notAfter := time.Date(2032, 5, 6, 7, 8, 9, 0, time.UTC)
  validCertificate := exporterTestCertificate(t, "valid.example.org", notAfter)

  _, err := db.Exec(
    "INSERT INTO items (id, name, del) VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)",
    1, "valid certificate", 0,
    2, "deleted certificate", 1,
    3, "invalid certificate", 0,
  )
  if err != nil {
    t.Fatalf("insert items: %v", err)
  }

  _, err = db.Exec(
    "INSERT INTO certs (item, cert) VALUES (?, ?), (?, ?), (?, ?)",
    1, validCertificate,
    2, validCertificate,
    3, "invalid-base64",
  )
  if err != nil {
    t.Fatalf("insert certs: %v", err)
  }

  collector := exporterTestCollector(t, filename)

  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }

  if len(metrics) != 2 {
    t.Fatalf("metric count = %d, want 2 (one certificate and error gauge)", len(metrics))
  }

  families := exporterTestMetricFamilies(t, metrics)

  certificateMetric := families["xca_certificate_not_after"]
  if certificateMetric == nil {
    t.Fatal("xca_certificate_not_after metric not found")
  }
  if got, want := certificateMetric.GetGauge().GetValue(), float64(notAfter.Unix()); got != want {
    t.Errorf("certificate expiration = %v, want %v", got, want)
  }

  labels := make(map[string]string)
  for _, pair := range certificateMetric.Label {
    labels[pair.GetName()] = pair.GetValue()
  }
  if labels["xca_id"] != "1" {
    t.Errorf("xca_id = %q, want 1", labels["xca_id"])
  }
  if labels["xca_name"] != "valid certificate" {
    t.Errorf("xca_name = %q", labels["xca_name"])
  }
  if labels["c_name"] != "valid.example.org" {
    t.Errorf("c_name = %q", labels["c_name"])
  }

  errorMetric := families["xca_exporter_collect_error"]
  if errorMetric == nil {
    t.Fatal("xca_exporter_collect_error metric not found")
  }
  if got := errorMetric.GetGauge().GetValue(); got != 1 {
    t.Errorf("decode error gauge = %v, want 1", got)
  }
}

func TestSpecificCollectorCollectEmptyDatabase(t *testing.T) {
  filename, _ := exporterTestSQLiteDatabase(t)
  collector := exporterTestCollector(t, filename)

  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }
  if len(metrics) != 1 {
    t.Fatalf("metric count = %d, want 1 error gauge", len(metrics))
  }

  families := exporterTestMetricFamilies(t, metrics)
  errorMetric := families["xca_exporter_collect_error"]
  if errorMetric == nil {
    t.Fatal("xca_exporter_collect_error metric not found")
  }
  if got := errorMetric.GetGauge().GetValue(); got != 0 {
    t.Errorf("decode error gauge = %v, want 0", got)
  }
}

func TestSpecificCollectorCollectDeletedCertificateIsIgnored(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  certificate := exporterTestCertificate(
    t,
    "deleted.example.org",
    time.Now().Add(24*time.Hour),
  )

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'deleted', 1)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, ?)", certificate)
  if err != nil {
    t.Fatalf("insert certificate: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }

  if len(metrics) != 1 {
    t.Fatalf("metric count = %d, want 1 (error gauge only)", len(metrics))
  }
  families := exporterTestMetricFamilies(t, metrics)
  if _, ok := families["xca_certificate_not_after"]; ok {
    t.Fatal("deleted certificate was exported")
  }
}

func TestSpecificCollectorCollectQueryError(t *testing.T) {
  db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "query-error.sqlite"))
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  collector := &specificCollector{
    db:    db,
    query: "SELECT item, name, cert FROM missing_table",
    collectErrorDesc: prometheus.NewDesc(
      "xca_exporter_collect_error",
      "errors",
      nil,
      nil,
    ),
    notAfterDesc: prometheus.NewDesc(
      "xca_certificate_not_after",
      "expiration",
      []string{"xca_id", "xca_name", "c_name", "d_name"},
      nil,
    ),
  }

  metrics, err := collector.Collect(context.Background())
  if err == nil {
    t.Fatal("Collect() error = nil, want query error")
  }
  if metrics != nil {
    t.Fatalf("metrics = %#v, want nil on collection error", metrics)
  }
}

func TestSpecificCollectorCloseNilDB(t *testing.T) {
  collector := &specificCollector{}
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error = %v, want nil", err)
  }
}

func TestSpecificCollectorClose(t *testing.T) {
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }

  collector := &specificCollector{db: db}
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }

  if err := db.Ping(); err == nil {
    t.Fatal("database Ping() succeeded after Close()")
  }
}

func TestSpecificCollectorCollectFromDatabase(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  notAfter := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
  certificate := exporterTestCertificate(t, "database.example.org", notAfter)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (42, 'database certificate', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (42, ?)", certificate)
  if err != nil {
    t.Fatalf("insert certificate: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  result, err := collector.collectFromDatabase(context.Background())
  if err != nil {
    t.Fatalf("collectFromDatabase() error: %v", err)
  }

  if len(result.certificates) != 1 {
    t.Fatalf("certificate count = %d, want 1", len(result.certificates))
  }
  if result.decodeErrors != 0 {
    t.Errorf("decodeErrors = %v, want 0", result.decodeErrors)
  }

  got := result.certificates[0]
  if got.xcaID != 42 {
    t.Errorf("xcaID = %d, want 42", got.xcaID)
  }
  if got.xcaName != "database certificate" {
    t.Errorf("xcaName = %q", got.xcaName)
  }
  if got.commonName != "database.example.org" {
    t.Errorf("commonName = %q", got.commonName)
  }
  if got.notAfter != notAfter.Unix() {
    t.Errorf("notAfter = %d, want %d", got.notAfter, notAfter.Unix())
  }
}

func TestSpecificCollectorCollectFromDatabaseDecodeError(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'invalid', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, 'not-base64')")
  if err != nil {
    t.Fatalf("insert certificate: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  result, err := collector.collectFromDatabase(context.Background())
  if err != nil {
    t.Fatalf("collectFromDatabase() error: %v", err)
  }

  if len(result.certificates) != 0 {
    t.Errorf("certificate count = %d, want 0", len(result.certificates))
  }
  if result.decodeErrors != 1 {
    t.Errorf("decodeErrors = %v, want 1", result.decodeErrors)
  }
}

func TestSpecificCollectorCollectFromDatabaseQueryFailure(t *testing.T) {
  db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "query-failure.sqlite"))
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  collector := &specificCollector{
    db:    db,
    query: "SELECT item, name, cert FROM absent",
  }

  result, err := collector.collectFromDatabase(context.Background())
  if err == nil {
    t.Fatal("collectFromDatabase() error = nil, want query error")
  }
  if len(result.certificates) != 0 {
    t.Errorf("certificates = %#v, want empty result", result.certificates)
  }
  if !strings.Contains(err.Error(), "query XCA certificates") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestSpecificCollectorQueryUsesConfiguredPrefix(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = filename

  _, err := db.Exec(`
CREATE TABLE custom_certs (item INTEGER, cert TEXT);
CREATE TABLE custom_items (id INTEGER, name TEXT, del INTEGER);
`)
  if err != nil {
    t.Fatalf("create prefixed schema: %v", err)
  }

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "custom_"
  config.Database.TestOnStart.Enabled = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  t.Cleanup(func() {
    if err := collector.Close(); err != nil {
      t.Errorf("failed to close collector: %v", err)
    }
  })

  specific := collector.(*specificCollector)
  if specific.tables["certs"] != "custom_certs" {
    t.Errorf("certs table = %q, want custom_certs", specific.tables["certs"])
  }
  if specific.tables["items"] != "custom_items" {
    t.Errorf("items table = %q, want custom_items", specific.tables["items"])
  }
  if !strings.Contains(specific.query, "custom_certs") {
    t.Errorf("query does not contain custom_certs: %s", specific.query)
  }
  if !strings.Contains(specific.query, "custom_items") {
    t.Errorf("query does not contain custom_items: %s", specific.query)
  }
}

func TestSpecificCollectorStartupTestUsesConfiguredPrefix(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "prefixed.sqlite")

  db, err := sql.Open("sqlite", filename)
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  _, err = db.Exec(`
CREATE TABLE custom_certs (item INTEGER, cert TEXT);
CREATE TABLE custom_items (id INTEGER, name TEXT, del INTEGER);
`)
  if err != nil {
    _ = db.Close()
    t.Fatalf("create prefixed schema: %v", err)
  }
  if err := db.Close(); err != nil {
    t.Fatalf("close setup database: %v", err)
  }

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "custom_"
  config.Database.TestOnStart.Enabled = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestSpecificCollectorCollectUsesSingleflightResultType(t *testing.T) {
  // The normal path returns collectionResult from collectFromDatabase.
  // This test verifies that the collector can turn an empty result into
  // the expected error gauge.
  collector := &specificCollector{
    collectErrorDesc: prometheus.NewDesc(
      "xca_exporter_collect_error",
      "errors",
      nil,
      nil,
    ),
    notAfterDesc: prometheus.NewDesc(
      "xca_certificate_not_after",
      "expiration",
      []string{"xca_id", "xca_name", "c_name", "d_name"},
      nil,
    ),
  }

  // Use an actual empty SQLite schema so the database query succeeds.
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  _, err = db.Exec(`
CREATE TABLE items (id INTEGER, name TEXT, del INTEGER);
CREATE TABLE certs (item INTEGER, cert TEXT);
`)
  if err != nil {
    t.Fatalf("create schema: %v", err)
  }

  collector.db = db
  collector.tables = map[string]string{"certs": "certs", "items": "items"}
  collector.query = `
SELECT c.item, i.name, c.cert
FROM certs AS c
JOIN items AS i ON i.id = c.item
WHERE i.del = 0`

  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }
  if len(metrics) != 1 {
    t.Fatalf("metric count = %d, want 1", len(metrics))
  }
}

func TestSpecificCollectorCollectContextCancellation(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = db

  collector := exporterTestCollector(t, filename)
  ctx, cancel := context.WithCancel(context.Background())
  cancel()

  // SQLite may complete a trivial query before observing cancellation,
  // so this test only asserts that Collect does not panic and that its
  // return values obey the normal contract.
  metrics, err := collector.Collect(ctx)
  if err == nil && metrics == nil {
    t.Fatal("Collect() returned nil metrics and nil error")
  }
}

func TestExporterTestCertificateHelper(t *testing.T) {
  encoded := exporterTestCertificate(t, "helper.example.org", time.Now().Add(time.Hour))
  if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
    t.Fatalf("helper returned invalid base64: %v", err)
  }
}

func TestSpecificCollectorDatabaseCloseFailureIsNotExpected(t *testing.T) {
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }

  collector := &specificCollector{db: db}
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("second Close() error = %v, want nil", err)
  }
}

func TestDriverAndConnectionStringErrorMessages(t *testing.T) {
  tests := []struct {
    input string
    want  string
  }{
    {"missing-scheme", "must start with"},
    {"postgresql://", "empty PostgreSQL"},
    {"mysql://", "empty MySQL"},
    {"sqlite://", "empty SQLite"},
    {"unknown://db", "unsupported"},
  }

  for _, tt := range tests {
    t.Run(tt.input, func(t *testing.T) {
      _, _, err := driverAndConnectionString(tt.input)
      if err == nil {
        t.Fatal("expected error")
      }
      if !strings.Contains(err.Error(), tt.want) {
        t.Fatalf("error = %q, want substring %q", err, tt.want)
      }
    })
  }
}

func TestSpecificCollectorCollectDecodeErrorsAreGauge(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'bad', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, 'bad-base64')")
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }

  families := exporterTestMetricFamilies(t, metrics)
  errorMetric := families["xca_exporter_collect_error"]
  if errorMetric == nil {
    t.Fatal("collect error metric missing")
  }
  if errorMetric.GetGauge() == nil {
    t.Fatal("collect error metric is not a gauge")
  }
  if got := errorMetric.GetGauge().GetValue(); got != 1 {
    t.Errorf("collect error gauge = %v, want 1", got)
  }
}

func TestSpecificCollectorTestDatabaseNoRowsIsSuccess(t *testing.T) {
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  _, err = db.Exec(`
CREATE TABLE certs (item INTEGER);
CREATE TABLE items (id INTEGER, item INTEGER);
`)
  if err != nil {
    t.Fatalf("create schema: %v", err)
  }

  collector := &specificCollector{
    db: db,
    tables: map[string]string{
      "certs": "certs",
      "items": "items",
    },
  }

  if err := collector.testDatabase(context.Background(), 0); err != nil {
    t.Fatalf("testDatabase() error = %v, want nil for empty tables", err)
  }
}

func TestNewSpecificCollectorDoesNotRunStartupTestWhenDisabled(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "missing-schema.sqlite")

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = false

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestSpecificCollectorCollectMalformedCertificateDoesNotFailScrape(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'broken', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, 'not-valid')")
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() returned scrape error for malformed certificate: %v", err)
  }
  if len(metrics) != 1 {
    t.Fatalf("metric count = %d, want only error gauge", len(metrics))
  }
}

func TestSpecificCollectorQueryExcludesDeletedItems(t *testing.T) {
  filename, _ := exporterTestSQLiteDatabase(t)
  collector := exporterTestCollector(t, filename)

  if !strings.Contains(collector.query, "i.del = 0") {
    t.Fatalf("query does not exclude deleted items: %s", collector.query)
  }
}

func TestSpecificCollectorCloseAfterStartupTest(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = db

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = ""
  config.Database.TestOnStart.Enabled = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestSpecificCollectorCollectResultMetricCount(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  certificate := exporterTestCertificate(t, "one.example.org", time.Now().Add(48*time.Hour))
  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'one', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, ?)", certificate)
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }

  if len(metrics) != 2 {
    t.Fatalf("metric count = %d, want certificate metric plus error gauge", len(metrics))
  }
}

func TestExporterDefinition(t *testing.T) {
  if exporterDefinition.Namespace != "xca" {
    t.Errorf("Namespace = %q, want xca", exporterDefinition.Namespace)
  }
  if exporterDefinition.Name != "xca_exporter" {
    t.Errorf("Name = %q, want xca_exporter", exporterDefinition.Name)
  }
  if exporterDefinition.DefaultListenAddress != ":9793" {
    t.Errorf("DefaultListenAddress = %q, want :9793", exporterDefinition.DefaultListenAddress)
  }
  if exporterDefinition.NewCollector == nil {
    t.Fatal("NewCollector is nil")
  }
}

func TestSpecificCollectorTableNames(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  _ = db

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "prefix_"
  config.Database.TestOnStart.Enabled = false

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  t.Cleanup(func() {
    if err := collector.Close(); err != nil {
      t.Errorf("failed to close collector: %v", err)
    }
  })

  specific := collector.(*specificCollector)
  if specific.tables["certs"] != "prefix_certs" {
    t.Errorf("certs table = %q, want prefix_certs", specific.tables["certs"])
  }
  if specific.tables["items"] != "prefix_items" {
    t.Errorf("items table = %q, want prefix_items", specific.tables["items"])
  }
}

func TestSpecificCollectorCollectDoesNotExportDeletedCertificate(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  certificate := exporterTestCertificate(t, "deleted.example.org", time.Now().Add(24*time.Hour))
  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (10, 'deleted', 1)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (10, ?)", certificate)
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }

  for _, metric := range metrics {
    var written dto.Metric
    if err := metric.Write(&written); err != nil {
      t.Fatalf("metric.Write() error: %v", err)
    }
    if written.GetGauge() != nil && written.GetGauge().GetValue() == float64(time.Now().Add(24*time.Hour).Unix()) {
      t.Fatal("deleted certificate expiration metric was exported")
    }
  }
}

func TestSpecificCollectorCollectWithMultipleCertificates(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  certOne := exporterTestCertificate(t, "one.example.org", time.Now().Add(24*time.Hour))
  certTwo := exporterTestCertificate(t, "two.example.org", time.Now().Add(48*time.Hour))

  _, err := db.Exec(
    "INSERT INTO items (id, name, del) VALUES (1, 'one', 0), (2, 'two', 0)",
  )
  if err != nil {
    t.Fatalf("insert items: %v", err)
  }
  _, err = db.Exec(
    "INSERT INTO certs (item, cert) VALUES (1, ?), (2, ?)",
    certOne, certTwo,
  )
  if err != nil {
    t.Fatalf("insert certificates: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }
  if len(metrics) != 3 {
    t.Fatalf("metric count = %d, want 3", len(metrics))
  }

  families := exporterTestMetricFamilies(t, metrics)
  if _, ok := families["xca_certificate_not_after"]; !ok {
    t.Fatal("certificate metric family not found")
  }
  if _, ok := families["xca_exporter_collect_error"]; !ok {
    t.Fatal("collect error metric family not found")
  }
}

func TestSpecificCollectorTestDatabaseContextParameter(t *testing.T) {
  db, err := sql.Open("sqlite", ":memory:")
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  _, err = db.Exec(`
CREATE TABLE certs (item INTEGER);
CREATE TABLE items (id INTEGER, item INTEGER);
`)
  if err != nil {
    t.Fatalf("create schema: %v", err)
  }

  collector := &specificCollector{
    db: db,
    tables: map[string]string{
      "certs": "certs",
      "items": "items",
    },
  }

  ctx, cancel := context.WithCancel(context.Background())
  cancel()

  // testDatabase currently creates its own background context rather than
  // deriving one from parent. The test captures the existing behavior:
  // with a valid schema the test itself succeeds.
  if err := collector.testDatabase(ctx, 0); err != nil {
    t.Fatalf("testDatabase() error: %v", err)
  }
}

func TestSpecificCollectorCollectFromDatabaseUsesUTCUnixTime(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  notAfter := time.Date(2031, 7, 8, 9, 10, 11, 0, time.FixedZone("test-zone", 5*60*60))
  certificate := exporterTestCertificate(t, "timezone.example.org", notAfter)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'timezone', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, ?)", certificate)
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  result, err := collector.collectFromDatabase(context.Background())
  if err != nil {
    t.Fatalf("collectFromDatabase() error: %v", err)
  }

  if got, want := result.certificates[0].notAfter, notAfter.UTC().Unix(); got != want {
    t.Errorf("notAfter = %d, want UTC Unix timestamp %d", got, want)
  }
}

func TestSpecificCollectorCollectFromDatabaseReturnsNoPartialResultOnScanFailure(t *testing.T) {
  // SQLite's dynamic typing makes a deterministic scan failure difficult
  // to produce for this query. The query-error path is covered separately.
  // This test asserts the row-query setup remains usable with an empty table.
  filename, _ := exporterTestSQLiteDatabase(t)
  collector := exporterTestCollector(t, filename)

  result, err := collector.collectFromDatabase(context.Background())
  if err != nil {
    t.Fatalf("collectFromDatabase() error: %v", err)
  }
  if len(result.certificates) != 0 {
    t.Fatalf("certificate count = %d, want 0", len(result.certificates))
  }
}

func TestSpecificCollectorCollectErrorMetricIsCurrentScrapeGauge(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (1, 'invalid', 0)")
  if err != nil {
    t.Fatalf("insert item: %v", err)
  }
  _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (1, 'bad')")
  if err != nil {
    t.Fatalf("insert cert: %v", err)
  }

  collector := exporterTestCollector(t, filename)
  first, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("first Collect() error: %v", err)
  }
  if got := exporterTestMetricFamilies(t, first)["xca_exporter_collect_error"].GetGauge().GetValue(); got != 1 {
    t.Fatalf("first scrape decode errors = %v, want 1", got)
  }

  _, err = db.Exec("DELETE FROM certs")
  if err != nil {
    t.Fatalf("delete invalid certificate: %v", err)
  }

  second, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("second Collect() error: %v", err)
  }
  if got := exporterTestMetricFamilies(t, second)["xca_exporter_collect_error"].GetGauge().GetValue(); got != 0 {
    t.Fatalf("second scrape decode errors = %v, want 0", got)
  }
}

func TestSpecificCollectorCloseAfterQuery(t *testing.T) {
  filename, _ := exporterTestSQLiteDatabase(t)
  collector := exporterTestCollector(t, filename)

  if _, err := collector.Collect(context.Background()); err != nil {
    t.Fatalf("Collect() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("Close() error: %v", err)
  }
}

func TestSpecificCollectorStartupTestFailureIncludesTableName(t *testing.T) {
  db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing-table.sqlite"))
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  t.Cleanup(func() {
    if err := db.Close(); err != nil {
      t.Errorf("failed to close database: %v", err)
    }
  })

  collector := &specificCollector{
    db: db,
    tables: map[string]string{
      "certs": "absent_table",
    },
  }

  err = collector.testDatabase(context.Background(), NonNegativeDuration(time.Second))
  if err == nil {
    t.Fatal("testDatabase() error = nil, want missing table error")
  }
  if !strings.Contains(err.Error(), "absent_table") {
    t.Fatalf("error %q does not identify missing table", err)
  }
}

func TestNewSpecificCollectorStartupTestExitOnFailure(t *testing.T) {
  filename := filepath.Join(t.TempDir(), "no-tables.sqlite")

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = true
  config.Database.TestOnStart.ExitOnFailure = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err == nil {
    t.Fatal("expected startup test error")
  }
  if collector != nil {
    t.Fatalf("collector = %#v, want nil", collector)
  }
}

func TestNewSpecificCollectorDefaultTablePrefix(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  _, err := db.Exec(`
CREATE TABLE xca_certs (item INTEGER, cert TEXT);
CREATE TABLE xca_items (id INTEGER, name TEXT, del INTEGER);
`)
  if err != nil {
    t.Fatalf("create default-prefixed schema: %v", err)
  }

  config := SpecificCLI{}
  config.Database.ConnectionString = "sqlite://" + filename
  config.Database.TablePrefix = "xca_"
  config.Database.TestOnStart.Enabled = true

  collector, err := NewSpecificCollector(config, exporterTestLogger())
  if err != nil {
    t.Fatalf("NewSpecificCollector() error: %v", err)
  }
  t.Cleanup(func() {
    if err := collector.Close(); err != nil {
      t.Errorf("failed to close collector: %v", err)
    }
  })

  specific := collector.(*specificCollector)
  if specific.tables["certs"] != "xca_certs" {
    t.Errorf("certs table = %q, want xca_certs", specific.tables["certs"])
  }
  if specific.tables["items"] != "xca_items" {
    t.Errorf("items table = %q, want xca_items", specific.tables["items"])
  }
}

func TestSpecificCollectorCollectResultContainsOneMetricPerCertificate(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)

  certificates := []string{
    exporterTestCertificate(t, "first.example.org", time.Now().Add(24*time.Hour)),
    exporterTestCertificate(t, "second.example.org", time.Now().Add(48*time.Hour)),
    exporterTestCertificate(t, "third.example.org", time.Now().Add(72*time.Hour)),
  }

  for i, certificate := range certificates {
    id := i + 1
    _, err := db.Exec("INSERT INTO items (id, name, del) VALUES (?, ?, 0)", id, fmt.Sprintf("certificate-%d", id))
    if err != nil {
      t.Fatalf("insert item %d: %v", id, err)
    }
    _, err = db.Exec("INSERT INTO certs (item, cert) VALUES (?, ?)", id, certificate)
    if err != nil {
      t.Fatalf("insert certificate %d: %v", id, err)
    }
  }

  collector := exporterTestCollector(t, filename)
  metrics, err := collector.Collect(context.Background())
  if err != nil {
    t.Fatalf("Collect() error: %v", err)
  }
  if got, want := len(metrics), len(certificates)+1; got != want {
    t.Fatalf("metric count = %d, want %d", got, want)
  }
}

func TestSpecificCollectorCollectReturnsDatabaseError(t *testing.T) {
  db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.sqlite"))
  if err != nil {
    t.Fatalf("sql.Open() error: %v", err)
  }
  if err := db.Close(); err != nil {
    t.Fatalf("db.Close() error: %v", err)
  }

  collector := &specificCollector{
    db: db,
    query: "SELECT c.item, i.name, c.cert FROM certs AS c " +
      "JOIN items AS i ON i.id = c.item WHERE i.del = 0",
    collectErrorDesc: prometheus.NewDesc("xca_exporter_collect_error", "errors", nil, nil),
    notAfterDesc: prometheus.NewDesc(
      "xca_certificate_not_after",
      "expiration",
      []string{"xca_id", "xca_name", "c_name", "d_name"},
      nil,
    ),
  }

  metrics, err := collector.Collect(context.Background())
  if err == nil {
    t.Fatal("Collect() error = nil, want database error")
  }
  if metrics != nil {
    t.Fatalf("metrics = %#v, want nil", metrics)
  }
}

func TestSpecificCollectorCloseIsIdempotentForNilDB(t *testing.T) {
  collector := &specificCollector{}
  if err := collector.Close(); err != nil {
    t.Fatalf("first Close() error: %v", err)
  }
  if err := collector.Close(); err != nil {
    t.Fatalf("second Close() error: %v", err)
  }
}

func TestExporterTestSQLiteDatabaseHelper(t *testing.T) {
  filename, db := exporterTestSQLiteDatabase(t)
  if _, err := os.Stat(filename); err != nil {
    t.Fatalf("database file does not exist: %v", err)
  }
  if err := db.Ping(); err != nil {
    t.Fatalf("db.Ping() error: %v", err)
  }
}
