package main

import (
  "errors"
  "reflect"
  "strings"
  "testing"
  "time"

  "github.com/alecthomas/kong"
  "gopkg.in/yaml.v3"
)

func TestNonNegativeDurationUnmarshalText(t *testing.T) {
  tests := []struct {
    name    string
    input   string
    want    time.Duration
    wantErr bool
  }{
    {name: "seconds", input: "10s", want: 10 * time.Second},
    {name: "compound", input: "1m30s", want: 90 * time.Second},
    {name: "zero", input: "0s", want: 0},
    {name: "negative parses but fails validation", input: "-1s", want: -time.Second},
    {name: "invalid", input: "invalid", wantErr: true},
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      var got NonNegativeDuration
      err := got.UnmarshalText([]byte(tt.input))

      if tt.wantErr {
        if err == nil {
          t.Fatal("UnmarshalText() error = nil, want error")
        }
        return
      }

      if err != nil {
        t.Fatalf("UnmarshalText() error: %v", err)
      }
      if got.Duration() != tt.want {
        t.Fatalf("duration = %s, want %s", got.Duration(), tt.want)
      }
    })
  }
}

func TestNonNegativeDurationValidate(t *testing.T) {
  tests := []struct {
    name    string
    value   NonNegativeDuration
    wantErr bool
  }{
    {name: "zero", value: 0},
    {name: "positive", value: NonNegativeDuration(time.Second)},
    {name: "negative", value: NonNegativeDuration(-time.Second), wantErr: true},
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      err := tt.value.Validate()
      if (err != nil) != tt.wantErr {
        t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
      }
      if tt.wantErr && !strings.Contains(err.Error(), "must not be negative") {
        t.Fatalf("unexpected validation error: %v", err)
      }
    })
  }
}

func TestNonNegativeDurationDuration(t *testing.T) {
  value := NonNegativeDuration(1500 * time.Millisecond)
  if got, want := value.Duration(), 1500*time.Millisecond; got != want {
    t.Fatalf("Duration() = %s, want %s", got, want)
  }
}

func TestNewConfigFilePolicyRejectsInvalidInput(t *testing.T) {
  tests := []struct {
    name  string
    input any
  }{
    {name: "nil", input: nil},
    {name: "non-pointer", input: CommonCLI{}},
    {name: "nil pointer", input: (*CommonCLI)(nil)},
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      policy, err := newConfigFilePolicy(tt.input)
      if err == nil {
        t.Fatal("newConfigFilePolicy() error = nil, want error")
      }
      if policy != nil {
        t.Fatalf("policy = %#v, want nil", policy)
      }
    })
  }
}

func TestNewConfigFilePolicy(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

  excluded := []any{
    &cli.Common.Config.File,
    &cli.Common.Version,
    &cli.Specific.Versions,
  }

  for _, target := range excluded {
    if _, ok := policy.excluded[target]; !ok {
      t.Errorf("field pointer %T was not marked as excluded", target)
    }
  }
}

func TestNewConfigFilePolicyRejectsInvalidTag(t *testing.T) {
  type invalidConfig struct {
    Value string `configfile:"not-a-bool"`
  }

  policy := &configFilePolicy{excluded: make(map[any]struct{})}
  err := policy.inspect(reflect.ValueOf(invalidConfig{}))
  if err == nil {
    t.Fatal("inspect() error = nil, want invalid tag error")
  }
  if !strings.Contains(err.Error(), "invalid configfile tag") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestConfigFilePolicyExcludes(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

    tests := []struct {
      name   string
      target reflect.Value
      want   bool
    }{
      {
        name:   "excluded version field",
        target: reflect.ValueOf(&cli.Common.Version).Elem(),
        want:   true,
      },
      {
        name:   "ordinary field",
        target: reflect.ValueOf(&cli.Common.Log.Level).Elem(),
        want:   false,
      },
      {
        name:   "invalid target",
        target: reflect.Value{},
        want:   false,
      },
    }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      flag := &kong.Flag{
          Value: &kong.Value{
              Target: tt.target,
          },
      }
      if got := policy.excludes(flag); got != tt.want {
        t.Fatalf("excludes() = %v, want %v", got, tt.want)
      }
    })
  }
}

