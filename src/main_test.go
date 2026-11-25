package main

import (
  "bytes"
  "context"
  "errors"
  "fmt"
  "io"
  "log/slog"
  "net/http"
  "net/http/httptest"
  "runtime"
  "strings"
  "testing"
  "time"

  "github.com/prometheus/client_golang/prometheus"
  dto "github.com/prometheus/client_model/go"
  "github.com/prometheus/exporter-toolkit/web"
)

type mainTestCollector struct {
  metrics   []prometheus.Metric
  collectErr error
  closeErr   error
  collectFn  func(context.Context) ([]prometheus.Metric, error)
  closed     bool
}

func (c *mainTestCollector) Collect(ctx context.Context) ([]prometheus.Metric, error) {
  if c.collectFn != nil {
    return c.collectFn(ctx)
  }
  return c.metrics, c.collectErr
}

func (c *mainTestCollector) Close() error {
  c.closed = true
  return c.closeErr
}

type mainTestSpecialHandler struct {
  handled bool
  err     error
  called  bool
}

func (h *mainTestSpecialHandler) HandleSpecialArgs(w io.Writer) (bool, error) {
  h.called = true
  _, _ = io.WriteString(w, "special")
  return h.handled, h.err
}

func mainTestLogger() *slog.Logger {
  return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mainTestMetric(name string, value float64) prometheus.Metric {
  desc := prometheus.NewDesc(name, "test metric", nil, nil)
  return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value)
}

func mainTestMetricFamilies(t *testing.T, families []*dto.MetricFamily) map[string]*dto.MetricFamily {
  t.Helper()

  result := make(map[string]*dto.MetricFamily, len(families))
  for _, family := range families {
    result[family.GetName()] = family
  }
  return result
}

func TestDispatchSpecialArgs(t *testing.T) {
  t.Run("no handler", func(t *testing.T) {
    handled, err := dispatchSpecialArgs(struct{}{}, io.Discard)
    if handled {
      t.Fatal("handled = true, want false")
    }
    if err != nil {
      t.Fatalf("unexpected error: %v", err)
    }
  })

  t.Run("handled", func(t *testing.T) {
    handler := &mainTestSpecialHandler{handled: true}
    var output bytes.Buffer

    handled, err := dispatchSpecialArgs(handler, &output)
    if err != nil {
      t.Fatalf("unexpected error: %v", err)
    }
    if !handled {
      t.Fatal("handled = false, want true")
    }
    if !handler.called {
      t.Fatal("handler was not called")
    }
    if got, want := output.String(), "special"; got != want {
      t.Fatalf("output = %q, want %q", got, want)
    }
  })

  t.Run("handler error", func(t *testing.T) {
    wantErr := errors.New("special failure")
    handler := &mainTestSpecialHandler{handled: true, err: wantErr}

    handled, err := dispatchSpecialArgs(handler, io.Discard)
    if !handled {
      t.Fatal("handled = false, want true")
    }
    if !errors.Is(err, wantErr) {
      t.Fatalf("error = %v, want %v", err, wantErr)
    }
  })
}

func TestDefaultMainDependencies(t *testing.T) {
  deps := defaultMainDependencies()

  if deps.definition.Namespace != exporterDefinition.Namespace {
    t.Fatalf("namespace = %q, want %q", deps.definition.Namespace, exporterDefinition.Namespace)
  }
  if deps.handleSpecialArgs == nil {
    t.Fatal("handleSpecialArgs is nil")
  }
  if deps.newLandingPage == nil {
    t.Fatal("newLandingPage is nil")
  }
  if deps.listenAndServe == nil {
    t.Fatal("listenAndServe is nil")
  }
}

func TestMetricSliceCollector(t *testing.T) {
  metrics := []prometheus.Metric{
    mainTestMetric("test_metric_one", 1),
    mainTestMetric("test_metric_two", 2),
  }
  collector := metricSliceCollector{metrics: metrics}

  // Describe intentionally emits no descriptors.
  descCh := make(chan *prometheus.Desc, 1)
  collector.Describe(descCh)
  close(descCh)

  if _, ok := <-descCh; ok {
    t.Fatal("Describe emitted a descriptor")
  }

  metricCh := make(chan prometheus.Metric, len(metrics))
  collector.Collect(metricCh)
  close(metricCh)

  if got := len(metricCh); got != len(metrics) {
    t.Fatalf("collected %d metrics, want %d", got, len(metrics))
  }
}

