package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/config"
)

type serviceConfig struct {
	config.Common
	DatabaseURL string `env:"DATABASE_URL"`
	Workers     int    `env:"WORKERS" envDefault:"4"`
}

func (c *serviceConfig) Validate() error {
	if err := c.Common.Validate(); err != nil {
		return err
	}
	if c.Workers < 1 {
		return errRequireWorkers
	}
	return nil
}

var errRequireWorkers = &workersError{}

type workersError struct{}

func (*workersError) Error() string { return "WORKERS must be >= 1" }

func minimalEnv() map[string]string {
	return map[string]string{
		"SERVICE_NAME": "control-plane",
		"DATABASE_URL": "postgres://app@localhost/controlplane",
	}
}

func TestLoad_AppliesDefaultsAndRequired(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[serviceConfig](config.WithEnvironment(minimalEnv()))
	require.NoError(t, err)
	require.Equal(t, "control-plane", cfg.ServiceName)
	require.Equal(t, config.EnvDev, cfg.Environment)
	require.Equal(t, ":8080", cfg.HTTP.Addr)
	require.Equal(t, 25*time.Second, cfg.Shutdown.Timeout)
	require.Equal(t, 4, cfg.Workers)
	require.Equal(t, "postgres://app@localhost/controlplane", cfg.DatabaseURL)
}

func TestLoad_MissingRequiredFails(t *testing.T) {
	t.Parallel()
	_, err := config.Load[serviceConfig](config.WithEnvironment(map[string]string{"SERVICE_NAME": "x"}))
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "DATABASE_URL")
}

func TestLoad_ParseErrorsAreAggregated(t *testing.T) {
	t.Parallel()
	e := minimalEnv()
	e["WORKERS"] = "many"
	e["HTTP_READ_TIMEOUT"] = "soon"
	_, err := config.Load[serviceConfig](config.WithEnvironment(e))
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "WORKERS")
	require.ErrorContains(t, err, "HTTP_READ_TIMEOUT")
}

func TestLoad_ValidateIsCalled(t *testing.T) {
	t.Parallel()
	e := minimalEnv()
	e["WORKERS"] = "0"
	_, err := config.Load[serviceConfig](config.WithEnvironment(e))
	require.ErrorIs(t, err, config.ErrInvalid)
	require.ErrorContains(t, err, "WORKERS must be >= 1")
}

func TestLoad_Prefix(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[serviceConfig](config.WithPrefix("IP_"), config.WithEnvironment(map[string]string{
		"IP_SERVICE_NAME": "gw",
		"IP_DATABASE_URL": "postgres://x",
		"IP_HTTP_ADDR":    ":9999",
	}))
	require.NoError(t, err)
	require.Equal(t, ":9999", cfg.HTTP.Addr)
}

