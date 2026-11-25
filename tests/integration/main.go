//go:build integration

package main

import (
  "context"
  "crypto/ecdsa"
  "crypto/elliptic"
  "crypto/rand"
  "crypto/x509"
  "crypto/x509/pkix"
  "database/sql"
  "encoding/base64"
  "errors"
  "flag"
  "fmt"
  "io"
  "math"
  "math/big"
  "net/http"
  "net/url"
  "os"
  "strconv"
  "strings"
  "time"

  mysqlDriver "github.com/go-sql-driver/mysql"
  _ "github.com/jackc/pgx/v5/stdlib"
  _ "modernc.org/sqlite"
)

const (
  itemID        = 4242
  deletedItemID = 4243

  itemName    = "xca-exporter-ci-item"
  deletedName = "xca-exporter-ci-deleted-item"
  commonName  = "xca-exporter-ci.example"
)

type certificateSample struct {
  labels map[string]string
  value  float64
}

type sqlStatement struct {
  name string
  sql  string
}

func main() {
  connectionString := flag.String(
    "database.connection-string",
    os.Getenv("XCA_EXPORTER_DATABASE_CONNECTION_STRING"),
    "Read-only database connection string; same as the exporter",
  )

  adminConnectionString := flag.String(
    "database.admin-connection-string",
    os.Getenv("XCA_EXPORTER_TEST_ADMIN_DATABASE_CONNECTION_STRING"),
    "Administrative database connection string used to prepare the fixture",
  )

  tablePrefix := flag.String(
    "database.table-prefix",
    envOrDefault("XCA_EXPORTER_DATABASE_TABLE_PREFIX", "xca_"),
    "Prefix used for XCA database tables",
  )

  metricsURL := flag.String(
    "test.metrics-url",
    os.Getenv("XCA_EXPORTER_TEST_METRICS_URL"),
    "URL of the exporter metrics endpoint",
  )

  flag.Parse()

  if err := validateTablePrefix(*tablePrefix); err != nil {
    fatalf("%v", err)
  }

  driver, exporterDSN, err := driverAndConnectionString(
    *connectionString,
  )
  if err != nil {
    fatalf("%v", err)
  }

  adminDSN := exporterDSN

  if strings.TrimSpace(*connectionString) == "" {
    fatalf(
      "database.connection-string is required",
    )
  }

  if driver == "sqlite" {
    if *adminConnectionString != "" {
      fatalf(
        "database.admin-connection-string must not be specified for SQLite",
      )
    }
  } else {
    if strings.TrimSpace(*adminConnectionString) == "" {
      fatalf(
        "database.admin-connection-string is required for PostgreSQL and MySQL",
      )
    }

    adminDriver, nativeDSN, err := driverAndConnectionString(
      *adminConnectionString,
    )
    if err != nil {
      fatalf("invalid administrative connection string: %v", err)
    }

    if adminDriver != driver {
      fatalf(
        "database drivers do not match: exporter uses %s, admin uses %s",
        driver,
        adminDriver,
      )
    }

    adminDSN = nativeDSN

    if err := validateDistinctDatabaseUsers(
      driver,
      adminDSN,
      exporterDSN,
    ); err != nil {
      fatalf("%v", err)
    }
  }

  if *metricsURL == "" {
    switch driver {
    case "sqlite":
      *metricsURL = "http://127.0.0.1:9793/metrics"
    default:
      *metricsURL = "http://exporter:9793/metrics"
    }
  }

  ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
  defer cancel()

  if err := run(
    ctx,
    driver,
    adminDSN,
    exporterDSN,
    *tablePrefix,
    *metricsURL,
  ); err != nil {
    fatalf("%v", err)
  }

  fmt.Println("Integration test passed")
}

func fatalf(format string, args ...any) {
  fmt.Fprintf(os.Stderr, format+"\n", args...)
  os.Exit(1)
}

func envOrDefault(name, fallback string) string {
  if value := os.Getenv(name); value != "" {
    return value
  }
  return fallback
}