func TestNewScrapeGatherer(t *testing.T) {
  common := prometheus.NewRegistry()
  common.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{
    Name: "common_test_metric",
    Help: "common test metric",
  }))

  specific := &mainTestCollector{
    metrics: []prometheus.Metric{
      mainTestMetric("specific_test_metric", 42),
    },
  }

  gatherer := newScrapeGatherer(
    common,
    ExporterDefinition{Namespace: "test"},
    specific,
    time.Second,
    mainTestLogger(),
  )

  if gatherer.namespace != "test" {
    t.Fatalf("namespace = %q, want %q", gatherer.namespace, "test")
  }
  if gatherer.timeout != time.Second {
    t.Fatalf("timeout = %s, want %s", gatherer.timeout, time.Second)
  }
  if gatherer.specific != specific {
    t.Fatal("specific collector was not assigned")
  }

  families, err := gatherer.Gather()
  if err != nil {
    t.Fatalf("Gather() error: %v", err)
  }

  got := mainTestMetricFamilies(t, families)
  for _, name := range []string{
    "common_test_metric",
    "specific_test_metric",
    "test_scrape_duration_seconds",
  } {
    if _, ok := got[name]; !ok {
      t.Errorf("metric family %q not found", name)
    }
  }

  if value := got["specific_test_metric"].Metric[0].GetGauge().GetValue(); value != 42 {
    t.Fatalf("specific metric value = %v, want 42", value)
  }
}

func TestScrapeGathererCollectError(t *testing.T) {
  wantErr := errors.New("database unavailable")
  gatherer := newScrapeGatherer(
    prometheus.NewRegistry(),
    ExporterDefinition{Namespace: "test"},
    &mainTestCollector{collectErr: wantErr},
    time.Second,
    mainTestLogger(),
  )

  families, err := gatherer.Gather()
  if !errors.Is(err, wantErr) {
    t.Fatalf("Gather() error = %v, want %v", err, wantErr)
  }
  if families != nil {
    t.Fatalf("Gather() families = %v, want nil", families)
  }
}

func TestScrapeGathererContextTimeout(t *testing.T) {
  var receivedDeadline bool

  collector := &mainTestCollector{
    collectFn: func(ctx context.Context) ([]prometheus.Metric, error) {
      _, receivedDeadline = ctx.Deadline()
      return nil, nil
    },
  }

  gatherer := newScrapeGatherer(
    prometheus.NewRegistry(),
    ExporterDefinition{Namespace: "test"},
    collector,
    time.Second,
    mainTestLogger(),
  )

  if _, err := gatherer.Gather(); err != nil {
    t.Fatalf("Gather() error: %v", err)
  }
  if !receivedDeadline {
    t.Fatal("collector context has no deadline")
  }
}

func TestScrapeGathererWithoutTimeout(t *testing.T) {
  var receivedDeadline bool

  collector := &mainTestCollector{
    collectFn: func(ctx context.Context) ([]prometheus.Metric, error) {
      _, receivedDeadline = ctx.Deadline()
      return nil, nil
    },
  }

  gatherer := newScrapeGatherer(
    prometheus.NewRegistry(),
    ExporterDefinition{Namespace: "test"},
    collector,
    0,
    mainTestLogger(),
  )

  if _, err := gatherer.Gather(); err != nil {
    t.Fatalf("Gather() error: %v", err)
  }
  if receivedDeadline {
    t.Fatal("collector context unexpectedly has a deadline")
  }
}

func TestNewBuildInfoCollector(t *testing.T) {
  oldVersion, oldRevision, oldBuildDate := version, revision, buildDate
  t.Cleanup(func() {
    version, revision, buildDate = oldVersion, oldRevision, oldBuildDate
  })

  version = "1.2.3"
  revision = "abc123"
  buildDate = "2026-01-02T03:04:05Z"

  registry := prometheus.NewRegistry()
  registry.MustRegister(newBuildInfoCollector("test"))

  families, err := registry.Gather()
  if err != nil {
    t.Fatalf("Gather() error: %v", err)
  }

  got := mainTestMetricFamilies(t, families)
  family := got["test_build_info"]
  if family == nil {
    t.Fatal("test_build_info metric family not found")
  }
  if len(family.Metric) != 1 {
    t.Fatalf("metric count = %d, want 1", len(family.Metric))
  }

  metric := family.Metric[0]
  if value := metric.GetGauge().GetValue(); value != 1 {
    t.Errorf("build info value = %v, want 1", value)
  }

  labels := make(map[string]string)
  for _, pair := range metric.Label {
    labels[pair.GetName()] = pair.GetValue()
  }

  wantLabels := map[string]string{
    "version":    version,
    "revision":   revision,
    "build_date": buildDate,
    "go_version": runtime.Version(),
  }
  for name, want := range wantLabels {
    if got := labels[name]; got != want {
      t.Errorf("label %s = %q, want %q", name, got, want)
    }
  }
}