type configTestNamedReader struct {
  *strings.Reader
  name string
}

func (r configTestNamedReader) Name() string {
  return r.name
}

func TestConfigLoaderRequiresFilename(t *testing.T) {
  loader := configLoader(nil)

  _, err := loader(strings.NewReader("web: {}"))
  if err == nil {
    t.Fatal("loader error = nil, want filename error")
  }
  if !strings.Contains(err.Error(), "does not expose filename") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestConfigLoaderRejectsUnsupportedExtension(t *testing.T) {
  loader := configLoader(nil)

  _, err := loader(configTestNamedReader{
    Reader: strings.NewReader("web: {}"),
    name:   "config.toml",
  })
  if err == nil {
    t.Fatal("loader error = nil, want unsupported format error")
  }
  if !strings.Contains(err.Error(), "only YAML is supported") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestConfigLoaderAcceptsYAMLExtensions(t *testing.T) {
  for _, filename := range []string{"config.yaml", "config.yml", "CONFIG.YAML"} {
    t.Run(filename, func(t *testing.T) {
      loader := configLoader(nil)
      resolver, err := loader(configTestNamedReader{
        Reader: strings.NewReader("web:\n  telemetry-path: /metrics\n"),
        name:   filename,
      })
      if err != nil {
        t.Fatalf("loader() error: %v", err)
      }
      if resolver == nil {
        t.Fatal("loader() returned nil resolver")
      }
    })
  }
}

func TestNewYAMLConfigResolver(t *testing.T) {
  tests := []struct {
    name    string
    input   string
    wantErr bool
    wantLen int
  }{
    {
      name:    "mapping",
      input:   "web:\n  telemetry-path: /metrics\n",
      wantLen: 1,
    },
    {
      name:    "empty document",
      input:   "",
      wantLen: 0,
    },
    {
      name:    "null document",
      input:   "null\n",
      wantLen: 0,
    },
    {
      name:    "invalid YAML",
      input:   "web: [",
      wantErr: true,
    },
    {
      name:    "multiple documents",
      input:   "web: {}\n---\nlog: {}\n",
      wantErr: true,
    },
    {
      name:    "invalid trailing document",
      input:   "web: {}\n---\nlog: [\n",
      wantErr: true,
    },
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      resolver, err := newYAMLConfigResolver(strings.NewReader(tt.input), "test.yaml")

      if tt.wantErr {
        if err == nil {
          t.Fatal("newYAMLConfigResolver() error = nil, want error")
        }
        return
      }
      if err != nil {
        t.Fatalf("newYAMLConfigResolver() error: %v", err)
      }
      if len(resolver.config) != tt.wantLen {
        t.Fatalf("config length = %d, want %d", len(resolver.config), tt.wantLen)
      }
      if resolver.source != "test.yaml" {
        t.Fatalf("source = %q, want test.yaml", resolver.source)
      }
    })
  }
}

func TestLookupYAML(t *testing.T) {
  config := map[string]any{
    "web": map[string]any{
      "config": map[string]any{
        "file": "web.yml",
      },
      "telemetry-path": "/metrics",
    },
    "enabled": true,
  }

  tests := []struct {
    name string
    path []string
    want any
    ok   bool
  }{
    {
      name: "nested value",
      path: []string{"web", "config", "file"},
      want: "web.yml",
      ok:   true,
    },
    {
      name: "top-level value",
      path: []string{"enabled"},
      want: true,
      ok:   true,
    },
    {
      name: "missing key",
      path: []string{"web", "missing"},
      ok:   false,
    },
    {
      name: "path through scalar",
      path: []string{"enabled", "nested"},
      ok:   false,
    },
    {
      name: "empty path",
      path: nil,
      want: config,
      ok:   true,
    },
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      got, ok := lookupYAML(config, tt.path)
      if ok != tt.ok {
        t.Fatalf("lookupYAML() ok = %v, want %v", ok, tt.ok)
      }
      if !reflect.DeepEqual(got, tt.want) {
        t.Fatalf("lookupYAML() = %#v, want %#v", got, tt.want)
      }
    })
  }
}

