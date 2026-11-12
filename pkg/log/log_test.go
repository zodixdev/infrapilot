package log_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/zodixdev/infrapilot/pkg/log"
)

func newLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := log.New(log.Config{Level: "debug"}, &buf, "svc", "1.2.3", "test")
	require.NoError(t, err)
	return l, &buf
}

func lastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var m map[string]any
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &m), string(lines[len(lines)-1]))
	return m
}

func TestNew_StandardFields(t *testing.T) {
	t.Parallel()
	l, buf := newLogger(t)
	l.Info("started", slog.Int("port", 8080))
	rec := lastRecord(t, buf)
	require.Equal(t, "started", rec["msg"])
	require.Equal(t, "INFO", rec["level"])
	require.Equal(t, "svc", rec["service"])
	require.Equal(t, "1.2.3", rec["version"])
	require.Equal(t, "test", rec["env"])
	require.InDelta(t, 8080, rec["port"], 0)
	require.NotEmpty(t, rec["time"])
}

func TestNew_RejectsBadConfig(t *testing.T) {
	t.Parallel()
	_, err := log.New(log.Config{Level: "loud"}, &bytes.Buffer{}, "s", "v", "e")
	require.Error(t, err)
	_, err = log.New(log.Config{Level: "info", Format: "xml"}, &bytes.Buffer{}, "s", "v", "e")
	require.Error(t, err)
}

func TestNew_LevelFilter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l, err := log.New(log.Config{Level: "warn"}, &buf, "s", "v", "e")
	require.NoError(t, err)
	l.Info("hidden")
	require.Empty(t, buf.String())
	l.Warn("shown")
	require.Contains(t, buf.String(), "shown")
}

func TestHandler_RedactsSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		attr  slog.Attr
		key   string
		want  string
		plain string // substring that must NOT appear anywhere in the output
	}{
		{"password key", slog.String("password", "hunter2"), "password", log.Redacted, "hunter2"},
		{"nested key fragment", slog.String("db_password_hash", "abc"), "db_password_hash", log.Redacted, "abc"},
		{"authorization header", slog.String("Authorization", "Bearer eyJ.abc"), "Authorization", log.Redacted, "eyJ.abc"},
		{"api key camel", slog.String("apiKey", "ipk_123"), "apiKey", log.Redacted, "ipk_123"},
		{"dsn value", slog.String("url", "postgres://app:s3cr3t@db:5432/x"), "url", "postgres://app:" + log.Redacted + "@db:5432/x", "s3cr3t"},
		{"kv in string", slog.String("conn", "host=db password=p4ss sslmode=disable"), "conn", "host=db password=" + log.Redacted + " sslmode=disable", "p4ss"},
		{"bearer in string", slog.String("hdr", "got Bearer abc.def.ghi today"), "hdr", "got Bearer " + log.Redacted + " today", "abc.def"},
		{"error value", slog.Any("error", errors.New("dial postgres://u:pw@h/db: refused")), "error", "dial postgres://u:" + log.Redacted + "@h/db: refused", ":pw@"},
		{"int untouched", slog.Int("count", 3), "count", "", "never"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(t)
			l.Info("m", tt.attr)
			out := buf.String()
			require.NotContains(t, out, tt.plain)
			rec := lastRecord(t, buf)
			if tt.want != "" {
				require.Equal(t, tt.want, rec[tt.key])
			}
		})
	}
}

func TestHandler_RedactsInsideGroupsAndWith(t *testing.T) {
	t.Parallel()
	l, buf := newLogger(t)
	l.With(slog.String("token", "t0k")).WithGroup("db").Info("m", slog.Group("conn", slog.String("secret", "s3")))
	out := buf.String()
	require.NotContains(t, out, "t0k")
	require.NotContains(t, out, "s3")
	require.Contains(t, out, log.Redacted)
}

func TestHandler_AddsTraceAndContextAttrs(t *testing.T) {
	t.Parallel()
	l, buf := newLogger(t)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0xaa, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		SpanID:     trace.SpanID{0xbb, 1, 2, 3, 4, 5, 6, 7},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(t.Context(), sc)
	ctx = log.ContextWithAttrs(ctx, slog.String("org_id", "org-1"))
	ctx = log.ContextWithAttrs(ctx, slog.String("request_id", "r-1"))

	l.InfoContext(ctx, "handled")
	rec := lastRecord(t, buf)
	require.Equal(t, sc.TraceID().String(), rec["trace_id"])
	require.Equal(t, sc.SpanID().String(), rec["span_id"])
	require.Equal(t, "org-1", rec["org_id"])
	require.Equal(t, "r-1", rec["request_id"])

	l.Info("no ctx")
	rec = lastRecord(t, buf)
	require.NotContains(t, rec, "trace_id")
}

func TestDiscard(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { log.Discard().Info("x") })
}