func TestNewLogger(t *testing.T) {
  tests := []struct {
    name       string
    format     string
    level      string
    wantJSON   bool
    wantDebug  bool
    wantInfo   bool
    wantWarn   bool
    wantError  bool
  }{
    {
      name:      "text debug",
      format:    "text",
      level:     "debug",
      wantDebug: true,
      wantInfo:  true,
      wantWarn:  true,
      wantError: true,
    },
    {
      name:      "json info",
      format:    "json",
      level:     "info",
      wantJSON:  true,
      wantInfo:  true,
      wantWarn:  true,
      wantError: true,
    },
    {
      name:      "warn",
      format:    "text",
      level:     "warn",
      wantWarn:  true,
      wantError: true,
    },
    {
      name:      "error",
      format:    "text",
      level:     "error",
      wantError: true,
    },
    {
      name:      "unknown level defaults to info",
      format:    "text",
      level:     "unknown",
      wantInfo:  true,
      wantWarn:  true,
      wantError: true,
    },
    {
      name:      "unknown format defaults to text",
      format:    "unknown",
      level:     "info",
      wantInfo:  true,
      wantWarn:  true,
      wantError: true,
    },
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      var output bytes.Buffer
      logger := newLogger(tt.format, tt.level, &output)

      logger.Debug("debug message")
      logger.Info("info message")
      logger.Warn("warn message")
      logger.Error("error message")

      got := output.String()

      for _, check := range []struct {
        name string
        want bool
      }{
        {"debug message", tt.wantDebug},
        {"info message", tt.wantInfo},
        {"warn message", tt.wantWarn},
        {"error message", tt.wantError},
      } {
        if strings.Contains(got, check.name) != check.want {
          t.Errorf("output contains %q = %v, want %v", check.name, strings.Contains(got, check.name), check.want)
        }
      }

      if strings.Contains(got, "{") != tt.wantJSON {
        t.Errorf("JSON output = %v, want %v; output: %s", strings.Contains(got, "{"), tt.wantJSON, got)
      }
    })
  }
}

func TestRunSpecialArgs(t *testing.T) {
  handler := &mainTestSpecialHandler{handled: true}
  newCollectorCalled := false
  listenCalled := false

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        newCollectorCalled = true
        return &mainTestCollector{}, nil
      },
    },
    handleSpecialArgs: func(_ any, w io.Writer) (bool, error) {
      return handler.HandleSpecialArgs(w)
    },
    newLandingPage: func(web.LandingConfig) (http.Handler, error) {
      return http.NotFoundHandler(), nil
    },
    listenAndServe: func(*http.Server, *web.FlagConfig, *slog.Logger) error {
      listenCalled = true
      return nil
    },
  }

  var stdout, stderr bytes.Buffer
  if err := run(nil, &stdout, &stderr, deps); err != nil {
    t.Fatalf("run() error: %v", err)
  }

  if !handler.called {
    t.Fatal("special handler was not called")
  }
  if newCollectorCalled {
    t.Fatal("collector was initialized for a handled special argument")
  }
  if listenCalled {
    t.Fatal("HTTP server was started for a handled special argument")
  }
}

func TestRunSpecialArgsError(t *testing.T) {
  wantErr := errors.New("special command failure")

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return true, wantErr
    },
  }

  err := run(nil, io.Discard, io.Discard, deps)
  if !errors.Is(err, wantErr) {
    t.Fatalf("run() error = %v, want wrapped %v", err, wantErr)
  }
  if !strings.Contains(err.Error(), "special command failed") {
    t.Fatalf("error %q does not contain wrapper message", err)
  }
}

func TestRunCollectorInitializationError(t *testing.T) {
  wantErr := errors.New("initialization failure")

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return nil, wantErr
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
  }

  err := run([]string{"--web.telemetry-path=/"}, io.Discard, io.Discard, deps)
  if !errors.Is(err, wantErr) {
    t.Fatalf("run() error = %v, want wrapped %v", err, wantErr)
  }
  if !strings.Contains(err.Error(), "failed to initialize exporter") {
    t.Fatalf("error %q does not contain wrapper message", err)
  }
}

