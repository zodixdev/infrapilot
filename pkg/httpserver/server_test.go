package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/log"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func testConfig() httpserver.Config {
	return httpserver.Config{
		Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second,
	}
}

func startServer(t *testing.T, h http.Handler) (*httpserver.Server, <-chan error) {
	t.Helper()
	s, err := httpserver.New(t.Context(), testConfig(), h)
	require.NoError(t, err)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(t.Context()) }()
	return s, runErr
}

func get(t *testing.T, s *httpserver.Server, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s.Addr().String()+path, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultTransport.RoundTrip(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, body
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	_, err := httpserver.New(t.Context(), testConfig(), nil)
	require.Error(t, err)
	cfg := testConfig()
	cfg.ReadTimeout = 0
	_, err = httpserver.New(t.Context(), cfg, http.NotFoundHandler())
	require.Error(t, err)
	cfg = testConfig()
	cfg.Addr = "256.256.256.256:1"
	_, err = httpserver.New(t.Context(), cfg, http.NotFoundHandler())
	require.Error(t, err)
}

func TestServer_ShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "done")
	})
	s, runErr := startServer(t, mux)

	type result struct {
		code int
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s.Addr().String()+"/slow", http.NoBody)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			res <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		res <- result{code: resp.StatusCode, body: string(b)}
	}()
	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	// Listener is closed promptly: new connections are refused while the in-flight request continues.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", s.Addr().String(), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	}, 2*time.Second, 10*time.Millisecond)

	close(release)
	r := <-res
	require.NoError(t, r.err)
	require.Equal(t, http.StatusOK, r.code)
	require.Equal(t, "done", r.body)
	require.NoError(t, <-shutdownDone)
	require.NoError(t, <-runErr)
}

func TestServer_ShutdownDeadlineForcesClose(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stuck", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // the forced close cancels the request context
		w.WriteHeader(http.StatusOK)
	})
	s, runErr := startServer(t, mux)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s.Addr().String()+"/stuck", http.NoBody)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
	require.NoError(t, <-runErr, "Run returns after the forced close")
}

func TestMiddleware_RecoverWritesProblem(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.New(log.Config{Level: "info"}, &buf, "s", "v", "e")
	require.NoError(t, err)
	h := httpserver.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }),
		httpserver.RequestID(), httpserver.Recover(logger))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var p httpserver.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "internal", p.Code)
	require.Equal(t, "/x", p.Instance)
	require.Contains(t, buf.String(), "kaboom")
	require.Contains(t, buf.String(), "request_id")
	require.NotContains(t, rec.Body.String(), "kaboom", "panic details never reach the client")

	abort := httpserver.Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
		httpserver.Recover(logger))
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", http.NoBody))
	})
}

func TestMiddleware_RequestID(t *testing.T) {
	t.Parallel()
	var seen string
	h := httpserver.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpserver.RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), httpserver.RequestID())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(httpserver.RequestIDHeader, "client-id-1")
	h.ServeHTTP(rec, req)
	require.Equal(t, "client-id-1", seen)
	require.Equal(t, "client-id-1", rec.Header().Get(httpserver.RequestIDHeader))

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(httpserver.RequestIDHeader, "bad id with spaces\n")
	h.ServeHTTP(rec, req)
	require.NotEmpty(t, seen)
	require.NotContains(t, seen, " ")
	require.Equal(t, seen, rec.Header().Get(httpserver.RequestIDHeader))
}

func TestMiddleware_MetricsUseRoutePattern(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.WithoutCancel(t.Context())) })
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.WithoutCancel(t.Context())) })

	mws, err := httpserver.Standard(log.Discard(), tp, mp, "test")
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := httpserver.Chain(mux, mws...)

	for _, id := range []string{"a", "b", "c"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/orgs/"+id, http.NoBody))
		require.Equal(t, http.StatusTeapot, rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http_server_requests_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				route, _ := dp.Attributes.Value(attribute.Key("route"))
				status, _ := dp.Attributes.Value(attribute.Key("status"))
				counts[route.AsString()+" "+status.AsString()] = dp.Value
			}
		}
	}
	require.Equal(t, int64(3), counts["/orgs/{id} 418"], "one series per route pattern, not per URL: %v", counts)
	require.Equal(t, int64(1), counts["unmatched 404"])

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans)
	require.Equal(t, "GET /orgs/{id}", spans[0].Name)
}

func TestMiddleware_AccessLogLevels(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.New(log.Config{Level: "info"}, &buf, "s", "v", "e")
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	h := httpserver.Chain(mux, httpserver.AccessLog(logger))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	require.Empty(t, buf.String(), "health probes are logged at debug")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", http.NoBody))
	require.Contains(t, buf.String(), `"level":"WARN"`)
	require.Contains(t, buf.String(), `"status":502`)
}

func TestAdminHandler(t *testing.T) {
	t.Parallel()
	reg := health.NewRegistry()
	reg.SetReady(true)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "up 1\n") })
	h := httpserver.AdminHandler(reg.LivenessHandler(), reg.ReadinessHandler(), metrics, true)

	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 200, "/debug/pprof/": 200, "/other": 404} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		require.Equal(t, want, rec.Code, path)
	}
	noPprof := httpserver.AdminHandler(reg.LivenessHandler(), reg.ReadinessHandler(), metrics, false)
	rec := httptest.NewRecorder()
	noPprof.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServer_RunEndToEnd(t *testing.T) {
	t.Parallel()
	s, runErr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	code, body := get(t, s, "/")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "ok", string(body))
	require.NoError(t, s.Shutdown(t.Context()))
	require.NoError(t, <-runErr)
	require.NoError(t, s.Shutdown(t.Context()), "idempotent")
	_, err := net.DialTimeout("tcp", s.Addr().String(), 200*time.Millisecond)
	require.Error(t, err, "listener closed")
}
