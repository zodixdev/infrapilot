// Package telemetry bootstraps OpenTelemetry for a service (ADR-0012, ADR-0019): resource
// attributes, W3C propagation, a tracer provider exporting OTLP/gRPC when an endpoint is
// configured, and a meter provider that always serves Prometheus exposition on /metrics and
// optionally pushes OTLP. Shutdown flushes everything in reverse order.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Config mirrors config.Telemetry without importing it (pkg packages stay independent).
type Config struct {
	// OTLPEndpoint is host:port of the collector's OTLP/gRPC receiver. Empty disables push export.
	OTLPEndpoint string
	// OTLPInsecure disables TLS towards the collector (local development only).
	OTLPInsecure bool
	// TraceSampleRatio is the parent-based head sampling ratio in [0, 1].
	TraceSampleRatio float64
	// MetricsPush also exports metrics over OTLP (pull on /metrics is always on).
	MetricsPush bool
	// ExportInterval for pushed metrics.
	ExportInterval time.Duration
	// ExportTimeout for OTLP exports.
	ExportTimeout time.Duration
}

// Service identity attached to every span, metric and log.
type Service struct {
	Name        string
	Version     string
	Environment string
	// InstanceID defaults to the hostname (pod name on Kubernetes).
	InstanceID string
}

// Provider owns the SDK objects created by Setup.
type Provider struct {
	tracer   *sdktrace.TracerProvider
	meter    *sdkmetric.MeterProvider
	registry *prometheus.Registry
	res      *resource.Resource
	log      *slog.Logger
}

// Setup initializes global OpenTelemetry providers and propagators. The caller must call Shutdown.
func Setup(ctx context.Context, cfg Config, svc Service, logger *slog.Logger) (*Provider, error) {
	if svc.Name == "" {
		return nil, errors.New("telemetry: service name is required")
	}
	if cfg.TraceSampleRatio < 0 || cfg.TraceSampleRatio > 1 {
		return nil, fmt.Errorf("telemetry: sample ratio %v out of [0,1]", cfg.TraceSampleRatio)
	}
	if cfg.ExportInterval <= 0 {
		cfg.ExportInterval = 15 * time.Second
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 10 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if svc.InstanceID == "" {
		svc.InstanceID, _ = os.Hostname()
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(svc.Name),
		semconv.ServiceVersion(svc.Version),
		semconv.ServiceInstanceID(svc.InstanceID),
		semconv.DeploymentEnvironmentNameKey.String(svc.Environment),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	p := &Provider{res: res, log: logger}

	otel.SetErrorHandler(errorHandler(logger)) //nolint:contextcheck // otel.ErrorHandler callbacks receive no context
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if err := p.setupTraces(ctx, cfg); err != nil {
		return nil, err
	}
	if err := p.setupMetrics(ctx, cfg); err != nil {
		_ = p.tracer.Shutdown(ctx)
		return nil, err
	}
	otel.SetTracerProvider(p.tracer)
	otel.SetMeterProvider(p.meter)

	if err := runtime.Start(runtime.WithMeterProvider(p.meter), runtime.WithMinimumReadMemStatsInterval(10*time.Second)); err != nil {
		_ = p.Shutdown(ctx)
		return nil, fmt.Errorf("telemetry: start runtime metrics: %w", err)
	}
	logger.InfoContext(ctx, "telemetry initialized",
		slog.String("otlp_endpoint", cfg.OTLPEndpoint),
		slog.Bool("metrics_push", cfg.MetricsPush && cfg.OTLPEndpoint != ""),
		slog.Float64("trace_sample_ratio", cfg.TraceSampleRatio))
	return p, nil
}

func (p *Provider) setupTraces(ctx context.Context, cfg Config) error {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(p.res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio))),
	}
	if cfg.OTLPEndpoint != "" {
		expOpts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithTimeout(cfg.ExportTimeout),
		}
		if cfg.OTLPInsecure {
			expOpts = append(expOpts, otlptracegrpc.WithInsecure())
		}
		exp, err := otlptracegrpc.New(ctx, expOpts...)
		if err != nil {
			return fmt.Errorf("telemetry: create trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithExportTimeout(cfg.ExportTimeout)))
	}
	p.tracer = sdktrace.NewTracerProvider(opts...)
	return nil
}

func (p *Provider) setupMetrics(ctx context.Context, cfg Config) error {
	p.registry = prometheus.NewRegistry()
	// Standard process/Go collectors keep the base "Go service" dashboard working for every service.
	p.registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	promExp, err := otelprom.New(otelprom.WithRegisterer(p.registry), otelprom.WithoutScopeInfo())
	if err != nil {
		return fmt.Errorf("telemetry: create prometheus exporter: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(p.res), sdkmetric.WithReader(promExp)}

	if cfg.MetricsPush && cfg.OTLPEndpoint != "" {
		expOpts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetricgrpc.WithTimeout(cfg.ExportTimeout),
		}
		if cfg.OTLPInsecure {
			expOpts = append(expOpts, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, expOpts...)
		if err != nil {
			return fmt.Errorf("telemetry: create metric exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(cfg.ExportInterval), sdkmetric.WithTimeout(cfg.ExportTimeout))))
	}
	p.meter = sdkmetric.NewMeterProvider(opts...)
	return nil
}

// Tracer returns a named tracer from this provider.
func (p *Provider) Tracer(name string) trace.Tracer { return p.tracer.Tracer(name) }

// Meter returns a named meter from this provider.
func (p *Provider) Meter(name string) metric.Meter { return p.meter.Meter(name) }

// TracerProvider exposes the SDK provider for instrumentation libraries.
func (p *Provider) TracerProvider() trace.TracerProvider { return p.tracer }

// MeterProvider exposes the SDK provider for instrumentation libraries.
func (p *Provider) MeterProvider() metric.MeterProvider { return p.meter }

// Propagator returns the global composite propagator configured by Setup.
func (p *Provider) Propagator() propagation.TextMapPropagator { return otel.GetTextMapPropagator() }

// MetricsHandler serves Prometheus exposition for this service's registry.
func (p *Provider) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	})
}

// Registry exposes the Prometheus registry for tests and additional collectors.
func (p *Provider) Registry() *prometheus.Registry { return p.registry }

// Resource returns the resource attributes.
func (p *Provider) Resource() []attribute.KeyValue { return p.res.Attributes() }

// Shutdown flushes pending telemetry and stops exporters: meter first (final scrape of counters),
// then tracer. Errors are joined; ctx bounds the whole operation.
func (p *Provider) Shutdown(ctx context.Context) error {
	var errs []error
	if p.meter != nil {
		if err := p.meter.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider: %w", err))
		}
	}
	if p.tracer != nil {
		if err := p.tracer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider: %w", err))
		}
	}
	return errors.Join(errs...)
}

// errorHandler routes SDK/exporter errors (which carry no request context) to the service logger.
func errorHandler(logger *slog.Logger) otel.ErrorHandler {
	return otel.ErrorHandlerFunc(func(err error) {
		logger.LogAttrs(context.Background(), slog.LevelWarn, "opentelemetry error", slog.Any("error", err))
	})
}