func TestRunStartsServerAndClosesCollector(t *testing.T) {
  collector := &mainTestCollector{
    metrics: []prometheus.Metric{mainTestMetric("specific_metric", 7)},
  }

  var (
    serverCalled bool
    landingCalled bool
  )

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace:            "test",
      Name:                 "Test exporter",
      Description:          "Test description",
      DefaultListenAddress: "127.0.0.1:9793",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return collector, nil
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
    newLandingPage: func(config web.LandingConfig) (http.Handler, error) {
      landingCalled = true
      if config.Name != "Test exporter" {
        t.Errorf("landing name = %q", config.Name)
      }
      if config.Description != "Test description" {
        t.Errorf("landing description = %q", config.Description)
      }
      if len(config.Links) != 1 || config.Links[0].Address != "/metrics" {
        t.Errorf("landing links = %#v", config.Links)
      }
      return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        _, _ = io.WriteString(w, "landing")
      }), nil
    },
    listenAndServe: func(server *http.Server, _ *web.FlagConfig, _ *slog.Logger) error {
      serverCalled = true

      if server.Addr != "127.0.0.1:9793" {
        t.Errorf("server address = %q", server.Addr)
      }
      if server.ReadHeaderTimeout <= 0 {
        t.Error("ReadHeaderTimeout was not configured")
      }

      metricsRequest := httptest.NewRecorder()
      server.Handler.ServeHTTP(metricsRequest, httptest.NewRequest(http.MethodGet, "/metrics", nil))
      if metricsRequest.Code != http.StatusOK {
        t.Errorf("metrics status = %d, want %d; body: %s", metricsRequest.Code, http.StatusOK, metricsRequest.Body.String())
      }
      if !strings.Contains(metricsRequest.Body.String(), "specific_metric") {
        t.Errorf("metrics response does not contain specific_metric: %s", metricsRequest.Body.String())
      }

      landingRequest := httptest.NewRecorder()
      server.Handler.ServeHTTP(landingRequest, httptest.NewRequest(http.MethodGet, "/", nil))
      if landingRequest.Code != http.StatusOK {
        t.Errorf("landing status = %d, want %d", landingRequest.Code, http.StatusOK)
      }
      if landingRequest.Body.String() != "landing" {
        t.Errorf("landing body = %q, want %q", landingRequest.Body.String(), "landing")
      }

      return nil
    },
  }

  err := run(
    []string{
      "--web.listen-address=127.0.0.1:9793",
      "--web.disable-exporter-metrics",
    },
    io.Discard,
    io.Discard,
    deps,
  )
  if err != nil {
    t.Fatalf("run() error: %v", err)
  }

  if !serverCalled {
    t.Fatal("listenAndServe was not called")
  }
  if !landingCalled {
    t.Fatal("landing page was not created")
  }
  if !collector.closed {
    t.Fatal("collector was not closed")
  }
}

func TestRunRootTelemetryPathSkipsLandingPage(t *testing.T) {
  collector := &mainTestCollector{}
  landingCalled := false
  serverCalled := false

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return collector, nil
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
    newLandingPage: func(web.LandingConfig) (http.Handler, error) {
      landingCalled = true
      return http.NotFoundHandler(), nil
    },
    listenAndServe: func(server *http.Server, _ *web.FlagConfig, _ *slog.Logger) error {
      serverCalled = true
      recorder := httptest.NewRecorder()
      server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
      if recorder.Code != http.StatusOK {
        t.Errorf("root metrics status = %d, want %d", recorder.Code, http.StatusOK)
      }
      return nil
    },
  }

  err := run(
    []string{"--web.telemetry-path=/", "--web.disable-exporter-metrics"},
    io.Discard,
    io.Discard,
    deps,
  )
  if err != nil {
    t.Fatalf("run() error: %v", err)
  }
  if landingCalled {
    t.Fatal("landing page was created for root telemetry path")
  }
  if !serverCalled {
    t.Fatal("listenAndServe was not called")
  }
  if !collector.closed {
    t.Fatal("collector was not closed")
  }
}

func TestRunLandingPageError(t *testing.T) {
  wantErr := errors.New("landing page failure")
  collector := &mainTestCollector{}

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return collector, nil
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
    newLandingPage: func(web.LandingConfig) (http.Handler, error) {
      return nil, wantErr
    },
    listenAndServe: func(*http.Server, *web.FlagConfig, *slog.Logger) error {
      t.Fatal("listenAndServe called after landing page failure")
      return nil
    },
  }

  err := run(nil, io.Discard, io.Discard, deps)
  if !errors.Is(err, wantErr) {
    t.Fatalf("run() error = %v, want wrapped %v", err, wantErr)
  }
  if !collector.closed {
    t.Fatal("collector was not closed after landing page failure")
  }
}

