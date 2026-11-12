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
