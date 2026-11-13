// Package telemetry bootstraps OpenTelemetry for a service (ADR-0012, ADR-0019): resource attributes,
// W3C propagation, a tracer provider exporting OTLP/gRPC, a meter provider that always serves Prometheus
// exposition on /metrics and optionally pushes OTLP, and an optional OTLP log provider that the slog
// bridge writes to. Shutdown flushes everything, traces first, each provider with its own share of the
// deadline.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Config mirrors config.Telemetry without importing it (pkg packages stay independent).
type Config struct {
	// OTLPEndpoint of the collector's OTLP/gRPC receiver: host:port, or a URL (http://host:4317) as the
	// OpenTelemetry specification defines it. Empty disables push export.
	OTLPEndpoint string
	// OTLPInsecure disables TLS towards the collector (local development only).
	OTLPInsecure bool
	// TraceSampleRatio is the parent-based head sampling ratio in [0, 1].
	TraceSampleRatio float64
	// MetricsPush also exports metrics over OTLP (pull on /metrics is always on).
	MetricsPush bool
	// LogsPush exports logs over OTLP through the slog bridge (stdout JSON is always on).
	LogsPush bool
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
	logger   *sdklog.LoggerProvider // nil unless LogsPush
	registry *prometheus.Registry   // OTel instruments (via the Prometheus exporter)
	runtime  *prometheus.Registry   // Go runtime + process collectors
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
	if (cfg.MetricsPush || cfg.LogsPush) && cfg.OTLPEndpoint == "" {
		return nil, errors.New("telemetry: metrics/logs push requires an OTLP endpoint")
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

	// resource.New (unlike Merge with resource.Default) never fails on schema-URL mismatches after an SDK bump.
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
		resource.WithAttributes(
			semconv.ServiceName(svc.Name),
			semconv.ServiceVersion(svc.Version),
			semconv.ServiceInstanceID(svc.InstanceID),
			semconv.DeploymentEnvironmentNameKey.String(svc.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	p := &Provider{res: res, log: logger}
	otel.SetErrorHandler(newErrorHandler(logger, time.Minute))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if err := p.setupTraces(ctx, cfg); err != nil {
		return nil, err
	}
	if err := p.setupMetrics(ctx, cfg); err != nil {
		_ = p.tracer.Shutdown(ctx)
		return nil, err
	}
	if cfg.LogsPush {
		if err := p.setupLogs(ctx, cfg); err != nil {
			_ = p.Shutdown(ctx)
			return nil, err
		}
	}
	otel.SetTracerProvider(p.tracer)
	otel.SetMeterProvider(p.meter)

	logger.InfoContext(ctx, "telemetry initialized",
		slog.String("otlp_endpoint", cfg.OTLPEndpoint),
		slog.Bool("metrics_push", cfg.MetricsPush),
		slog.Bool("logs_push", cfg.LogsPush),
		slog.Float64("trace_sample_ratio", cfg.TraceSampleRatio))
	return p, nil
}

// endpointIsURL reports whether the endpoint carries a scheme (OTel spec form) rather than host:port.
func endpointIsURL(e string) bool { return strings.Contains(e, "://") }

func (p *Provider) setupTraces(ctx context.Context, cfg Config) error {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(p.res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio))),
	}
	if cfg.OTLPEndpoint != "" {
		expOpts := []otlptracegrpc.Option{otlptracegrpc.WithTimeout(cfg.ExportTimeout)}
		if endpointIsURL(cfg.OTLPEndpoint) {
			expOpts = append(expOpts, otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint))
		} else {
			expOpts = append(expOpts, otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint))
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
	// Runtime metrics come from the standard Go and process collectors, in their own registry. They are served
	// on /metrics (pull) and bridged into the OTLP push below, so dashboards and alerts see the same names
	// (go_goroutines, process_resident_memory_bytes, ...) whichever path an environment uses.
	p.runtime = prometheus.NewRegistry()
	p.runtime.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	promExp, err := otelprom.New(otelprom.WithRegisterer(p.registry), otelprom.WithoutScopeInfo())
	if err != nil {
		return fmt.Errorf("telemetry: create prometheus exporter: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(p.res), sdkmetric.WithReader(promExp)}
	if cfg.MetricsPush {
		expOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithTimeout(cfg.ExportTimeout)}
		if endpointIsURL(cfg.OTLPEndpoint) {
			expOpts = append(expOpts, otlpmetricgrpc.WithEndpointURL(cfg.OTLPEndpoint))
		} else {
			expOpts = append(expOpts, otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint))
		}
		if cfg.OTLPInsecure {
			expOpts = append(expOpts, otlpmetricgrpc.WithInsecure())
		}
		exp, err := otlpmetricgrpc.New(ctx, expOpts...)
		if err != nil {
			return fmt.Errorf("telemetry: create metric exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(cfg.ExportInterval), sdkmetric.WithTimeout(cfg.ExportTimeout),
			sdkmetric.WithProducer(prombridge.NewMetricProducer(prombridge.WithGatherer(p.runtime))))))
	}
	p.meter = sdkmetric.NewMeterProvider(opts...)
	return nil
}

