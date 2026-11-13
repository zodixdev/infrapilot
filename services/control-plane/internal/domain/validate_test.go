package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

func violations(fn func(v *domain.Validator)) []string {
	var v domain.Validator
	fn(&v)
	var ve *domain.ValidationError
	if err := v.Err(); errors.As(err, &ve) {
		out := make([]string, 0, len(ve.Violations))
		for _, x := range ve.Violations {
			out = append(out, x.Field)
		}
		return out
	}
	return nil
}

func TestValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		fn   func(v *domain.Validator)
		bad  bool
	}{
		{"name ok", func(v *domain.Validator) { domain.CheckName(v, "name", "Acme Inc.") }, false},
		{"name unicode ok", func(v *domain.Validator) { domain.CheckName(v, "name", "Ünïcødé 組織") }, false},
		{"name blank", func(v *domain.Validator) { domain.CheckName(v, "name", "   ") }, true},
		{"name too long", func(v *domain.Validator) { domain.CheckName(v, "name", strings.Repeat("a", 201)) }, true},
		{"name control char", func(v *domain.Validator) { domain.CheckName(v, "name", "a\x00b") }, true},
		{"name invalid utf8", func(v *domain.Validator) { domain.CheckName(v, "name", "\xff") }, true},
		{"slug ok", func(v *domain.Validator) { domain.CheckSlug(v, "slug", "web-app-2") }, false},
		{"slug uppercase", func(v *domain.Validator) { domain.CheckSlug(v, "slug", "Web") }, true},
		{"slug trailing hyphen", func(v *domain.Validator) { domain.CheckSlug(v, "slug", "web-") }, true},
		{"slug too short", func(v *domain.Validator) { domain.CheckSlug(v, "slug", "ab") }, true},
		{"slug leading digit", func(v *domain.Validator) { domain.CheckSlug(v, "slug", "1web") }, true},
		{"labels ok", func(v *domain.Validator) {
			domain.CheckLabels(v, "labels", map[string]string{"env": "prod", "team.name": "sre"})
		}, false},
		{"label bad key", func(v *domain.Validator) { domain.CheckLabels(v, "labels", map[string]string{"Env": "prod"}) }, true},
		{"label long value", func(v *domain.Validator) {
			domain.CheckLabels(v, "labels", map[string]string{"env": strings.Repeat("x", 256)})
		}, true},
		{"too many labels", func(v *domain.Validator) {
			m := map[string]string{}
			for i := range 33 {
				m["k"+strings.Repeat("a", i)] = "v"
			}
			domain.CheckLabels(v, "labels", m)
		}, true},
		{"email ok", func(v *domain.Validator) { domain.NormalizeEmail(v, "email", "dev@example.com") }, false},
		{"email display form rejected", func(v *domain.Validator) { domain.NormalizeEmail(v, "email", "Dev <dev@example.com>") }, true},
		{"email garbage", func(v *domain.Validator) { domain.NormalizeEmail(v, "email", "not-an-email") }, true},
		{"scopes ok", func(v *domain.Validator) { domain.CheckScopes(v, "scopes", []domain.Scope{"read", "write"}) }, false},
		{"scopes empty", func(v *domain.Validator) { domain.CheckScopes(v, "scopes", nil) }, true},
		{"scopes unknown", func(v *domain.Validator) { domain.CheckScopes(v, "scopes", []domain.Scope{"admin"}) }, true},
		{"scopes duplicate", func(v *domain.Validator) { domain.CheckScopes(v, "scopes", []domain.Scope{"read", "read"}) }, true},
		{"expiry ok", func(v *domain.Validator) { e := now.Add(24 * time.Hour); domain.CheckExpiry(v, "expire_time", &e, now) }, false},
		{"expiry past", func(v *domain.Validator) { e := now.Add(-time.Second); domain.CheckExpiry(v, "expire_time", &e, now) }, true},
		{"expiry too far", func(v *domain.Validator) {
			e := now.Add(401 * 24 * time.Hour)
			domain.CheckExpiry(v, "expire_time", &e, now)
		}, true},
		{"description too long", func(v *domain.Validator) { domain.CheckDescription(v, "description", strings.Repeat("d", 2001)) }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := violations(tc.fn)
			require.Equal(t, tc.bad, len(got) > 0, "violations: %v", got)
		})
	}
}

func TestAPIKey(t *testing.T) {
	t.Parallel()
	secret, hash, display, err := domain.NewAPIKey()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(secret, "ipk_"))
	require.Len(t, secret, 4+43)
	require.Equal(t, secret[:12], display)
	require.Regexp(t, `^ipk_[A-Za-z0-9_-]{8}$`, display, "matches the database CHECK")
	require.True(t, domain.LooksLikeAPIKey(secret))
	require.NotEqual(t, [32]byte{}, hash)
	other, _, _, err := domain.NewAPIKey()
	require.NoError(t, err)
	require.NotEqual(t, secret, other)
	for _, bad := range []string{"", "ipk_", "ipk_short", "xyz_" + secret[4:], secret + "x", "ipk_" + strings.Repeat("!", 43)} {
		require.False(t, domain.LooksLikeAPIKey(bad), bad)
	}
}

func TestPage(t *testing.T) {
	t.Parallel()
	var v domain.Validator
	p := domain.ParsePage(&v, 0, "")
	require.NoError(t, v.Err())
	require.EqualValues(t, domain.DefaultPageSize, p.Size)
	require.Equal(t, uuid.Nil, p.Cursor)
	require.EqualValues(t, 51, p.Limit())

	last := uuid.Must(uuid.NewV7())
	rows := make([]int, 3)
	page, more := domain.Trim(domain.Page{Size: 2}, rows)
	require.Len(t, page, 2)
	require.True(t, more)
	token := domain.NextToken(more, last)
	p = domain.ParsePage(&v, 2, token)
	require.NoError(t, v.Err())
	require.Equal(t, last, p.Cursor, "token round-trips")
	_, more = domain.Trim(domain.Page{Size: 3}, rows)
	require.False(t, more)
	require.Empty(t, domain.NextToken(false, last))

	for _, tc := range []struct {
		size  int32
		token string
	}{{-1, ""}, {201, ""}, {10, "not base64!"}, {10, "AAAA"}} {
		var bad domain.Validator
		domain.ParsePage(&bad, tc.size, tc.token)
		require.Error(t, bad.Err(), "%+v", tc)
	}
}
