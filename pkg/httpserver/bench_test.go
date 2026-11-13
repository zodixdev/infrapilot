package httpserver_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/log"
)

// BenchmarkStandardStack measures the per-request overhead of the full middleware stack (request id, span,
// RED metrics, access log, body limit, recovery) around a trivial handler.
func BenchmarkStandardStack(b *testing.B) {
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	logger, err := log.New(log.Config{Level: "info"}, io.Discard, "svc", "v", "prod")
	if err != nil {
		b.Fatal(err)
	}
	mws, err := httpserver.Standard(httpserver.Options{Logger: logger, TracerProvider: tp, MeterProvider: mp, ServerName: "b", MaxBodyBytes: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := httpserver.Chain(mux, mws...)
	req := httptest.NewRequest(http.MethodGet, "/v1/orgs/42", http.NoBody)
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	_ = slog.Default()
}
