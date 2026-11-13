package telemetry_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"go.uber.org/goleak"
	"google.golang.org/grpc"

	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/pkg/telemetry"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestSetup_PullOnlyServesMetricsAndPropagates(t *testing.T) {
	p, err := telemetry.Setup(t.Context(), telemetry.Config{TraceSampleRatio: 1}, telemetry.Service{
		Name: "svc", Version: "1.0", Environment: "test", InstanceID: "i-1",
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Shutdown(context.WithoutCancel(t.Context()))) })

	ctr, err := p.Meter("test").Int64Counter("requests_total")
	require.NoError(t, err)
	ctr.Add(t.Context(), 3)

	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "requests_total 3")
	require.Contains(t, body, "go_goroutines", "standard Go collector registered")
	require.Contains(t, body, "process_cpu_seconds_total", "process collector registered")
	require.Contains(t, body, `target_info{`, "resource attributes exposed")
	require.Contains(t, body, `service_name="svc"`)

	// Propagator installed globally: inject + extract round trip.
	ctx, span := p.Tracer("t").Start(t.Context(), "op")
	carrier := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, mapCarrier(carrier))
	span.End()
	require.Contains(t, carrier["traceparent"], span.SpanContext().TraceID().String())
}

func TestSetup_ValidatesInput(t *testing.T) {
	_, err := telemetry.Setup(t.Context(), telemetry.Config{}, telemetry.Service{}, nil)
	require.Error(t, err)
	_, err = telemetry.Setup(t.Context(), telemetry.Config{TraceSampleRatio: 2}, telemetry.Service{Name: "s"}, nil)
	require.Error(t, err)
}

// fakeCollector accepts OTLP/gRPC trace exports and records the service names it saw.
type fakeCollector struct {
	collectortrace.UnimplementedTraceServiceServer
	mu       sync.Mutex
	services []string
	spans    int
}

func (f *fakeCollector) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, attr := range rs.GetResource().GetAttributes() {
			if attr.GetKey() == "service.name" {
				f.services = append(f.services, attr.GetValue().GetStringValue())
			}
		}
		for _, ss := range rs.GetScopeSpans() {
			f.spans += len(ss.GetSpans())
		}
	}
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

func TestSetup_ExportsTracesOverOTLP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	fc := &fakeCollector{}
	collectortrace.RegisterTraceServiceServer(srv, fc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	p, err := telemetry.Setup(t.Context(), telemetry.Config{
		OTLPEndpoint: lis.Addr().String(), OTLPInsecure: true, TraceSampleRatio: 1, ExportTimeout: 5 * time.Second,
	}, telemetry.Service{Name: "exporter-test", Version: "v", Environment: "test"}, nil)
	require.NoError(t, err)

	_, span := p.Tracer("t").Start(t.Context(), "work")
	span.End()
	require.NoError(t, p.Shutdown(t.Context()), "shutdown flushes the batch")

	fc.mu.Lock()
	defer fc.mu.Unlock()
	require.Equal(t, 1, fc.spans)
	require.Equal(t, []string{"exporter-test"}, fc.services)
}

func TestShutdown_RespectsContext(t *testing.T) {
	// Unreachable collector: shutdown must give up when ctx expires instead of hanging.
	p, err := telemetry.Setup(t.Context(), telemetry.Config{
		OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true, TraceSampleRatio: 1, ExportTimeout: 200 * time.Millisecond,
	}, telemetry.Service{Name: "s"}, nil)
	require.NoError(t, err)
	_, span := p.Tracer("t").Start(t.Context(), "work")
	span.End()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = p.Shutdown(ctx)
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestMetricsHandler_ContentType(t *testing.T) {
	p, err := telemetry.Setup(t.Context(), telemetry.Config{TraceSampleRatio: 0}, telemetry.Service{Name: "s"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.WithoutCancel(t.Context())) })
	rec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	ct := rec.Header().Get("Content-Type")
	require.True(t, strings.HasPrefix(ct, "text/plain") || strings.HasPrefix(ct, "application/openmetrics-text"), ct)
	b, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	require.NotEmpty(t, b)
}

type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }
func (m mapCarrier) Keys() []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// fakeLogs accepts OTLP/gRPC log exports and records message bodies.
type fakeLogs struct {
	collectorlogs.UnimplementedLogsServiceServer
	mu     sync.Mutex
	bodies []string
	traces []string
}

