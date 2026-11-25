package main

import (
  "context"
  "fmt"
  "io"
  "log/slog"
  "net/http"
  "os"
  "runtime"
  "time"

  "github.com/prometheus/client_golang/prometheus"
  "github.com/prometheus/client_golang/prometheus/collectors"
  "github.com/prometheus/client_golang/prometheus/promhttp"
  dto "github.com/prometheus/client_model/go"
  "github.com/prometheus/exporter-toolkit/web"
)


// Build information is injected by the linker, for example:
//
//  -X main.version=1.2.3
//  -X main.revision=$(git rev-parse HEAD)
//  -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)
var (
  version   = "0.0.0-dev"
  revision  = "unknown"
  buildDate = "unknown"
)


// ExporterDefinition is the contract between main.go and exporter.go.
type ExporterDefinition struct {
  Namespace            string
  Name                 string
  Description          string
  DefaultListenAddress string
  NewCollector         func(config SpecificCLI, logger *slog.Logger) (SpecificCollector, error)
}


// SpecificCollector is the exporter-specific interface.
//
// Collect is called for every Prometheus scrape. It returns fully constructed
// Prometheus metrics on success. A non-nil error means the scrape is invalid
// as a whole and the common HTTP layer returns HTTP 500.
type SpecificCollector interface {
  Collect(ctx context.Context) ([]prometheus.Metric, error)
  Close() error
}


// specificArgsHandler is an optional extension point. main.go does not
// know concrete exporter-specific commands; exporter.go can implement this
// interface on SpecificCLI when needed (currently used for --versions).
type specificArgsHandler interface {
  HandleSpecialArgs(w io.Writer) (handled bool, err error)
}


// A functions stored in a variables, rather than a standard declaration,
// to allow for substitution during testing.
var (
  runApplication = func(args []string, stdout, stderr io.Writer) error {
    return run(args, stdout, stderr, defaultMainDependencies())
  }

  exitProcess = os.Exit
)


// mainDependencies is a set of dependencies required by the core logic.
type mainDependencies struct {
  definition ExporterDefinition

  handleSpecialArgs  func(any, io.Writer) (bool, error)
  newLandingPage     func(web.LandingConfig) (http.Handler, error)
  listenAndServe     func(*http.Server, *web.FlagConfig, *slog.Logger) error
}


// dispatchSpecialArgs handles special options that do not require
// the initialization and startup of the collector and web server.
// Returns two values:
//   - handled: indicates whether a special option was recognized and processed.
//   - err:     contains an error if a problem occurred during processing.
func dispatchSpecialArgs(
  specific any,
  w io.Writer,
) (
  bool,
  error,
) {
  handler, ok := specific.(specificArgsHandler)
  if !ok {
    return false, nil
  }

  return handler.HandleSpecialArgs(w)
}


// defaultMainDependencies returns a mainDependencies structure populated with
// actual implementations.
func defaultMainDependencies() mainDependencies {
  return mainDependencies{
    definition: exporterDefinition,

    handleSpecialArgs: dispatchSpecialArgs,

    newLandingPage: func(config web.LandingConfig) (http.Handler, error) {
      return web.NewLandingPage(config)
    },

    listenAndServe: web.ListenAndServe,
  }
}


// newScrapeGatherer creates an instance of scrapeGatherer and populates it with
// the necessary dependencies.
func newScrapeGatherer(
  common prometheus.Gatherer,
  def ExporterDefinition,
  specific SpecificCollector,
  timeout time.Duration,
  logger *slog.Logger,
) *scrapeGatherer {
  return &scrapeGatherer{
    common:    common,
    specific:  specific,
    timeout:   timeout,
    namespace: def.Namespace,
    logger:    logger,
    scrapeDuration: prometheus.NewDesc(
      def.Namespace+"_scrape_duration_seconds",
      "Duration of the exporter-specific scrape in seconds.",
      nil,
      nil,
    ),
  }
}


// metricSliceCollector exposes an already collected metric snapshot. It is
// intentionally an unchecked collector: descriptors are supplied by the
// metrics themselves and are validated by Registry.Gather().
type metricSliceCollector struct {
  metrics []prometheus.Metric
}


// Describe intentionally does nothing, as metricSliceCollector receives
// metrics that are already prepared.
func (c metricSliceCollector) Describe(chan<- *prometheus.Desc) {}