func TestCommon_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad environment", map[string]string{"ENVIRONMENT": "production"}, "ENVIRONMENT"},
		{"bad log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL"},
		{"bad log format", map[string]string{"LOG_FORMAT": "yaml"}, "LOG_FORMAT"},
		{"zero timeout", map[string]string{"HTTP_READ_TIMEOUT": "0s"}, "HTTP_* timeouts"},
		{"negative delay", map[string]string{"SHUTDOWN_DELAY": "-1s"}, "SHUTDOWN_DELAY"},
		{"sampler out of range", map[string]string{"OTEL_TRACES_SAMPLER_ARG": "1.5"}, "OTEL_TRACES_SAMPLER_ARG"},
		{"metrics push without endpoint", map[string]string{"TELEMETRY_METRICS_PUSH": "true"}, "TELEMETRY_METRICS_PUSH"},
		{"insecure otlp in prod", map[string]string{"ENVIRONMENT": "prod", "OTEL_EXPORTER_OTLP_ENDPOINT": "c:4317", "OTEL_EXPORTER_OTLP_INSECURE": "true"}, "OTEL_EXPORTER_OTLP_INSECURE"},
		{"reflection in prod", map[string]string{"ENVIRONMENT": "prod", "GRPC_REFLECTION": "true"}, "GRPC_REFLECTION"},
		{"logs push without endpoint", map[string]string{"TELEMETRY_LOGS_PUSH": "true"}, "TELEMETRY_LOGS_PUSH"},
		{"endpoint with bad scheme", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "grpc://c:4317"}, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{"endpoint without port", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector"}, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{"unlimited grpc streams", map[string]string{"GRPC_MAX_CONCURRENT_STREAMS": "0"}, "GRPC_MAX_CONCURRENT_STREAMS"},
		{"admin on http port", map[string]string{"ADMIN_ADDR": ":8080"}, "must differ"},
		{"half tls", map[string]string{"GRPC_TLS_CERT_FILE": "/tls/tls.crt"}, "set together"},
		{"zero body limit", map[string]string{"HTTP_MAX_BODY_BYTES": "0"}, "HTTP_MAX_BODY_BYTES"},
		{"prod without grpc tls", map[string]string{"ENVIRONMENT": "prod"}, "GRPC_TLS_CERT_FILE is required in prod"},
		{"prod without mtls client ca", map[string]string{"ENVIRONMENT": "prod", "GRPC_TLS_CERT_FILE": "/tls/tls.crt", "GRPC_TLS_KEY_FILE": "/tls/tls.key"}, "GRPC_TLS_CLIENT_CA_FILE is required in prod"},
		{"staging insecure peer identity", map[string]string{"ENVIRONMENT": "staging", "GRPC_INSECURE_PEER_IDENTITY": "spiffe://infrapilot.local/ns/x/sa/y"}, "GRPC_INSECURE_PEER_IDENTITY"},
		{"staging pprof without ack", map[string]string{"ENVIRONMENT": "staging", "ADMIN_PPROF": "true"}, "ADMIN_PPROF_PROD_ACK"},
		{"staging text logs", map[string]string{"ENVIRONMENT": "staging", "LOG_FORMAT": "text"}, "LOG_FORMAT must be json in staging"},
		{"prod debug logs", map[string]string{"ENVIRONMENT": "prod", "LOG_LEVEL": "debug"}, "LOG_LEVEL=debug"},
		{"prod plaintext otlp url", map[string]string{"ENVIRONMENT": "prod", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317"}, "must not use http://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := minimalEnv()
			for k, v := range tt.env {
				e[k] = v
			}
			_, err := config.Load[serviceConfig](config.WithEnvironment(e))
			require.ErrorIs(t, err, config.ErrInvalid)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestCommon_ValidateAggregatesAllProblems(t *testing.T) {
	t.Parallel()
	e := minimalEnv()
	e["LOG_LEVEL"] = "loud"
	e["LOG_FORMAT"] = "xml"
	_, err := config.Load[serviceConfig](config.WithEnvironment(e))
	require.ErrorContains(t, err, "LOG_LEVEL")
	require.ErrorContains(t, err, "LOG_FORMAT")
}

func TestCommon_ValidProductionConfig(t *testing.T) {
	t.Parallel()
	e := minimalEnv()
	for k, v := range map[string]string{
		"ENVIRONMENT": "prod", "GRPC_TLS_CERT_FILE": "/tls/tls.crt", "GRPC_TLS_KEY_FILE": "/tls/tls.key",
		"GRPC_TLS_CLIENT_CA_FILE":     "/tls/ca.crt",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.observability:4317", "TELEMETRY_LOGS_PUSH": "true",
		"ADMIN_PPROF": "true", "ADMIN_PPROF_PROD_ACK": "true",
	} {
		e[k] = v
	}
	cfg, err := config.Load[serviceConfig](config.WithEnvironment(e))
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, cfg.Admin.WriteTimeout)
	require.Equal(t, int64(1<<20), cfg.HTTP.MaxBodyBytes)

	// A service without an internal API needs neither TLS nor a distinct gRPC port.
	e = minimalEnv()
	e["ENVIRONMENT"], e["GRPC_DISABLED"] = "prod", "true"
	_, err = config.Load[serviceConfig](config.WithEnvironment(e))
	require.NoError(t, err)
}

func TestGRPCClient_ValidateFor(t *testing.T) {
	t.Parallel()
	valid := config.GRPCClient{Target: "dns:///control-plane:9090", TLSCAFile: "/tls/ca.crt",
		TLSCertFile: "/tls/tls.crt", TLSKeyFile: "/tls/tls.key", Timeout: time.Second,
		ServerIdentity: "spiffe://infrapilot.local/ns/infrapilot/sa/control-plane"}
	require.NoError(t, valid.ValidateFor(config.EnvProd))
	tests := []struct {
		name string
		mut  func(*config.GRPCClient)
		env  config.Environment
		want string
	}{
		{"empty target", func(c *config.GRPCClient) { c.Target = "" }, config.EnvDev, "TARGET"},
		{"zero timeout", func(c *config.GRPCClient) { c.Timeout = 0 }, config.EnvDev, "TIMEOUT"},
		{"tls without ca", func(c *config.GRPCClient) { c.TLSCAFile = "" }, config.EnvDev, "TLS_CA_FILE"},
		{"half client cert", func(c *config.GRPCClient) { c.TLSKeyFile = "" }, config.EnvDev, "set together"},
		{"insecure mixed with tls", func(c *config.GRPCClient) { c.Insecure = true }, config.EnvDev, "cannot be combined"},
		{"insecure in prod", func(c *config.GRPCClient) {
			*c = config.GRPCClient{Target: "x:1", Timeout: time.Second, Insecure: true}
		}, config.EnvProd, "INSECURE is not allowed in prod"},
		{"no server identity in prod", func(c *config.GRPCClient) { c.ServerIdentity = "" }, config.EnvProd, "SERVER_IDENTITY is required in prod"},
		{"non-canonical server identity", func(c *config.GRPCClient) { c.ServerIdentity = "spiffe://infrapilot.local/ns/a/sa/%62" }, config.EnvDev, "SERVER_IDENTITY"},
		{"identity with insecure", func(c *config.GRPCClient) {
			*c = config.GRPCClient{Target: "x:1", Timeout: time.Second, Insecure: true, ServerIdentity: "spiffe://infrapilot.local/ns/a/sa/b"}
		}, config.EnvDev, "cannot be verified with INSECURE"},
		{"no client cert in staging", func(c *config.GRPCClient) { c.TLSCertFile, c.TLSKeyFile = "", "" }, config.EnvStaging, "TLS_CERT_FILE is required in staging"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid
			tt.mut(&c)
			require.ErrorContains(t, c.ValidateFor(tt.env), tt.want)
		})
	}
	insecureDev := config.GRPCClient{Target: "localhost:9090", Timeout: time.Second, Insecure: true}
	require.NoError(t, insecureDev.ValidateFor(config.EnvDev))
}
