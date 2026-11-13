package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/zodixdev/infrapilot/pkg/log"
)

func newLogger(t *testing.T, extra ...slog.Handler) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := log.New(log.Config{Level: "debug"}, &buf, "svc", "1.2.3", "test", extra...)
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
	var buf, extra bytes.Buffer
	l, err := log.New(log.Config{Level: "warn"}, &buf, "s", "v", "e", slog.NewJSONHandler(&extra, &slog.HandlerOptions{Level: slog.LevelDebug}))
	require.NoError(t, err)
	l.Info("hidden")
	require.Empty(t, buf.String())
	require.Empty(t, extra.String(), "extra handlers follow the configured level")
	l.Warn("shown")
	require.Contains(t, buf.String(), "shown")
	require.Contains(t, extra.String(), "shown")
}

type dbConfig struct {
	Host        string
	DatabaseURL string
	Password    string
	Nested      struct{ APIKey string }
}

type stringer struct{}

func (stringer) String() string { return "conn password=s3cret-stringer" }

// Every value below contains the marker "LEAK". No output may contain it.
func TestHandler_RedactsSecrets(t *testing.T) {
	t.Parallel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer LEAK-hdr")
	hdr.Set("Accept", "application/json")
	cfg := dbConfig{Host: "db", DatabaseURL: "postgres://a:LEAK16@h/d", Password: "LEAK17"}
	cfg.Nested.APIKey = "LEAK-nested"

	tests := []struct {
		name string
		attr slog.Attr
		keep string // a non-secret substring that must survive
	}{
		{"password key", slog.String("password", "LEAK1"), ""},
		{"key fragment", slog.String("db_password_hash", "LEAK2"), ""},
		{"authorization key", slog.String("Authorization", "Bearer LEAK3"), ""},
		{"camel api key", slog.String("apiKey", "LEAK4"), ""},
		{"hyphen private key", slog.String("private-key", "LEAK5"), ""},
		{"exact pass key", slog.String("pass", "LEAK6"), ""},
		{"jwt key", slog.String("jwt", "LEAK7"), ""},
		{"dsn value", slog.String("url", "postgres://app:LEAK8@db:5432/x"), "postgres://app:"},
		{"empty user url", slog.String("redis", "redis://:LEAK9@redis:6379/0"), "redis://:"},
		{"password with slash", slog.String("u", "postgres://u:ab/LEAK10@h/db"), "@h/db"},
		{"password with at", slog.String("u", "postgres://u:p@LEAK11@h/db"), "@h/db"},
		{"kv in string", slog.String("conn", "host=db password=LEAK12 sslmode=disable"), "sslmode=disable"},
		{"quoted libpq", slog.String("conn", "host=db password='LEAK13 x' sslmode=disable"), "sslmode=disable"},
		{"prefixed env", slog.String("env", "PGPASSWORD=LEAK14 POSTGRES_PASSWORD=LEAK15"), ""},
		{"oauth query", slog.String("q", "access_token=LEAK18&refresh_token=LEAK19&state=ok"), "state=ok"},
		{"aws key", slog.String("e", "aws_secret_access_key=LEAK20"), ""},
		{"json body", slog.String("body", `{"user":"bob","password":"LEAK21","token": "LEAK22"}`), `bob`},
		{"xml body", slog.String("body", "<user>bob</user><password>LEAK23</password>"), "<user>bob</user>"},
		{"bearer in text", slog.String("hdr", "got Bearer LEAK24 today"), "today"},
		{"token scheme header", slog.String("hdr", "Authorization: token LEAK25"), ""},
		{"raw jwt", slog.String("t", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJMRUFLMjYifQ.LEAK26sig"), ""},
		{"api key value", slog.String("x", "key ipk_LEAK27abcdef"), ""},
		{"error value", slog.Any("error", errors.New("dial postgres://u:LEAK28@h/db: refused")), "refused"},
		{"stringer", slog.Any("s", stringer{}), "conn"},
		{"http header", slog.Any("headers", hdr), "application/json"},
		{"url values", slog.Any("form", url.Values{"password": {"LEAK29"}, "user": {"bob"}}), "bob"},
		{"struct", slog.Any("cfg", cfg), `"Host":"db"`},
		{"pointer struct", slog.Any("cfg", &cfg), `"Host":"db"`},
		{"map", slog.Any("m", map[string]any{"secret": "LEAK30", "inner": map[string]string{"token": "LEAK31"}}), ""},
		{"bytes", slog.Any("raw", []byte("password=LEAK32")), "bytes len="},
		{"group", slog.Group("db", slog.String("password", "LEAK33"), slog.String("host", "h")), `"host":"h"`},
		{"secret group name", slog.Group("credentials", slog.String("value", "LEAK34")), ""},
		{"slice", slog.Any("list", []string{"ok", "password=LEAK35"}), `"ok"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(t)
			l.Info("m", tt.attr)
			out := buf.String()
			require.NotContains(t, out, "LEAK", out)
			require.Contains(t, out, "REDACTED", out)
			if tt.keep != "" {
				require.Contains(t, out, tt.keep, "over-redaction: %s", out)
			}
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &m), "output stays valid JSON")
		})
	}
}

func TestHandler_RedactsWithAndWithGroupAndMessages(t *testing.T) {
	t.Parallel()
	l, buf := newLogger(t)
	l.With(slog.String("token", "LEAK-with")).WithGroup("db").Info("m", slog.Group("conn", slog.String("secret", "LEAK-g")))
	l.WithGroup("authorization").Info("m", slog.String("value", "LEAK-wg"))
	l.Info("connect postgres://u:LEAK-msg@h/db failed")
	out := buf.String()
	require.NotContains(t, out, "LEAK")
	require.Equal(t, 3, strings.Count(out, "\n"))
}

func TestHandler_RedactsBeforeExtraHandlers(t *testing.T) {
	t.Parallel()
	var extra bytes.Buffer
	l, buf := newLogger(t, slog.NewJSONHandler(&extra, nil))
	l.Info("m", slog.String("password", "LEAK"), slog.String("user", "bob"))
	require.NotContains(t, extra.String(), "LEAK", "the OTLP bridge sees the same scrubbed record")
	require.Contains(t, extra.String(), "bob")
	require.NotContains(t, buf.String(), "LEAK")
}

func TestHandler_DoesNotOverRedact(t *testing.T) {
	t.Parallel()
	l, buf := newLogger(t)
	l.Info("token validation failed", slog.String("route", "/v1/orgs/{id}"), slog.Int("status", 401),
		slog.String("partition", "check.results.v1"), slog.String("msg_detail", "cache miss for key=user:42"))
	rec := lastRecord(t, buf)
	require.Equal(t, "token validation failed", rec["msg"])
	require.Equal(t, "/v1/orgs/{id}", rec["route"])
	require.Equal(t, "check.results.v1", rec["partition"])
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
	ctx = log.ContextWithAttrs(ctx, slog.String("request_id", "r-1"), slog.String("session", "LEAK"))

	l.InfoContext(ctx, "handled")
	rec := lastRecord(t, buf)
	require.Equal(t, sc.TraceID().String(), rec["trace_id"])
	require.Equal(t, sc.SpanID().String(), rec["span_id"])
	require.Equal(t, "org-1", rec["org_id"])
	require.Equal(t, "r-1", rec["request_id"])
	require.Equal(t, log.Redacted, rec["session"])

	l.InfoContext(context.WithoutCancel(t.Context()), "no span")
	rec = lastRecord(t, buf)
	require.NotContains(t, rec, "trace_id")
}

func TestFallbackAndDiscard(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log.Fallback(&buf).Error("startup failed", slog.Any("error", errors.New("connect postgres://u:LEAK@h/db")))
	require.NotContains(t, buf.String(), "LEAK")
	require.NotPanics(t, func() { log.Discard().Info("x") })
}

func FuzzScrub_NoPlantedSecretSurvives(f *testing.F) {
	for _, seed := range []string{"password=%s", "postgres://u:%s@h/db", `{"token":"%s"}`, "Bearer %s", "PGPASSWORD='%s'"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, format string) {
		if strings.Count(format, "%s") != 1 || strings.ContainsAny(format, "%\n") && strings.Count(format, "%") != 1 {
			t.Skip()
		}
		const marker = "Zq9LeakMarker7"
		// Only assert for the known secret shapes (the seeds); arbitrary formats just must not panic.
		out := log.Scrub(strings.Replace(format, "%s", marker, 1))
		for _, seed := range []string{"password=%s", "postgres://u:%s@h/db", `{"token":"%s"}`, "Bearer %s", "PGPASSWORD='%s'"} {
			if format == seed {
				require.NotContains(t, out, marker)
			}
		}
	})
}
