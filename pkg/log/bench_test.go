package log_test

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/zodixdev/infrapilot/pkg/log"
)

// Hot path: one access-log-shaped record per request.
func BenchmarkHandle_AccessLogRecord(b *testing.B) {
	l, err := log.New(log.Config{Level: "info"}, io.Discard, "svc", "v", "prod")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		l.Info("http request", slog.String("method", "GET"), slog.String("route", "/v1/orgs/{id}"),
			slog.String("path", "/v1/orgs/0192f7a0"), slog.Int("status", 200), slog.Int64("bytes", 512),
			slog.String("remote", "192.0.2.10:5555"), slog.String("user_agent", "k6/0.50"))
	}
}

// Worst realistic case: an error carrying a DSN plus a structured value.
func BenchmarkHandle_ErrorWithStructuredValue(b *testing.B) {
	l, err := log.New(log.Config{Level: "info"}, io.Discard, "svc", "v", "prod")
	if err != nil {
		b.Fatal(err)
	}
	cfg := struct {
		Host, Database string
		Password       string
		Pool           struct{ Max, Min int }
	}{Host: "db", Database: "cp", Password: "secret"}
	e := errors.New("dial postgres://app:secret@db:5432/cp: connection refused")
	b.ReportAllocs()
	for b.Loop() {
		l.Error("query failed", slog.Any("error", e), slog.Any("cfg", cfg))
	}
}
