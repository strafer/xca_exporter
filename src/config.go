package main

import (
  "errors"
  "fmt"
  "io"
  "path/filepath"
  "reflect"
  "sort"
  "strconv"
  "strings"
  "time"

  "github.com/alecthomas/kong"
  "gopkg.in/yaml.v3"
)


// CLI contains all common and exporter-specific CLI options.
type CLI struct {
  Common   CommonCLI   `embed:""`
  Specific SpecificCLI `embed:""`
}


// CommonCLI contains common exporter options.
type CommonCLI struct {
  Config struct {
    File kong.ConfigFlag `help:"Load exporter configuration from a YAML file."`
  } `embed:"" prefix:"config." configfile:"false"`

  Web struct {
    Config struct {
      File string `help:"Configuration file for TLS and/or basic authentication." default:""`
    } `embed:"" prefix:"config."`

    TelemetryPath          string               `help:"Path under which to expose metrics." default:"/metrics"`
    SystemdSocket          bool                 `help:"Use systemd socket activation instead of listening on web.listen-address (default: ${default})."  default:"false"`
    DisableExporterMetrics bool                 `help:"Exclude standard exporter self-metrics such as promhttp_*, process_* and go_* (default: ${default})."  default:"false"`
    ReadHeaderTimeout      NonNegativeDuration  `help:"Maximum time for reading HTTP headers."  default:"5s"`
    ReadTimeout            NonNegativeDuration  `help:"Maximum time for reading the entire HTTP request."  default:"10s"`
    WriteTimeout           NonNegativeDuration  `help:"Maximum time for sending an HTTP response."  default:"15s"`
    IdleTimeout            NonNegativeDuration  `help:"Maximum wait time for the next request on a keep-alive connection."  default:"60s"`
  } `embed:"" prefix:"web."`

  Scrape struct {
    Timeout NonNegativeDuration `help:"Maximum time allowed for one exporter scrape." default:"10s"`
  } `embed:"" prefix:"scrape."`

  Log struct {
    Level  string `enum:"debug,info,warn,error" help:"Log level." default:"info"`
    Format string `enum:"text,json" help:"Log format." default:"text"`
  } `embed:"" prefix:"log."`

  Version kong.VersionFlag `short:"V" help:"Show exporter version and exit." env:"-" configfile:"false"`
}


// NonNegativeDuration is a type for simplifying non-negativity checks.
type NonNegativeDuration time.Duration

// UnmarshalText parses a duration using Go's standard duration syntax.
func (d *NonNegativeDuration) UnmarshalText(text []byte) error {
    value, err := time.ParseDuration(string(text))
    if err != nil {
        return err
    }

    *d = NonNegativeDuration(value)
    return nil
}

// Validate rejects negative durations.
func (d NonNegativeDuration) Validate() error {
    if d < 0 {
        return errors.New("must not be negative")
    }
    return nil
}

// Duration returns the duration time in time.Duration format.
func (d NonNegativeDuration) Duration() time.Duration {
  return time.Duration(d)
}


// parseCLI parses command-line arguments, environment variables
// and the optional YAML configuration file.
func parseCLI(
  args []string,
  stdout io.Writer,
  stderr io.Writer,
  definition ExporterDefinition,
  version string,
) (
  CLI,
  error,
) {
  var cli CLI

  policy, err := newConfigFilePolicy(&cli)
  if err != nil {
    return cli, fmt.Errorf("initialize configuration policy: %w", err)
  }

  parser, err := kong.New(
    &cli,
    kong.Configuration(configLoader(policy)),
    kong.DefaultEnvars(strings.ToUpper(definition.Name)),
    kong.Name(definition.Name),
    kong.Description(definition.Description),
    kong.Vars{
      "version":                version,
      "default_listen_address": definition.DefaultListenAddress,
    },
    kong.Writers(stdout, stderr),
  )
  if err != nil {
    return cli, fmt.Errorf("initialize CLI parser: %w", err)
  }

  if _, err := parser.Parse(args); err != nil {
    return cli, fmt.Errorf("parse command line: %w", err)
  }

  return cli, nil
}


// configLoader creates a YAML configuration resolver with field filtering.
func configLoader(
  policy *configFilePolicy,
) kong.ConfigurationLoader {
  return func(r io.Reader) (kong.Resolver, error) {
    named, ok := r.(interface {
      Name() string
    })
    if !ok {
      return nil, fmt.Errorf("configuration reader does not expose filename")
    }

    filename := named.Name()

    switch strings.ToLower(filepath.Ext(filename)) {
    case ".yaml", ".yml":
    default:
      return nil, fmt.Errorf(
        "unsupported configuration format %q: only YAML is supported",
        filepath.Ext(filename),
      )
    }

    resolver, err := newYAMLConfigResolver(r, filename)
    if err != nil {
      return nil, fmt.Errorf("load configuration %s: %w", filename, err)
    }
    
    resolver.policy = policy
    
    return &configPolicyResolver{
      resolver: resolver,
      policy:   policy,
    }, nil
  }
}