// Keep this parsing contract in sync with exporter.go.
func driverAndConnectionString(
  connectionString string,
) (driverName, nativeConnectionString string, err error) {
  scheme, rest, found := strings.Cut(connectionString, "://")
  if !found || scheme == "" {
    return "", "", errors.New(
      "database.connection-string must start with postgresql://, mysql:// or sqlite://",
    )
  }

  switch strings.ToLower(scheme) {
  case "postgresql", "postgres":
    if rest == "" {
      return "", "", errors.New("empty PostgreSQL connection string")
    }
    return "pgx", connectionString, nil

  case "mysql":
    if rest == "" {
      return "", "", errors.New("empty MySQL connection string")
    }
    return "mysql", rest, nil

  case "sqlite":
    if rest == "" {
      return "", "", errors.New("empty SQLite connection string")
    }
    return "sqlite", rest, nil

  default:
    return "", "", fmt.Errorf(
      "unsupported database connection-string scheme %q",
      scheme,
    )
  }
}

// Keep this validation contract in sync with exporter.go.
func validateTablePrefix(prefix string) error {
  if prefix == "" {
    return nil
  }

  if prefix[0] != '_' &&
    (prefix[0] < 'A' || prefix[0] > 'Z') &&
    (prefix[0] < 'a' || prefix[0] > 'z') {
    return errors.New(
      "database.table-prefix must be empty or start with an ASCII letter or underscore",
    )
  }

  for _, r := range prefix[1:] {
    if r != '_' &&
      (r < 'A' || r > 'Z') &&
      (r < 'a' || r > 'z') &&
      (r < '0' || r > '9') {
      return fmt.Errorf(
        "database.table-prefix contains unsupported character %q; use only ASCII letters, digits and underscores",
        r,
      )
    }
  }

  return nil
}

func connectionCredentials(
  driver string,
  dsn string,
) (username, password string, err error) {
  switch driver {
  case "pgx":
    parsed, err := url.Parse(dsn)
    if err != nil {
      return "", "", fmt.Errorf("parse PostgreSQL DSN: %w", err)
    }

    if parsed.User == nil {
      return "", "", errors.New("PostgreSQL DSN has no username")
    }

    parsedPassword, ok := parsed.User.Password()
    if !ok {
      return "", "", errors.New("PostgreSQL DSN has no password")
    }

    username = parsed.User.Username()
    password = parsedPassword

  case "mysql":
    config, err := mysqlDriver.ParseDSN(dsn)
    if err != nil {
      return "", "", fmt.Errorf("parse MySQL DSN: %w", err)
    }

    username = config.User
    password = config.Passwd

  default:
    return "", "", fmt.Errorf(
      "database credentials are not supported for driver %q",
      driver,
    )
  }

  if username == "" {
    return "", "", errors.New("database username is empty")
  }
  if password == "" {
    return "", "", errors.New("database password is empty")
  }

  return username, password, nil
}

func validateDistinctDatabaseUsers(
  driver string,
  adminDSN string,
  exporterDSN string,
) error {
  adminUsername, _, err := connectionCredentials(driver, adminDSN)
  if err != nil {
    return fmt.Errorf("administrative database credentials: %w", err)
  }

  exporterUsername, _, err := connectionCredentials(driver, exporterDSN)
  if err != nil {
    return fmt.Errorf("exporter database credentials: %w", err)
  }

  same := adminUsername == exporterUsername

  if driver == "mysql" {
    same = strings.EqualFold(adminUsername, exporterUsername)
  }

  if same {
    return errors.New(
      "administrative and read-only database usernames must be different",
    )
  }

  return nil
}

