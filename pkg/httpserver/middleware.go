package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/zodixdev/infrapilot/pkg/log"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first in the list is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestIDHeader carries the request id in and out.
const RequestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// RequestIDFromContext returns the request id set by RequestID.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID reuses a well-formed inbound X-Request-Id (≤ 128 printable chars) or generates a UUIDv7,
// echoes it in the response and attaches it to the log context.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				u, err := uuid.NewV7()
				if err != nil {
					u = uuid.New()
				}
				id = u.String()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			ctx = log.ContextWithAttrs(ctx, slog.String("request_id", id))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// Recover converts panics into a 500 problem+json response and logs the stack. ErrAbortHandler is
// re-panicked as net/http expects.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && err == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rec)
				}
				logger.ErrorContext(ctx, "panic in http handler",
					slog.Any("panic", fmt.Sprint(rec)), slog.String("stack", string(debug.Stack())))
				WriteProblem(w, r, http.StatusInternalServerError, "internal", "Internal error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Tracing creates a server span per request named after the route pattern (bounded cardinality).
// Metrics are produced by Metrics, so otelhttp's own instruments are disabled.
func Tracing(tp trace.TracerProvider, serverName string) Middleware {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, serverName,
			otelhttp.WithTracerProvider(tp),
			otelhttp.WithMeterProvider(noop.NewMeterProvider()),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if p := routePattern(r); p != "" {
					return r.Method + " " + p
				}
				return r.Method
			}),
		)
	}
}

// Metrics records RED metrics with low-cardinality labels: method, route pattern, status class/code.
//
//	http_server_requests_total{method,route,status}
//	http_server_request_duration_seconds{method,route,status}
//	http_server_requests_in_flight
func Metrics(mp metric.MeterProvider) (Middleware, error) {
	m := mp.Meter("github.com/zodixdev/infrapilot/pkg/httpserver")
	total, err := m.Int64Counter("http_server_requests_total", metric.WithDescription("HTTP requests served"))
	if err != nil {
		return nil, fmt.Errorf("httpserver: create counter: %w", err)
	}
	dur, err := m.Float64Histogram("http_server_request_duration_seconds",
		metric.WithDescription("HTTP request latency"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10))
	if err != nil {
		return nil, fmt.Errorf("httpserver: create histogram: %w", err)
	}
	inflight, err := m.Int64UpDownCounter("http_server_requests_in_flight", metric.WithDescription("In-flight HTTP requests"))
	if err != nil {
		return nil, fmt.Errorf("httpserver: create gauge: %w", err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := r.Context()
			inflight.Add(ctx, 1)
			rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				inflight.Add(ctx, -1)
				attrs := metric.WithAttributes(
					attribute.String("method", r.Method),
					attribute.String("route", routeOrUnmatched(r)),
					attribute.String("status", strconv.Itoa(rw.status)),
				)
				total.Add(ctx, 1, attrs)
				dur.Record(ctx, time.Since(start).Seconds(), attrs)
			}()
			next.ServeHTTP(rw, r)
		})
	}, nil
}

// AccessLog writes one structured line per request at Info (Warn for 5xx). Health probes are
// logged at Debug to keep logs readable.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			level := slog.LevelInfo
			switch {
			case rw.status >= http.StatusInternalServerError:
				level = slog.LevelWarn
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics":
				level = slog.LevelDebug
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("route", routeOrUnmatched(r)),
				slog.String("path", r.URL.Path),
				slog.Int("status", rw.status),
				slog.Int64("bytes", rw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// Standard returns the default stack: request id → recovery → tracing → metrics → access log.
func Standard(logger *slog.Logger, tp trace.TracerProvider, mp metric.MeterProvider, serverName string) ([]Middleware, error) {
	metrics, err := Metrics(mp)
	if err != nil {
		return nil, err
	}
	return []Middleware{RequestID(), Recover(logger), Tracing(tp, serverName), metrics, AccessLog(logger)}, nil
}

// Problem is the RFC 9457 body written by WriteProblem (kept in sync with contracts/openapi/public.yaml).
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code"`
	TraceID  string `json:"trace_id"`
}

// WriteProblem writes an application/problem+json response.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, title string) {
	p := Problem{
		Type:     "about:blank",
		Title:    title,
		Status:   status,
		Instance: r.URL.Path,
		Code:     code,
		TraceID:  trace.SpanContextFromContext(r.Context()).TraceID().String(),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p) // status already sent; nothing more useful to do on failure
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush/Hijack on the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// routePattern returns the matched ServeMux pattern without its method prefix ("GET /orgs/{id}" → "/orgs/{id}").
func routePattern(r *http.Request) string {
	p := r.Pattern
	if i := strings.IndexByte(p, ' '); i >= 0 {
		return p[i+1:]
	}
	return p
}

func routeOrUnmatched(r *http.Request) string {
	if p := routePattern(r); p != "" {
		return p
	}
	return "unmatched"
}