// Collect sends the received metrics to a Prometheus channel.
func (c metricSliceCollector) Collect(ch chan<- prometheus.Metric) {
  for _, metric := range c.metrics {
    ch <- metric
  }
}


// scrapeGatherer adapts the exporter-specific Collect() contract to the
// prometheus.Gatherer interface used by promhttp.
type scrapeGatherer struct {
  common    prometheus.Gatherer
  specific  SpecificCollector
  timeout   time.Duration
  namespace string
  logger    *slog.Logger

  scrapeDuration *prometheus.Desc
}


// Gather is triggered each time metrics are requested,
// in order to collect them.
func (g *scrapeGatherer) Gather() (
  []*dto.MetricFamily,
  error,
) {
  started := time.Now()

  ctx := context.Background()

  if g.timeout > 0 {
    var cancel context.CancelFunc
    ctx, cancel = context.WithTimeout(
      ctx,
      g.timeout,
    )
    defer cancel()
  }

  // Call actual exporter metrics collector.
  metrics, err := g.specific.Collect(ctx)
  duration := time.Since(started).Seconds()

  // If a serious error occurs during collection that results in no series
  // being collected, then escalate this to an HTTP error.
  if err != nil {
    g.logger.Error("exporter scrape failed", "error", err)
    return nil, err
  }

  // Add a collection duration metric.
  metrics = append(metrics,
    prometheus.MustNewConstMetric(
      g.scrapeDuration,
      prometheus.GaugeValue,
      duration,
    ),
  )

  // The specific collector returns a snapshot of metrics for this scrape.
  // Register them in a temporary unchecked collector and merge it with the
  // long-lived common registry. No scrape data is retained after Gather().
  dynamicRegistry := prometheus.NewRegistry()
  dynamicRegistry.MustRegister(metricSliceCollector{metrics: metrics})

  return prometheus.Gatherers{
    g.common,
    dynamicRegistry,
  }.Gather()
}


// buildInfoCollector implements the standard prometheus.Collector interface.
type buildInfoCollector struct {
  desc *prometheus.Desc
}


// newBuildInfoCollector creates a collector and prepares a description
// of its metric.
func newBuildInfoCollector(namespace string) *buildInfoCollector {
  return &buildInfoCollector{
    desc: prometheus.NewDesc(
      namespace+"_build_info",
      "Build information about the exporter.",
      []string{"version", "revision", "build_date", "go_version"},
      nil,
    ),
  }
}


// Describe provides to Prometheus information about the metrics
// exposed by the collector.
func (c *buildInfoCollector) Describe(ch chan<- *prometheus.Desc) {
  ch <- c.desc
}


// buildInfoCollector creates a metric containing build information
// and sends it to a Prometheus channel.
func (c *buildInfoCollector) Collect(ch chan<- prometheus.Metric) {
  ch <- prometheus.MustNewConstMetric(
    c.desc,
    prometheus.GaugeValue,
    1,
    version,
    revision,
    buildDate,
    runtime.Version(),
  )
}


// newLogger sets the requested logging level and creates a logger with
// the requested format.
func newLogger(
  format,
  level string,
  output io.Writer,
) *slog.Logger {
  var slogLevel slog.Level

  switch level {
    case "debug":
      slogLevel = slog.LevelDebug
    case "warn":
      slogLevel = slog.LevelWarn
    case "error":
      slogLevel = slog.LevelError
    default:
      slogLevel = slog.LevelInfo
  }

  opts := &slog.HandlerOptions{Level: slogLevel}

  if format == "json" {
    return slog.New(slog.NewJSONHandler(output, opts))
  }

  return slog.New(slog.NewTextHandler(output, opts))
}