func (p *Provider) setupLogs(ctx context.Context, cfg Config) error {
	expOpts := []otlploggrpc.Option{otlploggrpc.WithTimeout(cfg.ExportTimeout)}
	if endpointIsURL(cfg.OTLPEndpoint) {
		expOpts = append(expOpts, otlploggrpc.WithEndpointURL(cfg.OTLPEndpoint))
	} else {
		expOpts = append(expOpts, otlploggrpc.WithEndpoint(cfg.OTLPEndpoint))
	}
	if cfg.OTLPInsecure {
		expOpts = append(expOpts, otlploggrpc.WithInsecure())
	}
	exp, err := otlploggrpc.New(ctx, expOpts...)
	if err != nil {
		return fmt.Errorf("telemetry: create log exporter: %w", err)
	}
	// The batch processor bounds memory (queue size) and drops on overflow rather than blocking callers.
	p.logger = sdklog.NewLoggerProvider(sdklog.WithResource(p.res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp, sdklog.WithExportTimeout(cfg.ExportTimeout))))
	return nil
}

// LogHandler returns the slog handler that exports records over OTLP, or nil when log push is off.
// Pass it to log.New as an extra handler: redaction runs before it.
func (p *Provider) LogHandler() slog.Handler {
	if p.logger == nil {
		return nil
	}
	return otelslog.NewHandler("github.com/zodixdev/infrapilot", otelslog.WithLoggerProvider(p.logger))
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
	return promhttp.HandlerFor(prometheus.Gatherers{p.registry, p.runtime}, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	})
}

// Registry exposes the Prometheus registry for tests and additional collectors.
func (p *Provider) Registry() *prometheus.Registry { return p.registry }

// Resource returns the resource attributes.
func (p *Provider) Resource() []attribute.KeyValue { return p.res.Attributes() }

// Shutdown flushes pending telemetry and stops exporters. Traces go first (they explain a slow shutdown and
// are the costliest loss), then logs, then metrics. When ctx has a deadline each provider gets an equal
// share of what remains, so one hung exporter cannot starve the others. Errors are joined.
func (p *Provider) Shutdown(ctx context.Context) error {
	type step struct {
		name string
		fn   func(context.Context) error
	}
	var steps []step
	if p.tracer != nil {
		steps = append(steps, step{"tracer provider", p.tracer.Shutdown})
	}
	if p.logger != nil {
		steps = append(steps, step{"logger provider", p.logger.Shutdown})
	}
	if p.meter != nil {
		steps = append(steps, step{"meter provider", p.meter.Shutdown})
	}
	var errs []error
	for i, s := range steps {
		stepCtx, cancel := ctx, context.CancelFunc(func() {})
		if dl, ok := ctx.Deadline(); ok {
			share := time.Until(dl) / time.Duration(len(steps)-i)
			stepCtx, cancel = context.WithTimeout(ctx, share)
		}
		if err := s.fn(stepCtx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		}
		cancel()
	}
	return errors.Join(errs...)
}

// errorHandler routes SDK/exporter errors (which carry no request context) to the service logger, at most
// once per interval per message so a collector outage does not flood the logs; suppressed repeats are
// counted and reported with the next line.
type errorHandler struct {
	log      *slog.Logger
	interval time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
	dropped  map[string]int
}

func newErrorHandler(logger *slog.Logger, interval time.Duration) *errorHandler {
	return &errorHandler{log: logger, interval: interval, last: map[string]time.Time{}, dropped: map[string]int{}}
}

func (h *errorHandler) Handle(err error) {
	msg := err.Error()
	h.mu.Lock()
	now := time.Now()
	if t, ok := h.last[msg]; ok && now.Sub(t) < h.interval {
		h.dropped[msg]++
		h.mu.Unlock()
		return
	}
	suppressed := h.dropped[msg]
	h.last[msg] = now
	delete(h.dropped, msg)
	if len(h.last) > 256 { // bound memory: forget everything older than the interval
		for k, t := range h.last {
			if now.Sub(t) >= h.interval {
				delete(h.last, k)
			}
		}
	}
	h.mu.Unlock()
	h.log.LogAttrs(context.Background(), slog.LevelWarn, "opentelemetry error",
		slog.Any("error", err), slog.Int("suppressed_repeats", suppressed))
}