func TestSplitConfigPath(t *testing.T) {
  tests := []struct {
    input string
    want  []string
  }{
    {input: "web.telemetry-path", want: []string{"web", "telemetry-path"}},
    {input: "web..config.file", want: []string{"web", "config", "file"}},
    {input: ".leading.", want: []string{"leading"}},
    {input: "", want: []string{}},
    {input: "single", want: []string{"single"}},
  }

  for _, tt := range tests {
    t.Run(tt.input, func(t *testing.T) {
      got := splitConfigPath(tt.input)
      if !reflect.DeepEqual(got, tt.want) {
        t.Fatalf("splitConfigPath(%q) = %#v, want %#v", tt.input, got, tt.want)
      }
    })
  }
}

func TestConfigPathFromNodes(t *testing.T) {
  nodes := []*kong.Node{
    {Name: "web"},
    {Name: "config.file"},
  }
  flag := &kong.Flag{
    Value: &kong.Value{
      Name: "path.value",
    },
  }

  got := configPathFromNodes(nodes, flag)
  want := []string{"web", "config", "file", "path", "value"}

  if !reflect.DeepEqual(got, want) {
    t.Fatalf("configPathFromNodes() = %#v, want %#v", got, want)
  }
}

func TestConfigSchemaAdd(t *testing.T) {
  schema := newConfigSchema()
  schema.add([]string{"database", "connection-string"}, false)
  schema.add([]string{"database", "labels"}, true)

  if !schema.children["database"].children["connection-string"].leaf {
    t.Fatal("connection-string schema node is not a leaf")
  }

  labels := schema.children["database"].children["labels"]
  if !labels.leaf {
    t.Fatal("labels schema node is not a leaf")
  }
  if !labels.anyMap {
    t.Fatal("labels schema node does not allow arbitrary map keys")
  }
}

func TestValidateYAMLConfig(t *testing.T) {
  schema := newConfigSchema()
  schema.add([]string{"web", "telemetry-path"}, false)
  schema.add([]string{"database", "labels"}, true)

  tests := []struct {
    name    string
    value   any
    wantErr bool
    unknown []string
  }{
    {
      name: "valid configuration",
      value: map[string]any{
        "web": map[string]any{
          "telemetry-path": "/metrics",
        },
      },
    },
    {
      name: "unknown nested key",
      value: map[string]any{
        "web": map[string]any{
          "unknown": true,
        },
      },
      unknown: []string{"web.unknown"},
    },
    {
      name: "unknown root key",
      value: map[string]any{
        "unknown": true,
      },
      unknown: []string{"unknown"},
    },
    {
      name: "arbitrary map keys",
      value: map[string]any{
        "database": map[string]any{
          "labels": map[string]any{
            "custom-label": "value",
          },
        },
      },
    },
    {
      name:    "expected mapping",
      value:   "not a mapping",
      wantErr: true,
    },
    {
      name: "nested expected mapping",
      value: map[string]any{
        "web": "not a mapping",
      },
      wantErr: true,
    },
  }

  for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
      var unknown []string
      err := validateYAMLConfig("", tt.value, schema, &unknown)

      if (err != nil) != tt.wantErr {
        t.Fatalf("validateYAMLConfig() error = %v, wantErr %v", err, tt.wantErr)
      }
      if !reflect.DeepEqual(unknown, tt.unknown) {
        t.Fatalf("unknown keys = %#v, want %#v", unknown, tt.unknown)
      }
    })
  }
}