// configFilePolicy stores fields excluded from YAML configuration.
type configFilePolicy struct {
  excluded map[any]struct{}
}


// newConfigFilePolicy collects fields marked configfile:"false".
func newConfigFilePolicy(
  cli any,
) (
  *configFilePolicy,
  error,
) {
  policy := &configFilePolicy{
    excluded: make(map[any]struct{}),
  }

  value := reflect.ValueOf(cli)
  if value.Kind() != reflect.Pointer || value.IsNil() {
    return nil, fmt.Errorf("CLI must be a non-nil pointer")
  }

  if err := policy.inspect(value.Elem()); err != nil {
    return nil, err
  }

  return policy, nil
}


// inspect serves as an entry point for the function that validates parameters
// for the configuration file.
func (p *configFilePolicy) inspect(
  value reflect.Value,
) error {
  return p.inspectValue(value, false)
}


// inspectValue recursively inspects Kong-embedded structures.
// A configfile:"false" tag on an embedded structure excludes
// all its descendants from configuration files.
func (p *configFilePolicy) inspectValue(
  value reflect.Value,
  inheritedExcluded bool,
) error {
  if value.Kind() == reflect.Pointer {
    if value.IsNil() {
      return nil
    }
    value = value.Elem()
  }

  if value.Kind() != reflect.Struct {
    return nil
  }

  typ := value.Type()

  for i := 0; i < value.NumField(); i++ {
    fieldType := typ.Field(i)
    fieldValue := value.Field(i)

    if fieldType.PkgPath != "" {
      continue
    }

    excluded := inheritedExcluded

    if raw, exists := fieldType.Tag.Lookup("configfile"); exists {
      allowed, err := strconv.ParseBool(raw)
      if err != nil {
        return fmt.Errorf(
          "invalid configfile tag on %s: %w",
          fieldType.Name,
          err,
        )
      }

      if !allowed {
        excluded = true
      }
    }

    if _, embedded := fieldType.Tag.Lookup("embed"); embedded {
      if err := p.inspectValue(fieldValue, excluded); err != nil {
        return fmt.Errorf("%s: %w", fieldType.Name, err)
      }

      continue
    }

    if !excluded {
      continue
    }

    if !fieldValue.CanAddr() || !fieldValue.Addr().CanInterface() {
      return fmt.Errorf(
        "cannot inspect configfile field %s",
        fieldType.Name,
      )
    }

    p.excluded[fieldValue.Addr().Interface()] = struct{}{}
  }

  return nil
}


// excludes reports whether a Kong flag is excluded from YAML configuration.
func (p *configFilePolicy) excludes(
  flag *kong.Flag,
) bool {
  target := flag.Target

  if !target.IsValid() {
    return false
  }

  if target.Kind() == reflect.Pointer {
    if target.IsNil() {
      return false
    }

    target = target.Elem()
  }

  if !target.CanAddr() || !target.Addr().CanInterface() {
    return false
  }

  _, excluded := p.excluded[target.Addr().Interface()]
  return excluded
}


// configPolicyResolver filters configuration values before applying them.
type configPolicyResolver struct {
  resolver kong.Resolver
  policy   *configFilePolicy
}


// Resolve searches for a parameter in the configuration file.
func (r *configPolicyResolver) Resolve(
  ctx *kong.Context,
  parent *kong.Path,
  flag *kong.Flag,
) (any, error) {
  if r.policy.excludes(flag) {
    return nil, nil
  }

  return r.resolver.Resolve(ctx, parent, flag)
}


// Validate implements the kong.Resolver interface.
func (r *configPolicyResolver) Validate(
  app *kong.Application,
) error {
  return r.resolver.Validate(app)
}


// yamlConfigResolver resolves values from a hierarchical YAML document.
type yamlConfigResolver struct {
  config map[string]any
  source string
  schema *configSchema
  policy *configFilePolicy
}


// newYAMLConfigResolver decodes a YAML document.
func newYAMLConfigResolver(
  reader io.Reader,
  source string,
) (
  *yamlConfigResolver,
  error,
) {
  decoder := yaml.NewDecoder(reader)

  config := make(map[string]any)
  if err := decoder.Decode(&config); err != nil && !errors.Is(err, io.EOF) {
    return nil, fmt.Errorf("decode YAML: %w", err)
  }

  // An empty YAML document is equivalent to an empty configuration.
  if config == nil {
    config = make(map[string]any)
  }

  // A configuration file must contain exactly one YAML document.
  var extra any
  if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
    if err == nil {
      return nil, fmt.Errorf("multiple YAML documents are not supported")
    }
    return nil, fmt.Errorf("decode trailing YAML document: %w", err)
  }

  return &yamlConfigResolver{
    config: config,
    source: source,
  }, nil
}

