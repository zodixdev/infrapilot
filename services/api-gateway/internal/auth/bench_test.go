package auth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// BenchmarkVerify measures bearer-token verification with a warm key set: the per-request CPU cost of user
// authentication at the gateway (signature check dominates).
func BenchmarkVerify(b *testing.B) {
	p := newIDP(b)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(b, err)
	p.mu.Lock()
	p.keys = append(p.keys, jose.JSONWebKey{Key: &ec.PublicKey, KeyID: "ec-1", Algorithm: string(jose.ES256), Use: "sig"})
	p.mu.Unlock()
	v, _ := verifier(b, p, time.Millisecond)
	for _, tc := range []struct {
		name string
		tok  string
	}{
		{"RS256", sign(b, jose.RS256, p.rsa, p.kid, validClaims())},
		{"ES256", sign(b, jose.ES256, ec, "ec-1", validClaims())},
	} {
		b.Run(tc.name, func(b *testing.B) {
			_, err := v.Verify(b.Context(), tc.tok) // warms the key set
			require.NoError(b, err)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := v.Verify(b.Context(), tc.tok); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
