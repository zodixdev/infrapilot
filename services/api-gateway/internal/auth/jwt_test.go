package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	"go.uber.org/goleak"

	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const (
	issuer   = "http://keycloak.test/realms/infrapilot"
	audience = "infrapilot-api"
)

// idp is a fake identity provider serving a JWKS whose availability tests control.
type idp struct {
	mu   sync.Mutex
	keys []jose.JSONWebKey
	down bool
	hits atomic.Int32
	srv  *httptest.Server
	rsa  *rsa.PrivateKey
	kid  string
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p := &idp{rsa: k, kid: "rsa-1"}
	p.keys = []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: p.kid, Algorithm: string(jose.RS256), Use: "sig"}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: p.keys})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *idp) setDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

type claims struct {
	jwt.Claims
	Type          string `json:"typ,omitempty"`
	AZP           string `json:"azp,omitempty"`
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"email_verified,omitempty"`
	Name          string `json:"name,omitempty"`
}

func validClaims() claims {
	now := time.Now()
	return claims{Claims: jwt.Claims{Issuer: issuer, Subject: "user-1", Audience: jwt.Audience{"account", audience},
		Expiry: jwt.NewNumericDate(now.Add(5 * time.Minute)), IssuedAt: jwt.NewNumericDate(now)},
		Type: "Bearer", AZP: "infrapilot-dashboard", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, c claims) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	require.NoError(t, err)
	tok, err := jwt.Signed(s).Claims(c).Serialize()
	require.NoError(t, err)
	return tok
}

func verifier(t *testing.T, p *idp, gap time.Duration) (*auth.Verifier, *auth.KeySet) {
	t.Helper()
	ks, err := auth.NewKeySet(auth.KeySetConfig{URL: p.srv.URL, MinRefreshGap: gap}, p.srv.Client(), slog.New(slog.DiscardHandler), noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	v, err := auth.NewVerifier(auth.VerifierConfig{Issuer: issuer, Audience: audience, AuthorizedParties: []string{"infrapilot-dashboard"}}, ks)
	require.NoError(t, err)
	return v, ks
}

func TestVerify(t *testing.T) {
	t.Parallel()
	p := newIDP(t)
	v, _ := verifier(t, p, time.Millisecond)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	id, err := v.Verify(t.Context(), sign(t, jose.RS256, p.rsa, p.kid, validClaims()))
	require.NoError(t, err)
	require.Equal(t, auth.Identity{Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Name: "Alice"}, id)

	mutate := func(fn func(c *claims)) claims { c := validClaims(); fn(&c); return c }
	bad := map[string]string{
		"expired":                    sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Minute)) })),
		"no expiry":                  sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Expiry = nil })),
		"wrong issuer":               sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Issuer = "http://evil/realms/infrapilot" })),
		"wrong audience":             sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Audience = jwt.Audience{"account"} })),
		"id token":                   sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Type = "ID" })),
		"no subject":                 sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.Subject = "" })),
		"not yet valid":              sign(t, jose.RS256, p.rsa, p.kid, mutate(func(c *claims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) })),
		"foreign key":                sign(t, jose.ES256, ec, p.kid, validClaims()),
		"hmac with public key bytes": sign(t, jose.HS256, []byte("public-key-as-secret-0123456789ab"), p.kid, validClaims()),
		"garbage":                    "a.b.c",
		"empty":                      "",
	}
	for name, tok := range bad {
		_, err = v.Verify(t.Context(), tok)
		require.ErrorIs(t, err, auth.ErrInvalidToken, name)
	}
	// alg "none": hand-built unsigned token.
	_, err = v.Verify(t.Context(), "eyJhbGciOiJub25lIiwia2lkIjoicnNhLTEifQ.eyJzdWIiOiJ4In0.")
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	t.Parallel()
	p := newIDP(t)
	v, _ := verifier(t, p, time.Hour)
	_, err := v.Verify(t.Context(), sign(t, jose.RS256, p.rsa, p.kid, validClaims()))
	require.NoError(t, err)
	before := p.hits.Load()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			_, _ = v.Verify(t.Context(), sign(t, jose.RS256, other, "random-"+string(rune('a'+i%26)), validClaims()))
		})
	}
	wg.Wait()
	require.LessOrEqual(t, p.hits.Load()-before, int32(1), "forged key ids must not become load on the identity provider")
}

func TestKeyRotationIsPickedUp(t *testing.T) {
	t.Parallel()
	p := newIDP(t)
	v, _ := verifier(t, p, time.Millisecond)
	_, err := v.Verify(t.Context(), sign(t, jose.RS256, p.rsa, p.kid, validClaims()))
	require.NoError(t, err)
	next, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p.mu.Lock()
	p.keys = append(p.keys, jose.JSONWebKey{Key: &next.PublicKey, KeyID: "rsa-2", Algorithm: string(jose.RS256), Use: "sig"})
	p.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	_, err = v.Verify(t.Context(), sign(t, jose.RS256, next, "rsa-2", validClaims()))
	require.NoError(t, err, "a new key id triggers a refresh")
}

// TestF02_KeycloakDown_CachedKeysKeepValidating: once loaded, keys stay usable while the identity provider is
// down; before the first load the gateway reports keys unavailable (503), not invalid (401).
func TestF02_KeycloakDown_CachedKeysKeepValidating(t *testing.T) {
	t.Parallel()
	p := newIDP(t)
	p.setDown(true)
	v, ks := verifier(t, p, time.Millisecond)
	tok := sign(t, jose.RS256, p.rsa, p.kid, validClaims())
	_, err := v.Verify(t.Context(), tok)
	require.ErrorIs(t, err, auth.ErrKeysUnavailable)
	require.False(t, ks.Loaded())

	p.setDown(false)
	time.Sleep(2 * time.Millisecond)
	_, err = v.Verify(t.Context(), tok)
	require.NoError(t, err)

	p.setDown(true)
	time.Sleep(2 * time.Millisecond)
	_, err = v.Verify(t.Context(), tok)
	require.NoError(t, err, "existing tokens keep validating while the identity provider is down")
	unknown, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	_, err = v.Verify(t.Context(), sign(t, jose.RS256, unknown, "rsa-9", validClaims()))
	require.ErrorIs(t, err, auth.ErrInvalidToken, "unknown keys stay invalid, the cached set is kept")
	require.True(t, ks.Loaded())
}

func TestKeySet_RunRefreshesUntilCancelled(t *testing.T) {
	t.Parallel()
	p := newIDP(t)
	_, ks := verifier(t, p, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ks.Run(ctx) }()
	require.Eventually(t, ks.Loaded, 2*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}
