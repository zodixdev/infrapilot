package workloadid_test

import (
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/internal/workloadid"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	require.NoError(t, workloadid.Validate("spiffe://infrapilot.local/ns/infrapilot/sa/api-gateway"))
	for _, bad := range []string{
		"https://infrapilot.local/ns/a/sa/b",
		"spiffe://Infrapilot.local/ns/a/sa/b",
		"spiffe://infrapilot.local:443/ns/a/sa/b",
		"spiffe://user@infrapilot.local/ns/a/sa/b",
		"spiffe://infrapilot.local/ns/a/sa/b?x=1",
		"spiffe://infrapilot.local/ns/a/sa/b#frag",
		"spiffe://infrapilot.local/ns/a/sa/%62",
		"spiffe://infrapilot.local/ns//sa/b",
		"spiffe://infrapilot.local/ns/../sa/b",
		"spiffe://infrapilot.local",
		"spiffe://infrapilot.local/",
		"spiffe:opaque",
		"",
	} {
		require.Error(t, workloadid.Validate(bad), bad)
	}
}

func TestFromCertificate(t *testing.T) {
	t.Parallel()
	uri := func(s string) *url.URL {
		u, err := url.Parse(s)
		require.NoError(t, err)
		return u
	}
	good := "spiffe://infrapilot.local/ns/infrapilot/sa/control-plane"
	id, err := workloadid.FromCertificate(&x509.Certificate{URIs: []*url.URL{uri(good)}})
	require.NoError(t, err)
	require.Equal(t, good, id)

	for name, c := range map[string]*x509.Certificate{
		"nil":          nil,
		"no uri":       {},
		"two uris":     {URIs: []*url.URL{uri(good), uri("spiffe://infrapilot.local/ns/x/sa/y")}},
		"not spiffe":   {URIs: []*url.URL{uri("https://example.com/x")}},
		"bad identity": {URIs: []*url.URL{uri("spiffe://infrapilot.local/ns/a/sa/%62")}},
	} {
		_, err := workloadid.FromCertificate(c)
		require.ErrorIs(t, err, workloadid.ErrNoIdentity, name)
	}
}
