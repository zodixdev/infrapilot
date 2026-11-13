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

// RequestID reuses a well-formed inbound X-Request-Id (1–128 chars of [A-Za-z0-9._:-]) or generates a
// UUIDv7, echoes it in the response and attaches it to the log context. Internal services trust the id set
// by the gateway; the public edge (M02 gateway) always generates its own and logs the client's separately.
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
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// Recover converts panics into a 500 problem+json response and logs the stack. If the handler had already
// started the response, the connection is aborted instead (a half-written body must not look complete).
// http.ErrAbortHandler is re-panicked as net/http expects. Place it innermost so that metrics, access logs
// and spans observe the 500.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			rw := wrap(w)
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
				if rw.wroteHeader {
					panic(http.ErrAbortHandler)
				}
				WriteProblem(rw, r, http.StatusInternalServerError, "internal", "Internal error")
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// BodyLimit rejects bodies larger than maxBytes with 413: early when Content-Length says so, otherwise the
// handler's read fails with *http.MaxBytesError once the limit is crossed.
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				WriteProblem(w, r, http.StatusRequestEntityTooLarge, "validation_failed",
					"Request body exceeds "+strconv.FormatInt(maxBytes, 10)+" bytes")
				return
			}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
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
					return normalizeMethod(r.Method) + " " + p
				}
				return normalizeMethod(r.Method)
			}),
		)
	}
}

// Metrics records RED metrics with low-cardinality labels: method (standard methods, else _OTHER), route
// pattern, status code. A panic passing through is recorded as 500 and re-raised.
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
			rw := wrap(w)
			defer func() {
				rec := recover()
				status := rw.status
				if rec != nil {
					status = http.StatusInternalServerError
				}
				inflight.Add(ctx, -1)
				attrs := metric.WithAttributes(
					attribute.String("method", normalizeMethod(r.Method)),
					attribute.String("route", routeOrUnmatched(r)),
					attribute.String("status", strconv.Itoa(status)),
				)
				total.Add(ctx, 1, attrs)
				dur.Record(ctx, time.Since(start).Seconds(), attrs)
				if rec != nil {
					panic(rec)
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}, nil
}

// AccessLog writes one structured line per request at Info (Warn for 5xx, Debug for health/metrics
// routes). A panic passing through is logged as 500 and re-raised.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := r.Context()
			rw := wrap(w)
			defer func() {
				rec := recover()
				status := rw.status
				if rec != nil {
					status = http.StatusInternalServerError
				}
				route := routeOrUnmatched(r)
				level := slog.LevelInfo
				switch {
				case status >= http.StatusInternalServerError:
					level = slog.LevelWarn
				case route == "/healthz" || route == "/readyz" || route == "/metrics":
					level = slog.LevelDebug
				}
				logger.LogAttrs(ctx, level, "http request",
					slog.String("method", normalizeMethod(r.Method)),
					slog.String("route", route),
					slog.String("path", r.URL.Path),
					slog.Int("status", status),
					slog.Int64("bytes", rw.bytes),
					slog.Duration("duration", time.Since(start)),
					slog.String("remote", r.RemoteAddr),
					slog.String("user_agent", r.UserAgent()),
				)
				if rec != nil {
					panic(rec)
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// Options for the standard middleware stack.
type Options struct {
	Logger         *slog.Logger
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	ServerName     string
	// MaxBodyBytes for every route (BodyLimit); <= 0 disables the limit.
	MaxBodyBytes int64
}

// Standard returns the default stack, outermost first:
// request id → tracing → metrics → access log → body limit → recovery.
// Recovery is innermost so every outer layer observes the 500 it writes.
func Standard(o Options) ([]Middleware, error) {
	metrics, err := Metrics(o.MeterProvider)
	if err != nil {
		return nil, err
	}
	mws := []Middleware{RequestID(), Tracing(o.TracerProvider, o.ServerName), metrics, AccessLog(o.Logger)}
	if o.MaxBodyBytes > 0 {
		mws = append(mws, BodyLimit(o.MaxBodyBytes))
	}
	return append(mws, Recover(o.Logger)), nil
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

var standardMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true,
	http.MethodDelete: true, http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

// normalizeMethod bounds label cardinality: clients can send arbitrary method tokens.
func normalizeMethod(m string) string {
	if standardMethods[m] {
		return m
	}
	return "_OTHER"
}

// statusWriter records the final status and byte count. Each middleware layer wraps once; wrap reuses an
// existing statusWriter so nested layers share the observation.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func wrap(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx responses are informational; the final status comes later.
	if !w.wroteHeader && code >= 200 {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush supports streaming handlers that type-assert http.Flusher.
func (w *statusWriter) Flush() {
	w.wroteHeader = true
	_ = http.NewResponseController(w.ResponseWriter).Flush() // unsupported writers simply do not flush
}

// Unwrap lets http.ResponseController reach Hijack/SetDeadline on the underlying writer.
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