func run(
  ctx context.Context,
  driver string,
  adminDSN string,
  exporterDSN string,
  tablePrefix string,
  metricsURL string,
) (retErr error) {
  adminDB, err := sql.Open(driver, adminDSN)
  if err != nil {
    return fmt.Errorf("open administrative database connection: %w", err)
  }
  defer func() {
    if closeErr := adminDB.Close(); closeErr != nil {
      retErr = errors.Join(
        retErr,
        fmt.Errorf("close admin database: %w", closeErr),
      )
    }
  }()

  adminDB.SetMaxOpenConns(1)
  adminDB.SetMaxIdleConns(1)

  if err := waitDatabase(ctx, adminDB); err != nil {
    return fmt.Errorf("administrative database connection: %w", err)
  }

  certDER, cert, err := generateCertificate()
  if err != nil {
    return fmt.Errorf("generate certificate: %w", err)
  }

  if err := createFixture(
    ctx,
    adminDB,
    driver,
    tablePrefix,
    certDER,
    cert,
  ); err != nil {
    return fmt.Errorf("create database fixture: %w", err)
  }

  if driver != "sqlite" {
    if err := createReadOnlyAccount(
      ctx,
      adminDB,
      driver,
      tablePrefix,
      exporterDSN,
    ); err != nil {
      return fmt.Errorf("create read-only database account: %w", err)
    }
  }

  lines, err := fetchMetrics(ctx, metricsURL)
  if err != nil {
    return err
  }

  if err := checkScalarMetric(lines, "xca_exporter_collect_error", 0); err != nil {
    return err
  }

  samples, err := parseCertificateSamples(lines)
  if err != nil {
    return err
  }

  // One active and one deleted certificate were inserted.
  // exporter.go filters rows using WHERE i.del = 0.
  if len(samples) != 1 {
    return fmt.Errorf(
      "expected exactly one xca_certificate_not_after sample, got %d",
      len(samples),
    )
  }

  actual := samples[0]
  expectedLabels := map[string]string{
    "xca_id":   strconv.Itoa(itemID),
    "xca_name": itemName,
    "c_name":   cert.Subject.CommonName,
    "d_name":   cert.Subject.String(),
  }

  if !equalLabels(actual.labels, expectedLabels) {
    return fmt.Errorf(
      "certificate labels mismatch:\nexpected: %#v\nactual:   %#v",
      expectedLabels,
      actual.labels,
    )
  }

  expectedExpiry := cert.NotAfter.UTC().Unix()

  if math.IsNaN(actual.value) ||
    math.IsInf(actual.value, 0) ||
    math.Trunc(actual.value) != actual.value {
    return fmt.Errorf(
      "certificate expiry is not an integer Unix timestamp: %v",
      actual.value,
    )
  }

  if actual.value != float64(expectedExpiry) {
    return fmt.Errorf(
      "certificate expiry mismatch: expected %d, got %.0f",
      expectedExpiry,
      actual.value,
    )
  }

  fmt.Printf("Database driver: %s\n", driver)
  fmt.Printf("Certificate expiry: %d\n", expectedExpiry)
  fmt.Printf("Certificate labels: %#v\n", actual.labels)
  fmt.Println("Deleted certificate correctly excluded")

  return nil
}

func waitDatabase(ctx context.Context, db *sql.DB) error {
  ticker := time.NewTicker(500 * time.Millisecond)
  defer ticker.Stop()

  deadline := time.NewTimer(30 * time.Second)
  defer deadline.Stop()

  var lastErr error

  for {
    pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
    err := db.PingContext(pingCtx)
    cancel()

    if err == nil {
      return nil
    }
    lastErr = err

    select {
    case <-ctx.Done():
      return fmt.Errorf(
        "database connection interrupted: %w; last error: %w",
        ctx.Err(),
        lastErr,
      )
    case <-deadline.C:
      return fmt.Errorf("database did not become ready: %w", lastErr)
    case <-ticker.C:
    }
  }
}

func generateCertificate() ([]byte, *x509.Certificate, error) {
  key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
  if err != nil {
    return nil, nil, err
  }

  now := time.Now().UTC().Truncate(time.Second)

  template := &x509.Certificate{
    SerialNumber: big.NewInt(424242),
    Subject: pkix.Name{
      CommonName: commonName,
    },
    NotBefore:             now.Add(-time.Minute),
    NotAfter:              now.Add(90 * 24 * time.Hour),
    KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
    BasicConstraintsValid: true,
    IsCA:                  true,
  }

  der, err := x509.CreateCertificate(
    rand.Reader,
    template,
    template,
    &key.PublicKey,
    key,
  )
  if err != nil {
    return nil, nil, err
  }

  cert, err := x509.ParseCertificate(der)
  if err != nil {
    return nil, nil, err
  }

  return der, cert, nil
}