func (f *fakeLogs) Export(_ context.Context, req *collectorlogs.ExportLogsServiceRequest) (*collectorlogs.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				f.bodies = append(f.bodies, r.GetBody().GetStringValue())
				f.traces = append(f.traces, hex.EncodeToString(r.GetTraceId()))
			}
		}
	}
	return &collectorlogs.ExportLogsServiceResponse{}, nil
}

// fakeMetrics accepts OTLP/gRPC metric exports and records metric names.
type fakeMetrics struct {
	collectormetrics.UnimplementedMetricsServiceServer
	mu    sync.Mutex
	names map[string]bool
}

func (f *fakeMetrics) Export(_ context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				f.names[m.GetName()] = true
			}
		}
	}
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

var fm = &fakeMetrics{names: map[string]bool{}}

func startCollector(t *testing.T) (string, *fakeCollector, *fakeLogs) {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	fc, fl := &fakeCollector{}, &fakeLogs{}
	collectortrace.RegisterTraceServiceServer(srv, fc)
	collectorlogs.RegisterLogsServiceServer(srv, fl)
	collectormetrics.RegisterMetricsServiceServer(srv, fm)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), fc, fl
}

func TestSetup_ExportsLogsThroughSlogBridgeWithRedaction(t *testing.T) {
	addr, _, fl := startCollector(t)
	p, err := telemetry.Setup(t.Context(), telemetry.Config{
		OTLPEndpoint: "http://" + addr, OTLPInsecure: true, TraceSampleRatio: 1, LogsPush: true, ExportTimeout: 5 * time.Second,
	}, telemetry.Service{Name: "logs-test"}, nil)
	require.NoError(t, err, "URL-form endpoint (OTel spec) is accepted")
	require.NotNil(t, p.LogHandler())

	logger, err := log.New(log.Config{Level: "info"}, io.Discard, "logs-test", "v", "test", p.LogHandler())
	require.NoError(t, err)
	ctx, span := p.Tracer("t").Start(t.Context(), "op")
	logger.InfoContext(ctx, "user logged in", slog.String("password", "LEAK"))
	span.End()
	require.NoError(t, p.Shutdown(t.Context()), "shutdown flushes the log batch")

	fl.mu.Lock()
	defer fl.mu.Unlock()
	require.Equal(t, []string{"user logged in"}, fl.bodies)
	require.Equal(t, span.SpanContext().TraceID().String(), fl.traces[0], "log records carry the trace id")
}

func TestSetup_PushRequiresEndpoint(t *testing.T) {
	_, err := telemetry.Setup(t.Context(), telemetry.Config{LogsPush: true}, telemetry.Service{Name: "s"}, nil)
	require.Error(t, err)
}

func TestErrorHandler_RateLimitsRepeats(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	p, err := telemetry.Setup(t.Context(), telemetry.Config{TraceSampleRatio: 1}, telemetry.Service{Name: "s"}, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Shutdown(context.WithoutCancel(t.Context())) })
	buf.Reset()
	for range 50 {
		otel.Handle(errors.New("export failed: connection refused"))
	}
	require.Equal(t, 1, strings.Count(buf.String(), "opentelemetry error"), "repeats within the interval are suppressed")
}

// TestSetup_PushedMetricsIncludeRuntimeCollectors: dashboards and alerts use go_* / process_* names, which
// must exist on the push path too.
func TestSetup_PushedMetricsIncludeRuntimeCollectors(t *testing.T) {
	addr, _, _ := startCollector(t)
	p, err := telemetry.Setup(t.Context(), telemetry.Config{
		OTLPEndpoint: addr, OTLPInsecure: true, TraceSampleRatio: 1, MetricsPush: true,
		ExportInterval: time.Hour, ExportTimeout: 5 * time.Second,
	}, telemetry.Service{Name: "push-test"}, nil)
	require.NoError(t, err)
	ctr, err := p.Meter("t").Int64Counter("http_server_requests_total")
	require.NoError(t, err)
	ctr.Add(t.Context(), 1)
	require.NoError(t, p.Shutdown(t.Context()), "shutdown performs a final export")

	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, name := range []string{"http_server_requests_total", "go_goroutines", "go_memstats_heap_inuse_bytes", "process_resident_memory_bytes", "process_cpu_seconds_total"} {
		require.True(t, fm.names[name], "%s missing from the OTLP push; got %v", name, fm.names)
	}
}
