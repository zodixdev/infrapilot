package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
)

// closeSignal reports when a response body is closed, i.e. when the KeySet has finished reading the JWKS.
type closeSignal struct {
	io.ReadCloser
	once sync.Once
	done chan struct{}
}

func (c *closeSignal) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

type firstBodyTransport struct {
	base  http.RoundTripper
	calls atomic.Int32
	first *closeSignal
}

func (f *firstBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	first := f.calls.Add(1) == 1 // counted on entry: the held request is the first one sent, not the first answered
	resp, err := f.base.RoundTrip(r)
	if err == nil && first {
		f.first.ReadCloser = resp.Body
		resp.Body = f.first
	}
	return resp, err
}

// TestKeySet_OlderFetchDoesNotReplaceNewerSet: the startup/interval fetch and an unknown-kid fetch overlap; the
// older fetch finishes last with the pre-rotation key set. It must not replace the newer set, otherwise tokens
// signed with the rotated key are rejected (and, with the identity provider down, stay rejected).
func TestKeySet_OlderFetchDoesNotReplaceNewerSet(t *testing.T) {
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwk := func(k *rsa.PrivateKey, kid string) jose.JSONWebKey {
		return jose.JSONWebKey{Key: &k.PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}
	}

	var (
		mu       sync.Mutex
		keys     = []jose.JSONWebKey{jwk(oldKey, "old")}
		down     bool
		requests atomic.Int32
	)
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		snapshot, isDown := keys, down
		mu.Unlock()
		if requests.Add(1) == 1 { // the startup fetch: answer with the key set as of its arrival, but late
			close(firstArrived)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		} else if isDown {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: snapshot})
	}))
	t.Cleanup(srv.Close)

	tr := &firstBodyTransport{base: srv.Client().Transport, first: &closeSignal{done: make(chan struct{})}}
	ks, err := auth.NewKeySet(auth.KeySetConfig{URL: srv.URL, MinRefreshGap: time.Millisecond, RefreshInterval: time.Hour},
		&http.Client{Transport: tr}, slog.New(slog.DiscardHandler), noop.NewMeterProvider().Meter("t"))
	require.NoError(t, err)
	v, err := auth.NewVerifier(auth.VerifierConfig{Issuer: issuer, Audience: audience, AuthorizedParties: []string{"infrapilot-dashboard"}}, ks)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- ks.Run(ctx) }()

	select {
	case <-firstArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("startup fetch never reached the identity provider")
	}
	// Rotation while the startup fetch is in flight.
	mu.Lock()
	keys = []jose.JSONWebKey{jwk(newKey, "new")}
	mu.Unlock()
	tok := sign(t, jose.RS256, newKey, "new", validClaims())
	require.Eventually(t, func() bool {
		_, verr := v.Verify(t.Context(), tok)
		return verr == nil
	}, 5*time.Second, 2*time.Millisecond, "an unknown-kid fetch installs the rotated key set")

	// The identity provider goes down, then the stale startup response arrives.
	mu.Lock()
	down = true
	mu.Unlock()
	close(releaseFirst)
	select {
	case <-tr.first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("startup fetch never completed")
	}
	// Run returns only after its in-flight refresh has made its install decision.
	cancel()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}

	_, err = v.Verify(t.Context(), tok)
	require.NoError(t, err, "the older fetch must not replace the newer key set")
	_, err = v.Verify(t.Context(), sign(t, jose.RS256, oldKey, "old", validClaims()))
	require.ErrorIs(t, err, auth.ErrInvalidToken, "the rotated-out key stays unknown")
}