func TestValidateYAMLConfigLeafAcceptsScalar(t *testing.T) {
  schema := newConfigSchema()
  schema.add([]string{"log"}, false)

  var unknown []string

  err := validateYAMLConfig(
    "log",
    "info",
    schema.children["log"],
    &unknown,
  )
  if err != nil {
    t.Fatalf("validateYAMLConfig() error: %v", err)
  }

  if len(unknown) != 0 {
    t.Errorf("unknown keys = %v, want none", unknown)
  }
}

func TestYAMLConfigResolverValidate(t *testing.T) {
  // A schema leaf accepts its value without requiring it to be a mapping.
  schema := newConfigSchema()
  schema.add([]string{"value"}, false)

  var unknown []string
  if err := validateYAMLConfig("", map[string]any{"value": "text"}, schema, &unknown); err != nil {
    t.Fatalf("validateYAMLConfig() error: %v", err)
  }
}

func TestConfigPolicyResolverExcludesField(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

  resolver := &configPolicyResolver{
    resolver: configTestResolver{},
    policy:   policy,
  }

  flag := &kong.Flag{
    Value: &kong.Value{
      Target: reflect.ValueOf(&cli.Common.Version).Elem(),
    },
  }

  value, err := resolver.Resolve(nil, nil, flag)
  if err != nil {
    t.Fatalf("Resolve() error: %v", err)
  }
  if value != nil {
    t.Fatalf("Resolve() value = %#v, want nil for excluded field", value)
  }
}

type configTestResolver struct{}

func (configTestResolver) Resolve(_ *kong.Context, _ *kong.Path, _ *kong.Flag) (any, error) {
  return "resolved", nil
}

func (configTestResolver) Validate(_ *kong.Application) error {
  return nil
}

func TestConfigPolicyResolverDelegates(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

  resolver := &configPolicyResolver{
    resolver: configTestResolver{},
    policy:   policy,
  }

  flag := &kong.Flag{
    Value: &kong.Value{
      Target: reflect.ValueOf(cli.Common.Log.Level),
    },
  }

  value, err := resolver.Resolve(nil, nil, flag)
  if err != nil {
    t.Fatalf("Resolve() error: %v", err)
  }
  if value != "resolved" {
    t.Fatalf("Resolve() value = %#v, want %q", value, "resolved")
  }
}

func TestConfigPolicyResolverValidateDelegates(t *testing.T) {
  resolver := &configPolicyResolver{
    resolver: configTestResolver{},
    policy:   &configFilePolicy{excluded: make(map[any]struct{})},
  }

  if err := resolver.Validate(nil); err != nil {
    t.Fatalf("Validate() error: %v", err)
  }
}

func TestParseCLI(t *testing.T) {
  var stdout, stderr strings.Builder

  cli, err := parseCLI(
    nil,
    &stdout,
    &stderr,
    ExporterDefinition{
      Namespace:            "test",
      Name:                 "test_exporter",
      Description:          "Test exporter",
      DefaultListenAddress: "127.0.0.1:9793",
    },
    "1.2.3",
  )
  if err != nil {
    t.Fatalf("parseCLI() error: %v; stderr: %s", err, stderr.String())
  }

  if cli.Specific.Web.ListenAddress != ":9793" {
    t.Errorf("listen address = %q, want :9793", cli.Specific.Web.ListenAddress)
  }
  if cli.Common.Web.TelemetryPath != "/metrics" {
    t.Errorf("telemetry path = %q, want /metrics", cli.Common.Web.TelemetryPath)
  }
  if cli.Common.Log.Level != "info" {
    t.Errorf("log level = %q, want info", cli.Common.Log.Level)
  }
  if cli.Common.Log.Format != "text" {
    t.Errorf("log format = %q, want text", cli.Common.Log.Format)
  }
  if cli.Common.Scrape.Timeout.Duration() != 10*time.Second {
    t.Errorf("scrape timeout = %s, want 10s", cli.Common.Scrape.Timeout.Duration())
  }
}