// Resolve retrieves a value using the hierarchical Kong flag path.
func (r *yamlConfigResolver) Resolve(
  _ *kong.Context,
  parent *kong.Path,
  flag *kong.Flag,
) (
  any,
  error,
) {
  path := configPath(parent, flag)

  value, ok := lookupYAML(r.config, path)
  if !ok {
    return nil, nil
  }

  return value, nil
}


// Validate checks all YAML keys against the Kong application model.
func (r *yamlConfigResolver) Validate(
  app *kong.Application,
) error {
  schema := newConfigSchema()

  var nodes []*kong.Node

  err := kong.Visit(app, func(
    node kong.Visitable,
    next kong.Next,
  ) error {
    switch node := node.(type) {
      case *kong.Application:
        nodes = append(nodes, node.Node)
        defer func() {
          nodes = nodes[:len(nodes)-1]
        }()

      case *kong.Node:
        nodes = append(nodes, node)
        defer func() {
          nodes = nodes[:len(nodes)-1]
        }()

      case *kong.Flag:
        if r.policy != nil && r.policy.excludes(node) {
          break
        }

        path := configPathFromNodes(nodes, node)
        schema.add(path, node.Target.Kind() == reflect.Map)
    }

    return next(nil)
  })
  if err != nil {
    return fmt.Errorf("inspect Kong configuration: %w", err)
  }

  r.schema = schema

  var unknown []string
  if err := validateYAMLConfig("", r.config, schema, &unknown); err != nil {
    return fmt.Errorf("%s: %w", r.source, err)
  }

  if len(unknown) > 0 {
    sort.Strings(unknown)
    return fmt.Errorf(
      "%s: unknown configuration keys: %s",
      r.source,
      strings.Join(unknown, ", "),
    )
  }

  return nil
}


// configSchema describes all valid YAML paths derived from Kong flags.
type configSchema struct {
  children map[string]*configSchema
  leaf     bool
  anyMap   bool
}


// newConfigSchema creates an empty schema node with
// an empty set of child elements.
func newConfigSchema() *configSchema {
  return &configSchema{
    children: make(map[string]*configSchema),
  }
}


// add registers a valid configuration path.
func (s *configSchema) add(path []string, anyMap bool) {
  current := s

  for _, part := range path {
    child := current.children[part]
    if child == nil {
      child = newConfigSchema()
      current.children[part] = child
    }
    current = child
  }

  current.leaf = true
  current.anyMap = anyMap
}


// validateYAMLConfig recursively checks YAML keys.
func validateYAMLConfig(
  path string,
  value any,
  schema *configSchema,
  unknown *[]string,
) error {
  if schema.leaf {
    return nil
  }

  values, ok := value.(map[string]any)
  if !ok {
    return fmt.Errorf("expected a mapping at %q", path)
  }

  for key, childValue := range values {
    childSchema, exists := schema.children[key]

    childPath := key
    if path != "" {
      childPath = path + "." + key
    }

    if !exists {
      *unknown = append(*unknown, childPath)
      continue
    }

    if childSchema.anyMap {
      continue
    }

    if err := validateYAMLConfig(
      childPath,
      childValue,
      childSchema,
      unknown,
    ); err != nil {
      return err
    }
  }

  return nil
}


// configPath returns the full YAML path of a Kong flag.
func configPath(parent *kong.Path, flag *kong.Flag) []string {
  var nodes []*kong.Node

  for node := parent.Node(); node != nil; node = node.Parent {
    if node.Type != kong.ApplicationNode {
      nodes = append([]*kong.Node{node}, nodes...)
    }
  }

  return configPathFromNodes(nodes, flag)
}


// configPathFromNodes splits the names of all nodes and the parameter itself
// by dots and combines them into a single path.
func configPathFromNodes(
  nodes []*kong.Node,
  flag *kong.Flag,
) []string {
  var path []string

  for _, node := range nodes {
    path = append(path, splitConfigPath(node.Name)...)
  }

  path = append(path, splitConfigPath(flag.Name)...)

  return path
}


// splitConfigPath treats dots as YAML hierarchy separators.
func splitConfigPath(name string) []string {
  parts := strings.Split(name, ".")
  result := make([]string, 0, len(parts))

  for _, part := range parts {
    if part != "" {
      result = append(result, part)
    }
  }

  return result
}


// lookupYAML retrieves a value by its hierarchical path.
func lookupYAML(
  config map[string]any,
  path []string,
) (any, bool) {
  var current any = config

  for _, part := range path {
    values, ok := current.(map[string]any)
    if !ok {
      return nil, false
    }

    current, ok = values[part]
    if !ok {
      return nil, false
    }
  }

  return current, true
}