func TestRunListenAndServeError(t *testing.T) {
  wantErr := errors.New("server failure")
  collector := &mainTestCollector{}

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return collector, nil
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
    newLandingPage: func(web.LandingConfig) (http.Handler, error) {
      return http.NotFoundHandler(), nil
    },
    listenAndServe: func(*http.Server, *web.FlagConfig, *slog.Logger) error {
      return wantErr
    },
  }

  err := run(
    []string{"--web.telemetry-path=/", "--web.disable-exporter-metrics"},
    io.Discard,
    io.Discard,
    deps,
  )
  if !errors.Is(err, wantErr) {
    t.Fatalf("run() error = %v, want wrapped %v", err, wantErr)
  }
  if !strings.Contains(err.Error(), "HTTP server stopped") {
    t.Fatalf("error %q does not contain wrapper message", err)
  }
  if !collector.closed {
    t.Fatal("collector was not closed after server error")
  }
}

func TestRunInvalidCLI(t *testing.T) {
  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
    },
  }

  err := run([]string{"--unknown-option"}, io.Discard, io.Discard, deps)
  if err == nil {
    t.Fatal("run() error = nil, want CLI parsing error")
  }
}

func TestMain(t *testing.T) {
  oldRunApplication := runApplication
  oldExitProcess := exitProcess
  t.Cleanup(func() {
    runApplication = oldRunApplication
    exitProcess = oldExitProcess
  })

  t.Run("success", func(t *testing.T) {
    called := false
    exitCalled := false

    runApplication = func(args []string, stdout, stderr io.Writer) error {
      called = true
      return nil
    }
    exitProcess = func(code int) {
      exitCalled = true
    }

    main()

    if !called {
      t.Fatal("runApplication was not called")
    }
    if exitCalled {
      t.Fatal("exitProcess was called after successful run")
    }
  })

  t.Run("failure", func(t *testing.T) {
    wantErr := errors.New("run failure")
    exitCode := -1

    runApplication = func(args []string, stdout, output io.Writer) error {
      _, _ = io.WriteString(output, "")
      return wantErr
    }
    exitProcess = func(code int) {
      exitCode = code
    }

    // main() writes to os.Stderr directly. The error path and exit code
    // are verified through the substituted functions.
    main()

    if exitCode != 1 {
      t.Fatalf("exit code = %d, want 1", exitCode)
    }
  })
}

func TestMainTestMetricFamiliesHelper(t *testing.T) {
  families := []*dto.MetricFamily{
    {Name: stringPointer("one")},
    {Name: stringPointer("two")},
  }

  got := mainTestMetricFamilies(t, families)
  if len(got) != 2 {
    t.Fatalf("family map length = %d, want 2", len(got))
  }
}

func stringPointer(value string) *string {
  return &value
}

func TestRunCloseErrorDoesNotReplaceServerResult(t *testing.T) {
  collector := &mainTestCollector{
    closeErr: errors.New("close failure"),
  }

  deps := mainDependencies{
    definition: ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
      NewCollector: func(SpecificCLI, *slog.Logger) (SpecificCollector, error) {
        return collector, nil
      },
    },
    handleSpecialArgs: func(any, io.Writer) (bool, error) {
      return false, nil
    },
    newLandingPage: func(web.LandingConfig) (http.Handler, error) {
      return http.NotFoundHandler(), nil
    },
    listenAndServe: func(*http.Server, *web.FlagConfig, *slog.Logger) error {
      return nil
    },
  }

  if err := run(
    []string{"--web.telemetry-path=/", "--web.disable-exporter-metrics"},
    io.Discard,
    io.Discard,
    deps,
  ); err != nil {
    t.Fatalf("run() returned close error, want nil: %v", err)
  }
  if !collector.closed {
    t.Fatal("collector was not closed")
  }
}

func TestMainTestCollectorClose(t *testing.T) {
  wantErr := fmt.Errorf("close failed")
  collector := &mainTestCollector{closeErr: wantErr}

  err := collector.Close()
  if !errors.Is(err, wantErr) {
    t.Fatalf("Close() error = %v, want %v", err, wantErr)
  }
  if !collector.closed {
    t.Fatal("closed = false, want true")
  }
}