func TestParseCLIOverridesFlags(t *testing.T) {
  cli, err := parseCLI(
    []string{
      "--web.telemetry-path=/custom",
      "--web.disable-exporter-metrics",
      "--scrape.timeout=3s",
      "--log.level=debug",
      "--log.format=json",
      "--web.listen-address=127.0.0.1:9999",
    },
    nil,
    nil,
    ExporterDefinition{
      Namespace: "test",
      Name:      "test_exporter",
    },
    "test-version",
  )
  if err != nil {
    t.Fatalf("parseCLI() error: %v", err)
  }

  if cli.Common.Web.TelemetryPath != "/custom" {
    t.Errorf("telemetry path = %q", cli.Common.Web.TelemetryPath)
  }
  if !cli.Common.Web.DisableExporterMetrics {
    t.Error("DisableExporterMetrics = false, want true")
  }
  if cli.Common.Scrape.Timeout.Duration() != 3*time.Second {
    t.Errorf("scrape timeout = %s, want 3s", cli.Common.Scrape.Timeout.Duration())
  }
  if cli.Common.Log.Level != "debug" {
    t.Errorf("log level = %q, want debug", cli.Common.Log.Level)
  }
  if cli.Common.Log.Format != "json" {
    t.Errorf("log format = %q, want json", cli.Common.Log.Format)
  }
  if cli.Specific.Web.ListenAddress != "127.0.0.1:9999" {
    t.Errorf("listen address = %q", cli.Specific.Web.ListenAddress)
  }
}

func TestParseCLIRejectsInvalidDuration(t *testing.T) {
  _, err := parseCLI(
    []string{"--scrape.timeout=-1s"},
    nil,
    nil,
    ExporterDefinition{Namespace: "test", Name: "test_exporter"},
    "test",
  )
  if err == nil {
    t.Fatal("parseCLI() error = nil, want validation error")
  }
}

func TestParseCLIRejectsUnknownFlag(t *testing.T) {
  _, err := parseCLI(
    []string{"--not-a-real-option"},
    nil,
    nil,
    ExporterDefinition{Namespace: "test", Name: "test_exporter"},
    "test",
  )
  if err == nil {
    t.Fatal("parseCLI() error = nil, want unknown flag error")
  }
}

func TestYAMLConfigDecoderUsesYAML(t *testing.T) {
  input := "enabled: true\ncount: 3\n"
  resolver, err := newYAMLConfigResolver(strings.NewReader(input), "test.yaml")
  if err != nil {
    t.Fatalf("newYAMLConfigResolver() error: %v", err)
  }

  var decoded map[string]any
  raw, err := yaml.Marshal(resolver.config)
  if err != nil {
    t.Fatalf("yaml.Marshal() error: %v", err)
  }
  if err := yaml.Unmarshal(raw, &decoded); err != nil {
    t.Fatalf("yaml.Unmarshal() error: %v", err)
  }
  if decoded["enabled"] != true {
    t.Errorf("enabled = %#v, want true", decoded["enabled"])
  }
}

func TestConfigFilePolicyInspectNilPointerField(t *testing.T) {
  type nested struct {
    Value string `configfile:"false"`
  }
  type config struct {
    Nested *nested `embed:""`
  }

  value := config{}
  policy := &configFilePolicy{excluded: make(map[any]struct{})}

  if err := policy.inspect(reflect.ValueOf(value)); err != nil {
    t.Fatalf("inspect() error: %v", err)
  }
  if len(policy.excluded) != 0 {
    t.Fatalf("excluded count = %d, want 0 for nil embedded pointer", len(policy.excluded))
  }
}

