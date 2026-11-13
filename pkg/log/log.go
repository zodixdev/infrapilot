// Package log builds the structured slog logger used by every InfraPilot service: JSON output, secret
// redaction applied before any handler sees a record (AGENTS.md §8), trace/span ids and context attributes
// on every record (ADR-0012), and optional fan-out to additional handlers such as the OTLP log bridge.
package log

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// New returns a logger writing to w and to every extra handler. Redaction runs once, before fan-out, so
// all destinations receive the same scrubbed record. service/version/env are attached to every record.
func New(cfg Config, w io.Writer, service, version, environment string, extra ...slog.Handler) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(cfg.Level))); err != nil {
		return nil, fmt.Errorf("log level %q: %w", cfg.Level, err)
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource}
	var base slog.Handler
	switch cfg.Format {
	case "", "json":
		base = slog.NewJSONHandler(w, opts)
	case "text":
		base = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("log format %q: must be json or text", cfg.Format)
	}
	handlers := []slog.Handler{base}
	for _, h := range extra {
		handlers = append(handlers, &levelFilter{Handler: h, level: level})
	}
	out := handlers[0]
	if len(handlers) > 1 {
		out = &fanout{handlers: handlers}
	}
	return slog.New(&redactingHandler{next: out}).With(
		slog.String("service", service),
		slog.String("version", version),
		slog.String("env", environment),
	), nil
}

// Fallback returns a redacting JSON logger with default settings for failures that happen before the
// configuration is loaded (main's last-resort error path).
func Fallback(w io.Writer) *slog.Logger {
	return slog.New(&redactingHandler{next: slog.NewJSONHandler(w, nil)})
}

// Discard returns a logger that drops everything. For tests.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

type ctxKey struct{}

// ContextWithAttrs attaches attributes (e.g. request_id, org_id) that every record logged with this
// context will carry.
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

// redactingHandler enriches records with trace ids and context attributes, scrubs every attribute (including
// those bound with With/WithGroup) and forwards a rewritten record.
type redactingHandler struct {
	next slog.Handler
	// secretGroup is true when any enclosing WithGroup name is a secret key: everything below is masked.
	secretGroup bool
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Scrub(r.Message), r.PC)
	add := func(a slog.Attr) { out.AddAttrs(redact(a, h.secretGroup, 0)) }
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		out.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	if attrs, ok := ctx.Value(ctxKey{}).([]slog.Attr); ok {
		for _, a := range attrs {
			add(a)
		}
	}
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = redact(a, h.secretGroup, 0)
	}
	return &redactingHandler{next: h.next.WithAttrs(clean), secretGroup: h.secretGroup}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), secretGroup: h.secretGroup || isSecretKey(name)}
}

// fanout sends each record to several handlers; the first error is returned after all have run.
type fanout struct{ handlers []slog.Handler }

func (f *fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f *fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (f *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &fanout{handlers: hs}
}

func (f *fanout) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &fanout{handlers: hs}
}

// levelFilter applies the configured level to extra handlers that have their own (or no) level.
type levelFilter struct {
	slog.Handler
	level slog.Level
}

func (l *levelFilter) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= l.level && l.Handler.Enabled(ctx, lv)
}

func (l *levelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelFilter{Handler: l.Handler.WithAttrs(attrs), level: l.level}
}

func (l *levelFilter) WithGroup(name string) slog.Handler {
	return &levelFilter{Handler: l.Handler.WithGroup(name), level: l.level}
}