func quoteIdentifier(driver, identifier string) string {
  if driver == "mysql" {
    return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
  }

  return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func placeholders(driver string, count int) string {
  values := make([]string, count)

  for i := range values {
    if driver == "pgx" {
      values[i] = "$" + strconv.Itoa(i+1)
    } else {
      values[i] = "?"
    }
  }

  return strings.Join(values, ", ")
}

func quotedColumns(driver string, names ...string) string {
  columns := make([]string, len(names))

  for i, name := range names {
    columns[i] = quoteIdentifier(driver, name)
  }

  return strings.Join(columns, ", ")
}

func createFixture(
  ctx context.Context,
  db *sql.DB,
  driver string,
  prefix string,
  certDER []byte,
  cert *x509.Certificate,
) error {
  q := func(name string) string {
    return quoteIdentifier(driver, name)
  }

  itemsTable := q(prefix + "items")
  certsTable := q(prefix + "certs")

  itemsDDL := fmt.Sprintf(`
CREATE TABLE %s (
    %s INTEGER NOT NULL,
    %s VARCHAR(128),
    %s INTEGER,
    %s INTEGER,
    %s CHAR(15),
    %s VARCHAR(2048),
    %s INTEGER DEFAULT 0 NOT NULL,
    %s SMALLINT DEFAULT 0 NOT NULL
)`,
    itemsTable,
    q("id"), q("name"), q("type"), q("source"),
    q("date"), q("comment"), q("stamp"), q("del"),
  )

  certsDDL := fmt.Sprintf(`
CREATE TABLE %s (
    %s INTEGER,
    %s INTEGER,
    %s INTEGER,
    %s VARCHAR(64),
    %s INTEGER,
    %s INTEGER,
    %s VARCHAR(8000)
)`,
    certsTable,
    q("item"), q("hash"), q("iss_hash"), q("serial"),
    q("issuer"), q("ca"), q("cert"),
  )

  for _, statement := range []string{itemsDDL, certsDDL} {
    if _, err := db.ExecContext(ctx, statement); err != nil {
      return err
    }
  }

  itemColumns := quotedColumns(
    driver,
    "id", "name", "type", "source",
    "date", "comment", "stamp", "del",
  )

  itemSQL := fmt.Sprintf(  // #nosec G201 -- SQL identifiers are quoted and values use parameterized placeholders
    "INSERT INTO %s (%s) VALUES (%s)",
    itemsTable,
    itemColumns,
    placeholders(driver, 8),
  )

  // XCA date format: YYYYMMDDHHMMSSZ (15 characters).
  itemDate := time.Now().UTC().Format("20060102150405Z")

  insertItem := func(id int, name string, deleted int) error {
    _, err := db.ExecContext(
      ctx,
      itemSQL,
      id,
      name,
      3, // type
      2, // source
      itemDate,
      "GitHub Actions integration test",
      0, // stamp
      deleted,
    )

    return err
  }

  if err := insertItem(itemID, itemName, 0); err != nil {
    return err
  }

  if err := insertItem(deletedItemID, deletedName, 1); err != nil {
    return err
  }

  certSQL := fmt.Sprintf(  // #nosec G201 -- SQL identifiers are quoted and values use parameterized placeholders
    `INSERT INTO %s (
        %s
    ) VALUES (%s)`,
    certsTable,
    quotedColumns(
      driver,
      "item", "hash", "iss_hash", "serial", "issuer", "ca", "cert",
    ),
    placeholders(driver, 7),
  )

  certBase64 := base64.StdEncoding.EncodeToString(certDER)
  serial := cert.SerialNumber.Text(16)

  insertCertificate := func(id int) error {
    _, err := db.ExecContext(
      ctx,
      certSQL,
      id,
      1, // hash
      1, // iss_hash
      serial,
      id, // issuer
      1,  // ca
      certBase64,
    )

    return err
  }

  if err := insertCertificate(itemID); err != nil {
    return err
  }

  if err := insertCertificate(deletedItemID); err != nil {
    return err
  }

  return nil
}

func postgresStringLiteral(value string) (string, error) {
  if strings.IndexByte(value, 0) >= 0 {
    return "", errors.New("PostgreSQL string contains NUL")
  }

  value = strings.ReplaceAll(value, `\`, `\\`)
  value = strings.ReplaceAll(value, "'", "''")

  return "E'" + value + "'", nil
}

func mysqlStringLiteral(
  value string,
  noBackslashEscapes bool,
) (string, error) {
  if strings.IndexByte(value, 0) >= 0 {
    return "", errors.New("MySQL string contains NUL")
  }

  if !noBackslashEscapes {
    value = strings.ReplaceAll(value, `\`, `\\`)
  }

  value = strings.ReplaceAll(value, "'", "''")

  return "'" + value + "'", nil
}

func executeSQLStatements(
  ctx context.Context,
  db *sql.DB,
  statements []sqlStatement,
) error {
  for _, statement := range statements {
    if _, err := db.ExecContext(ctx, statement.sql); err != nil {
      // Do not include SQL text here: CREATE USER / CREATE ROLE
      // statements contain the plaintext password.
      return fmt.Errorf("%s: %w", statement.name, err)
    }
  }

  return nil
}

func createReadOnlyAccount(
  ctx context.Context,
  db *sql.DB,
  driver string,
  prefix string,
  exporterDSN string,
) error {
  username, password, err := connectionCredentials(driver, exporterDSN)
  if err != nil {
    return err
  }

  itemsName := prefix + "items"
  certsName := prefix + "certs"

  switch driver {
  case "pgx":
    var databaseName string
    var schemaName sql.NullString

    err := db.QueryRowContext(
      ctx,
      "SELECT current_database(), current_schema()",
    ).Scan(&databaseName, &schemaName)
    if err != nil {
      return fmt.Errorf("get PostgreSQL database and schema: %w", err)
    }

    if !schemaName.Valid || schemaName.String == "" {
      return errors.New("PostgreSQL current schema is empty")
    }

    passwordLiteral, err := postgresStringLiteral(password)
    if err != nil {
      return err
    }

    schemaLiteral, err := postgresStringLiteral(schemaName.String)
    if err != nil {
      return err
    }

    role := quoteIdentifier(driver, username)
    database := quoteIdentifier(driver, databaseName)
    schema := quoteIdentifier(driver, schemaName.String)

    statements := []sqlStatement{
      {
        name: "create PostgreSQL read-only role",
        sql: fmt.Sprintf(
          `CREATE ROLE %s WITH
            LOGIN
            NOSUPERUSER
            NOCREATEDB
            NOCREATEROLE
            NOREPLICATION
            NOBYPASSRLS
            NOINHERIT
            PASSWORD %s`,
          role,
          passwordLiteral,
        ),
      },
      {
        name: "grant PostgreSQL database connection",
        sql: fmt.Sprintf(
          `GRANT CONNECT ON DATABASE %s TO %s`,
          database,
          role,
        ),
      },
      {
        name: "grant PostgreSQL schema usage",
        sql: fmt.Sprintf(
          `GRANT USAGE ON SCHEMA %s TO %s`,
          schema,
          role,
        ),
      },
      {
        name: "grant PostgreSQL table read access",
        sql: fmt.Sprintf(
          `GRANT SELECT ON TABLE %s.%s, %s.%s TO %s`,
          schema,
          quoteIdentifier(driver, itemsName),
          schema,
          quoteIdentifier(driver, certsName),
          role,
        ),
      },
      {
        name: "set PostgreSQL read-only role search path",
        sql: fmt.Sprintf(
          `ALTER ROLE %s IN DATABASE %s SET search_path TO %s`,
          role,
          database,
          schemaLiteral,
        ),
      },
    }

    return executeSQLStatements(ctx, db, statements)

  case "mysql":
    var databaseName sql.NullString
    var sqlMode sql.NullString

    err := db.QueryRowContext(
      ctx,
      "SELECT DATABASE(), @@SESSION.sql_mode",
    ).Scan(&databaseName, &sqlMode)
    if err != nil {
      return fmt.Errorf("get MySQL database and SQL mode: %w", err)
    }

    if !databaseName.Valid || databaseName.String == "" {
      return errors.New("MySQL connection has no selected database")
    }

    noBackslashEscapes := false

    if sqlMode.Valid {
      for _, mode := range strings.Split(sqlMode.String, ",") {
        if strings.EqualFold(
          strings.TrimSpace(mode),
          "NO_BACKSLASH_ESCAPES",
        ) {
          noBackslashEscapes = true
          break
        }
      }
    }

    userLiteral, err := mysqlStringLiteral(
      username,
      noBackslashEscapes,
    )
    if err != nil {
      return err
    }

    passwordLiteral, err := mysqlStringLiteral(
      password,
      noBackslashEscapes,
    )
    if err != nil {
      return err
    }

    account := userLiteral + "@'%'"
    database := quoteIdentifier(driver, databaseName.String)

    statements := []sqlStatement{
      {
        name: "create MySQL read-only user",
        sql: fmt.Sprintf(
          `CREATE USER %s IDENTIFIED BY %s`,
          account,
          passwordLiteral,
        ),
      },
      {
        name: "grant MySQL read access to items table",
        sql: fmt.Sprintf(
          `GRANT SELECT ON %s.%s TO %s`,
          database,
          quoteIdentifier(driver, itemsName),
          account,
        ),
      },
      {
        name: "grant MySQL read access to certs table",
        sql: fmt.Sprintf(
          `GRANT SELECT ON %s.%s TO %s`,
          database,
          quoteIdentifier(driver, certsName),
          account,
        ),
      },
    }

    return executeSQLStatements(ctx, db, statements)

  default:
    return fmt.Errorf("unsupported database driver %q", driver)
  }
}


func fetchMetrics(ctx context.Context, url string) ([]string, error) {
  waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
  defer cancel()

  client := &http.Client{Timeout: 3 * time.Second}
  ticker := time.NewTicker(500 * time.Millisecond)
  defer ticker.Stop()

  var lastErr error

  for {
    req, err := http.NewRequestWithContext(   // #nosec G704 -- URL is supplied by CI configuration
      waitCtx,
      http.MethodGet,
      url,
      nil,
    )
    if err != nil {
      return nil, err
    }

    resp, err := client.Do(req)   // #nosec G704 -- URL is supplied by CI configuration
    if err != nil {
      lastErr = err
    } else {
      body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
      closeErr := resp.Body.Close()

      if responseErr := errors.Join(readErr, closeErr); responseErr != nil {
        lastErr = fmt.Errorf("read/close metrics response: %w", responseErr)
      } else if resp.StatusCode == http.StatusOK {
        return strings.Split(string(body), "\n"), nil
      } else {
        lastErr = fmt.Errorf(
          "metrics endpoint returned HTTP %d: %s",
          resp.StatusCode,
          strings.TrimSpace(string(body)),
        )
      }
    }

    select {
    case <-waitCtx.Done():
      return nil, fmt.Errorf(
        "metrics endpoint did not become ready: %w; last error: %w",
        waitCtx.Err(),
        lastErr,
      )
    case <-ticker.C:
    }
  }
}

func checkScalarMetric(
  lines []string,
  name string,
  expected float64,
) error {
  var (
    found bool
    value float64
  )

  for _, line := range lines {
    line = strings.TrimSpace(line)

    if line == "" || strings.HasPrefix(line, "#") {
      continue
    }

    fields := strings.Fields(line)
    if len(fields) < 2 || fields[0] != name {
      continue
    }

    if found {
      return fmt.Errorf("duplicate scalar metric %q", name)
    }

    parsed, err := strconv.ParseFloat(fields[1], 64)
    if err != nil {
      return fmt.Errorf("parse metric %q: %w", name, err)
    }

    found = true
    value = parsed
  }

  if !found {
    return fmt.Errorf("metric %q not found", name)
  }

  if value != expected {
    return fmt.Errorf(
      "metric %s: expected %v, got %v",
      name,
      expected,
      value,
    )
  }

  return nil
}

func parseCertificateSamples(
  lines []string,
) ([]certificateSample, error) {
  const prefix = "xca_certificate_not_after{"

  var samples []certificateSample

  for _, line := range lines {
    line = strings.TrimSpace(line)

    if !strings.HasPrefix(line, prefix) {
      continue
    }

    rest := line[len(prefix):]
    end := closingBrace(rest)

    if end < 0 {
      return nil, fmt.Errorf(
        "malformed certificate metric: %s",
        line,
      )
    }

    labels, err := parseLabels(rest[:end])
    if err != nil {
      return nil, fmt.Errorf(
        "parse certificate metric labels: %w",
        err,
      )
    }

    fields := strings.Fields(rest[end+1:])
    if len(fields) == 0 {
      return nil, fmt.Errorf(
        "certificate metric has no value: %s",
        line,
      )
    }

    value, err := strconv.ParseFloat(fields[0], 64)
    if err != nil {
      return nil, fmt.Errorf(
        "parse certificate metric value: %w",
        err,
      )
    }

    samples = append(samples, certificateSample{
      labels: labels,
      value:  value,
    })
  }

  return samples, nil
}

func closingBrace(s string) int {
  inQuote := false
  escaped := false

  for i := 0; i < len(s); i++ {
    ch := s[i]

    if inQuote {
      if escaped {
        escaped = false
        continue
      }

      switch ch {
      case '\\':
        escaped = true
      case '"':
        inQuote = false
      }

      continue
    }

    switch ch {
      case '"': {
        inQuote = true
      }
      case '}': {
        return i
      }
    }
  }

  return -1
}

func parseLabels(raw string) (map[string]string, error) {
  labels := make(map[string]string)

  for i := 0; i < len(raw); {
    for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
      i++
    }

    if i == len(raw) {
      break
    }

    keyStart := i

    for i < len(raw) && raw[i] != '=' {
      if raw[i] == ',' {
        return nil, fmt.Errorf(
          "missing '=' near %q",
          raw[keyStart:i],
        )
      }
      i++
    }

    if i == len(raw) {
      return nil, fmt.Errorf(
        "missing '=' near %q",
        raw[keyStart:],
      )
    }

    key := strings.TrimSpace(raw[keyStart:i])
    if key == "" {
      return nil, errors.New("empty label name")
    }

    i++ // '='

    for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
      i++
    }

    if i == len(raw) || raw[i] != '"' {
      return nil, fmt.Errorf(
        "label %q has no quoted value",
        key,
      )
    }

    valueStart := i
    i++ // opening quote

    escaped := false
    closed := false

    valueLoop:
    for i < len(raw) {
      ch := raw[i]

      if escaped {
        escaped = false
        i++
        continue
      }

      switch ch {
        case '\\': {
          escaped = true
          i++
          continue
        }

        case '"': {
          i++
          closed = true
          break valueLoop
        }
      }

      i++
    }

    if !closed {
      return nil, fmt.Errorf(
        "unterminated value for label %q",
        key,
      )
    }

    value, err := strconv.Unquote(raw[valueStart:i])
    if err != nil {
      return nil, fmt.Errorf(
        "decode label %q: %w",
        key,
        err,
      )
    }

    if _, exists := labels[key]; exists {
      return nil, fmt.Errorf("duplicate label %q", key)
    }

    labels[key] = value

    for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
      i++
    }

    if i < len(raw) {
      if raw[i] != ',' {
        return nil, fmt.Errorf(
          "expected comma after label %q",
          key,
        )
      }

      i++

      if i == len(raw) {
        return nil, errors.New("trailing comma in labels")
      }
    }
  }

  return labels, nil
}

func equalLabels(
  actual map[string]string,
  expected map[string]string,
) bool {
  if len(actual) != len(expected) {
    return false
  }

  for key, value := range expected {
    if actual[key] != value {
      return false
    }
  }

  return true
}
