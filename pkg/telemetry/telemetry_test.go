package telemetry_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"go.uber.org/goleak"
	"google.golang.org/grpc"

	"github.com/zodixdev/infrapilot/pkg/telemetry"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// The runtime instrumentation keeps one background goroutine for the process lifetime by design.
		goleak.IgnoreTopFunction("go.opentelemetry.io/contrib/instrumentation/runtime.Start.func1"),
	)
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
