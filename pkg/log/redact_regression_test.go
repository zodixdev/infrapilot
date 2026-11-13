package log_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type nilError struct{}

func (*nilError) Error() string { return "boom" }

type secretValue string

func (secretValue) LogValue() slog.Value { return slog.StringValue("[masked]") }

type tagged struct {
	User string `json:"user"`
	Pw   string `json:"password"`
}

type withValuer struct{ Value secretValue }

// TestHandler_RedactionEdgeCases: known bypass shapes and values that must not crash the logger.
func TestHandler_RedactionEdgeCases(t *testing.T) {
	t.Parallel()
	var nilURL *url.URL
	var typedNil *nilError
	big := make([]int, 1000)
	tests := []struct {
		name string
		attr slog.Attr
		keep string
	}{
		{"url userinfo stringer", slog.Any("u", url.UserPassword("bob", "LEAK1")), ""},
		{"cookie header", slog.String("h", "Cookie: sid=LEAK2; theme=dark"), "Cookie"},
		{"set-cookie header", slog.String("h", "Set-Cookie: sid=LEAK3; HttpOnly"), "Set-Cookie"},
		{"session/auth in json", slog.String("b", `{"session":"LEAK4","auth":"LEAK5","user":"bob"}`), "bob"},
		{"escaped quote", slog.String("b", `{"password":"ab\"LEAK6 x"}`), ""},
		{"xml attrs", slog.String("b", `<password type="x">LEAK7</password>`), "</password>"},
		{"json tag", slog.Any("cfg", tagged{User: "bob", Pw: "LEAK8"}), "bob"},
		{"byte array", slog.Any("k", [4]byte{'L', 'E', 'A', 'K'}), "bytes len=4"},
		{"map key", slog.Any("m", map[string]int{"ipk_LEAK9abcdefgh": 1}), ""},
		{"token scheme", slog.String("h", "X-Thing: Token LEAK10abcdefghijklmnopqrstu"), "X-Thing"},
		{"token-only url", slog.String("u", "https://ghp_LEAK11@github.com/org/repo.git"), "github.com/org/repo.git"},
		{"nested log valuer", slog.Any("v", withValuer{Value: "LEAK12"}), "[masked]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newLogger(t)
			l.Info("m", tt.attr)
			out := buf.String()
			require.NotContains(t, out, "LEAK", out)
			require.NotContains(t, out, "\"0\":76", "byte arrays are not walked element by element")
			if tt.keep != "" {
				require.Contains(t, out, tt.keep, out)
			}
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &m))
		})
	}

	t.Run("nil receivers never crash the logger", func(t *testing.T) {
		t.Parallel()
		l, buf := newLogger(t)
		require.NotPanics(t, func() {
			l.Info("m", slog.Any("u", nilURL), slog.Any("e", error(typedNil)), slog.Any("j", errors.Join(typedNil)))
		})
		require.Contains(t, buf.String(), `"u":"<nil>"`)
	})

	t.Run("large containers are bounded and maps are sorted", func(t *testing.T) {
		t.Parallel()
		l, buf := newLogger(t)
		l.Info("m", slog.Any("big", big), slog.Any("m", map[string]int{"b": 2, "a": 1}))
		out := buf.String()
		require.Contains(t, out, "…truncated")
		require.Less(t, strings.Count(out, ","), 200)
		require.Less(t, strings.Index(out, `"a":1`), strings.Index(out, `"b":2`))
	})

	t.Run("host:port urls with @ in the query are not mangled", func(t *testing.T) {
		t.Parallel()
		l, buf := newLogger(t)
		l.Info("m", slog.String("err", "GET http://api:8080/v1/users?email=a@b.com failed"))
		require.Contains(t, buf.String(), "http://api:8080/v1/users?email=a@b.com")
	})
}
