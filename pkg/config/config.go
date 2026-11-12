// Package config loads typed configuration from environment variables and fails fast on
// missing or invalid values (ADR-0016: env vars → typed struct, validated at startup).
package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Validator is implemented by configuration structs that have cross-field rules.
type Validator interface {
	Validate() error
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid configuration")

// Option customizes Load.
type Option func(*options)

type options struct {
	prefix      string
	environment map[string]string
}

// WithPrefix prepends prefix to every env tag (e.g. "INFRAPILOT_").
func WithPrefix(prefix string) Option { return func(o *options) { o.prefix = prefix } }

// WithEnvironment reads from the given map instead of the process environment. For tests.
func WithEnvironment(environment map[string]string) Option {
	return func(o *options) { o.environment = environment }
}

// Load parses T from the environment. Every field without an `envDefault` is required unless it is
// tagged `env:"NAME,notRequired"` or lives in a nested struct that is itself optional; parsing and
// validation errors are aggregated so the operator sees all problems at once.
func Load[T any](opts ...Option) (T, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg, err := env.ParseAsWithOptions[T](env.Options{
		Prefix:                o.prefix,
		Environment:           o.environment,
		RequiredIfNoDef:       true,
		UseFieldNameByDefault: false,
	})
	if err != nil {
		return cfg, fmt.Errorf("%w: %w", ErrInvalid, withEnvKeys[T](o.prefix, err))
	}
	if v, ok := any(&cfg).(Validator); ok {
		if err := v.Validate(); err != nil {
			return cfg, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return cfg, nil
}

// Environment is the deployment environment of a service.
type Environment string

// Deployment environments. Environment-specific behavior must be limited to defaults and safety rails
// (e.g. refusing insecure exporters in production).
const (
	EnvDev     Environment = "dev"
	EnvTest    Environment = "test"
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

// Common holds the configuration every InfraPilot Go service shares. Services embed it:
//
//	type Config struct {
//	    config.Common
//	    DatabaseURL string `env:"DATABASE_URL"`
//	}
type Common struct {
	ServiceName string      `env:"SERVICE_NAME"`
	Environment Environment `env:"ENVIRONMENT" envDefault:"dev"`
	// Version is injected by the build (ldflags → env in the container, or set by main).
	Version string `env:"SERVICE_VERSION" envDefault:"dev"`

	Log       Log      `envPrefix:"LOG_"`
	HTTP      HTTP     `envPrefix:"HTTP_"`
	Admin     Admin    `envPrefix:"ADMIN_"`
	GRPC      GRPC     `envPrefix:"GRPC_"`
	Shutdown  Shutdown `envPrefix:"SHUTDOWN_"`
	Telemetry Telemetry
}

// Log configures the slog logger.
type Log struct {
	Level     string `env:"LEVEL" envDefault:"info"`
	Format    string `env:"FORMAT" envDefault:"json"`
	AddSource bool   `env:"ADD_SOURCE" envDefault:"false"`
}

// HTTP configures the public/admin HTTP listener.
type HTTP struct {
	Addr              string        `env:"ADDR" envDefault:":8080"`
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`
	ReadTimeout       time.Duration `env:"READ_TIMEOUT" envDefault:"30s"`
	WriteTimeout      time.Duration `env:"WRITE_TIMEOUT" envDefault:"30s"`
	IdleTimeout       time.Duration `env:"IDLE_TIMEOUT" envDefault:"120s"`
	MaxHeaderBytes    int           `env:"MAX_HEADER_BYTES" envDefault:"1048576"`
}

// GRPC configures the internal gRPC listener.
type GRPC struct {
	Addr                 string        `env:"ADDR" envDefault:":9090"`
	MaxRecvMsgBytes      int           `env:"MAX_RECV_MSG_BYTES" envDefault:"4194304"`
	ConnectionTimeout    time.Duration `env:"CONNECTION_TIMEOUT" envDefault:"10s"`
	KeepaliveTime        time.Duration `env:"KEEPALIVE_TIME" envDefault:"30s"`
	KeepaliveTimeout     time.Duration `env:"KEEPALIVE_TIMEOUT" envDefault:"10s"`
	MaxConnectionAge     time.Duration `env:"MAX_CONNECTION_AGE" envDefault:"30m"`
	EnableReflection     bool          `env:"REFLECTION" envDefault:"false"`
	MaxConcurrentStreams uint32        `env:"MAX_CONCURRENT_STREAMS" envDefault:"1024"`
}

// Shutdown controls graceful termination (AGENTS.md §5).
type Shutdown struct {
	// Delay keeps the process serving after readiness turns false so load balancers drain the endpoint
	// (should be ≥ the readiness probe period). Zero in tests.
	Delay time.Duration `env:"DELAY" envDefault:"5s"`
	// Timeout bounds the drain of in-flight work; afterwards servers are stopped hard.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"25s"`
}

// Telemetry configures OpenTelemetry export (ADR-0012, ADR-0019). Names that exist in the OpenTelemetry
// environment specification keep their standard spelling and semantics; InfraPilot-specific settings use
// the TELEMETRY_ prefix so the SDK never parses them with different units.
type Telemetry struct {
	// ExporterOTLPEndpoint is host:port of the collector's OTLP/gRPC receiver; empty disables push export.
	ExporterOTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
	ExporterOTLPInsecure bool   `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"false"`
	// TracesSamplerArg is the parent-based trace id ratio (0..1).
	TracesSamplerArg float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"1.0"`
	// MetricsPush additionally pushes metrics over OTLP (pull on /metrics is always on).
	MetricsPush    bool          `env:"TELEMETRY_METRICS_PUSH" envDefault:"false"`
	ExportInterval time.Duration `env:"TELEMETRY_METRIC_EXPORT_INTERVAL" envDefault:"15s"`
	ExportTimeout  time.Duration `env:"TELEMETRY_EXPORT_TIMEOUT" envDefault:"10s"`
}

// Validate implements Validator.
func (c *Common) Validate() error {
	var errs []error
	if strings.TrimSpace(c.ServiceName) == "" {
		errs = append(errs, errors.New("SERVICE_NAME must not be empty"))
	}
	switch c.Environment {
	case EnvDev, EnvTest, EnvStaging, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("ENVIRONMENT %q must be one of dev, test, staging, prod", c.Environment))
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL %q must be one of debug, info, warn, error", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT %q must be json or text", c.Log.Format))
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ReadTimeout <= 0 || c.HTTP.WriteTimeout <= 0 || c.HTTP.IdleTimeout <= 0 {
		errs = append(errs, errors.New("HTTP_* timeouts must be positive"))
	}
	if c.Shutdown.Timeout <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT must be positive"))
	}
	if c.Shutdown.Delay < 0 {
		errs = append(errs, errors.New("SHUTDOWN_DELAY must not be negative"))
	}
	if c.Telemetry.TracesSamplerArg < 0 || c.Telemetry.TracesSamplerArg > 1 {
		errs = append(errs, errors.New("OTEL_TRACES_SAMPLER_ARG must be within [0, 1]"))
	}
	if c.Telemetry.MetricsPush && c.Telemetry.ExporterOTLPEndpoint == "" {
		errs = append(errs, errors.New("TELEMETRY_METRICS_PUSH requires OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if c.Environment == EnvProd && c.Telemetry.ExporterOTLPEndpoint != "" && c.Telemetry.ExporterOTLPInsecure {
		errs = append(errs, errors.New("OTEL_EXPORTER_OTLP_INSECURE is not allowed in prod"))
	}
	if c.Environment == EnvProd && c.GRPC.EnableReflection {
		errs = append(errs, errors.New("GRPC_REFLECTION is not allowed in prod"))
	}
	return errors.Join(errs...)
}

// withEnvKeys rewrites parse errors so they name the environment variable (what an operator sets)
// instead of the Go field. Other error kinds already carry the variable name.
func withEnvKeys[T any](prefix string, err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	var zero T
	keys := map[string][]string{}
	collectKeys(reflect.TypeOf(zero), prefix, keys)
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) {
			if ks := keys[pe.Name]; len(ks) > 0 {
				out = append(out, fmt.Errorf("%s: %w", strings.Join(ks, "|"), pe.Err))
				continue
			}
		}
		out = append(out, e)
	}
	return errors.Join(out...)
}

// collectKeys maps struct field names to the env keys they bind to, following env/envPrefix tags
// through nested and embedded structs.
func collectKeys(t reflect.Type, prefix string, out map[string][]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if tag, ok := f.Tag.Lookup("env"); ok {
			name, _, _ := strings.Cut(tag, ",")
			if name != "" {
				out[f.Name] = append(out[f.Name], prefix+name)
				continue
			}
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}) {
			collectKeys(ft, prefix+f.Tag.Get("envPrefix"), out)
		}
	}
}

// Admin configures the operations listener (/healthz, /readyz, /metrics, optional pprof). It is bound
// to a separate port so that it is never exposed through the public ingress.
type Admin struct {
	Addr  string `env:"ADDR" envDefault:":8081"`
	Pprof bool   `env:"PPROF" envDefault:"false"`
}