// run prepares auxiliary tools and the exporter, and launches the web server.
// Returns an error if processing fails.
func run(
  args []string,
  stdout io.Writer,
  stderr io.Writer,
  deps mainDependencies,
) error {
  var cli struct {
    Common   CommonCLI   `embed:""`
    Specific SpecificCLI `embed:""`
  }

  cli, err := parseCLI(
    args,
    stdout,
    stderr,
    deps.definition,
    version,
  )
  if err != nil {
    return err
  }

  logger := newLogger(cli.Common.Log.Format, cli.Common.Log.Level, stderr).With(
    "exporter", deps.definition.Namespace,
  )

  // Рrocess special arguments (which do not require launching the builder or
  // the web server).
  handled, err := deps.handleSpecialArgs(cli.Specific, stdout)
  if err != nil {
    errMsg := "special command failed"
    logger.Error(errMsg, "error", err)
    return fmt.Errorf("%s: %w", errMsg, err)
  }
  if handled {
    return nil
  }

  // Iitialize the exporter itself.
  specific, err := deps.definition.NewCollector(cli.Specific, logger)
  if err != nil {
    errMsg := "failed to initialize exporter"
    logger.Error(errMsg, "error", err)
    return fmt.Errorf("%s: %w", errMsg, err)
  }

  // Free up resources in any case.
  defer func() {
    if err := specific.Close(); err != nil {
      logger.Error("failed to close exporter resources", "error", err)
    }
  }()

  // Create a separate registry of metrics.
  commonRegistry := prometheus.NewRegistry()
  commonRegistry.MustRegister(
    newBuildInfoCollector(deps.definition.Namespace),
  )

  // Add Go runtime and exporter system process metrics,
  // unless they have been disabled.
  if !cli.Common.Web.DisableExporterMetrics {
    commonRegistry.MustRegister(collectors.NewGoCollector())
    commonRegistry.MustRegister(
      collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
    )
  }

  // Create an object that collects metric.
  scrapeGatherer := newScrapeGatherer(
    commonRegistry,
    deps.definition,
    specific,
    cli.Common.Scrape.Timeout.Duration(),
    logger,
  )

  // Return an HTTP error if the collector returned a collection error.
  handlerOpts := promhttp.HandlerOpts{
    ErrorHandling: promhttp.HTTPErrorOnError,
  }

  // Add metrics for internal HTTP handler errors during the collection
  // and encoding of metrics, unless they has been disabled.
  if !cli.Common.Web.DisableExporterMetrics {
    handlerOpts.Registry = commonRegistry
  }

  metricsHandler := promhttp.HandlerFor(scrapeGatherer, handlerOpts)

  // Add metrics of access to the metrics page itself,
  // unless they has been disabled.
  if !cli.Common.Web.DisableExporterMetrics {
    metricsHandler = promhttp.InstrumentMetricHandler(
      commonRegistry,
      metricsHandler,
    )
  }

  // Create a multiplexer that routes incoming HTTP requests.
  httpMux := http.NewServeMux()
  httpMux.Handle(cli.Common.Web.TelemetryPath, metricsHandler)

  // Create an exporter landing page, if the metrics page is not located at
  // the site root.
  if cli.Common.Web.TelemetryPath != "/" && cli.Common.Web.TelemetryPath != "" {
    landingPage, err := deps.newLandingPage(web.LandingConfig{
      Name:        deps.definition.Name,
      Description: deps.definition.Description,
      Version:     version,
      Links: []web.LandingLinks{
        {Address: cli.Common.Web.TelemetryPath, Text: "Metrics"},
      },
    })
    if err != nil {
      logger.Error("failed to create landing page", "error", err)
      return fmt.Errorf("failed to create landing page: %w", err)
    }

    httpMux.Handle("/", landingPage)
  }

  // Create HTTP server.
  server := &http.Server{
    Addr:              cli.Specific.Web.ListenAddress,
    Handler:           httpMux,
    ReadHeaderTimeout: cli.Common.Web.ReadHeaderTimeout.Duration(),
    ReadTimeout:       cli.Common.Web.ReadTimeout.Duration(),
    WriteTimeout:      cli.Common.Web.WriteTimeout.Duration(),
    IdleTimeout:       cli.Common.Web.IdleTimeout.Duration(),
  }

  // Prepare the HTTP server configuration.
  webConfig := &web.FlagConfig{
    WebListenAddresses: &[]string{cli.Specific.Web.ListenAddress},
    WebSystemdSocket:   &cli.Common.Web.SystemdSocket,
    WebConfigFile:      &cli.Common.Web.Config.File,
  }

  logger.Info(
    "starting exporter",
    "listen_address", cli.Specific.Web.ListenAddress,
    "telemetry_path", cli.Common.Web.TelemetryPath,
  )

  // Start the HTTP server.
  if err := deps.listenAndServe(server, webConfig, logger); err != nil {
    return fmt.Errorf("HTTP server stopped: %w", err)
  }

  return nil
}


func main() {
  if err := runApplication(os.Args[1:], os.Stdout, os.Stderr); err != nil {
    fmt.Fprintln(os.Stderr, err)
    exitProcess(1)
  }
}