func TestConfigSchemaUnknownKeysAreCollected(t *testing.T) {
  schema := newConfigSchema()
  schema.add([]string{"known"}, false)

  var unknown []string
  err := validateYAMLConfig(
    "",
    map[string]any{
      "z":     1,
      "a":     2,
      "known": "value",
    },
    schema,
    &unknown,
  )
  if err != nil {
    t.Fatalf("validateYAMLConfig() error: %v", err)
  }
  if len(unknown) != 2 {
    t.Fatalf("unknown keys = %#v, want 2 entries", unknown)
  }
}

func TestConfigFilePolicyMapContainsPointers(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

  if _, ok := policy.excluded[&cli.Common.Version]; !ok {
    t.Fatal("Common.Version pointer not found in policy")
  }
  if _, ok := policy.excluded[&cli.Specific.Versions]; !ok {
    t.Fatal("Specific.Versions pointer not found in policy")
  }
}

func TestYAMLResolverMultipleDocumentsErrorIsWrapped(t *testing.T) {
  _, err := newYAMLConfigResolver(
    strings.NewReader("one: 1\n---\ntwo: 2\n"),
    "multi.yaml",
  )
  if err == nil {
    t.Fatal("expected multiple document error")
  }
  if !strings.Contains(err.Error(), "multiple YAML documents") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestValidateYAMLConfigInvalidRootType(t *testing.T) {
  schema := newConfigSchema()
  schema.add([]string{"web", "path"}, false)

  var unknown []string
  err := validateYAMLConfig("", []any{"not", "a", "mapping"}, schema, &unknown)
  if err == nil {
    t.Fatal("expected mapping type error")
  }
  if !strings.Contains(err.Error(), "expected a mapping") {
    t.Fatalf("unexpected error: %v", err)
  }
}

func TestConfigFilePolicyInspectInvalidTagOnEmbeddedField(t *testing.T) {
  type config struct {
    Nested struct {
      Value string
    } `embed:"" configfile:"invalid"`
  }

  policy := &configFilePolicy{excluded: make(map[any]struct{})}
  err := policy.inspect(reflect.ValueOf(config{}))
  if err == nil {
    t.Fatal("inspect() error = nil, want invalid embedded tag error")
  }
}

func TestConfigPolicyResolverDoesNotResolveExcludedField(t *testing.T) {
  cli := CLI{}
  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    t.Fatalf("newConfigFilePolicy() error: %v", err)
  }

  resolver := &configPolicyResolver{
    resolver: configTestResolver{},
    policy:   policy,
  }

  flag := &kong.Flag{
    Value: &kong.Value{
      Target: reflect.ValueOf(&cli.Common.Version).Elem(),
    },
  }
  value, err := resolver.Resolve(nil, nil, flag)
  if err != nil {
    t.Fatalf("Resolve() error: %v", err)
  }
  if value != nil {
    t.Fatalf("Resolve() = %#v, want nil", value)
  }
}

func TestNewYAMLConfigResolverRejectsNonMappingRoot(t *testing.T) {
  _, err := newYAMLConfigResolver(
    strings.NewReader("- one\n- two\n"),
    "list.yaml",
  )
  if err == nil {
    t.Fatal("newYAMLConfigResolver() error = nil, want error")
  }
}

func TestConfigPolicyResolverUnderlyingError(t *testing.T) {
  wantErr := errors.New("resolver failure")
  resolver := &configPolicyResolver{
    resolver: configTestErrorResolver{err: wantErr},
    policy:   &configFilePolicy{excluded: make(map[any]struct{})},
  }

  err := resolver.Validate(nil)
  if !errors.Is(err, wantErr) {
    t.Fatalf("Validate() error = %v, want %v", err, wantErr)
  }
}

type configTestErrorResolver struct {
  err error
}

func (r configTestErrorResolver) Resolve(_ *kong.Context, _ *kong.Path, _ *kong.Flag) (any, error) {
  return nil, r.err
}

func (r configTestErrorResolver) Validate(_ *kong.Application) error {
  return r.err
}
