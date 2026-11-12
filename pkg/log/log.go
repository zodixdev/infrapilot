// Package log builds the structured slog logger used by every InfraPilot service: JSON output,
// redaction of secret-looking attributes (AGENTS.md §8), and trace/span ids taken from the context
// (ADR-0012).
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Config controls the handler.
type Config struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is json (default) or text (local development only).
	Format string
	// AddSource adds file:line of the log call.
	AddSource bool
}

// Redacted replaces every secret-looking value.
const Redacted = "[REDACTED]"

// secretKeyFragments are matched case-insensitively against attribute keys.
var secretKeyFragments = []string{
	"password", "passwd", "secret", "token", "authorization", "api_key", "apikey", "api-key",
	"cookie", "set-cookie", "dsn", "private_key", "privatekey", "credential", "client_secret", "session",
}

// secretValuePatterns catch secrets embedded in free-form strings (DSNs, URLs, key=value fragments).
var secretValuePatterns = []*regexp.Regexp{
	// scheme://user:password@host
	regexp.MustCompile(`(://[^/\s:@]+:)([^@/\s]+)(@)`),
	// password=... token=... in connection strings / query strings
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|client[_-]?secret)(\s*[=:]\s*)([^\s&;,'"]+)`),
	// Authorization header values
	regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)([A-Za-z0-9\-._~+/]+=*)`),
}

// New returns a logger writing to w. service/version/env are attached to every record.
func New(cfg Config, w io.Writer, service, version, environment string) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(cfg.Level))); err != nil {
		return nil, fmt.Errorf("log level %q: %w", cfg.Level, err)
	}
	opts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   cfg.AddSource,
		ReplaceAttr: redactAttr,
	}
	var base slog.Handler
	switch cfg.Format {
	case "", "json":
		base = slog.NewJSONHandler(w, opts)
	case "text":
		base = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("log format %q: must be json or text", cfg.Format)
	}
	h := &contextHandler{Handler: base}
	return slog.New(h).With(
		slog.String("service", service),
		slog.String("version", version),
		slog.String("env", environment),
	), nil
}

// Discard returns a logger that drops everything. For tests.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

type ctxKey struct{}

// ContextWithAttrs attaches attributes (e.g. request_id, org_id) that every record logged with
// this context will carry.
func ContextWithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	existing, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, ctxKey{}, merged)
}

// contextHandler enriches records with trace ids and context attributes before delegating.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// redactAttr is the slog ReplaceAttr hook: secret keys are masked entirely, string values are
// scrubbed for embedded secrets, errors are rendered as scrubbed strings.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if isSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, scrub(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, scrub(err.Error()))
		}
		if s, ok := a.Value.Any().(fmt.Stringer); ok {
			return slog.String(a.Key, scrub(s.String()))
		}
	case slog.KindGroup:
		// Members are passed to ReplaceAttr individually by slog.
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindTime, slog.KindUint64, slog.KindLogValuer:
	}
	return a
}

func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, frag := range secretKeyFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

func scrub(s string) string {
	out := secretValuePatterns[0].ReplaceAllString(s, "${1}"+Redacted+"${3}")
	out = secretValuePatterns[1].ReplaceAllString(out, "${1}${2}"+Redacted)
	out = secretValuePatterns[2].ReplaceAllString(out, "${1}${2}"+Redacted)
	return out
}
