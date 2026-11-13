// Package certtest issues short-lived test certificates from an in-memory CA, written as PEM files, so tests
// exercise real (m)TLS with workload identities in URI SANs (ADR-0025).
package certtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// CA is a test certificate authority.
type CA struct {
	t      testing.TB
	dir    string
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	File   string // PEM bundle with the CA certificate
	serial atomic.Int64
}

// Files are the PEM files of an issued certificate.
type Files struct {
	Cert, Key string
}

// NewCA creates a CA whose files live in a test temp dir.
func NewCA(tb testing.TB) *CA {
	tb.Helper()
	t := tb
	ca := &CA{t: t, dir: t.TempDir()}
	ca.serial.Store(time.Now().UnixNano())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(ca.serial.Add(1)),
		Subject:               pkix.Name{CommonName: "infrapilot-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(2 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca.cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	ca.key = key
	ca.File = filepath.Join(ca.dir, "ca.crt")
	require.NoError(t, os.WriteFile(ca.File, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return ca
}

// SPIFFE returns the workload identity URI used by InfraPilot (ADR-0025).
func SPIFFE(namespace, serviceAccount string) string {
	return "spiffe://infrapilot.local/ns/" + namespace + "/sa/" + serviceAccount
}

// Issue writes a leaf certificate valid for localhost/127.0.0.1 (server and client auth) carrying the given
// URI SANs, and returns its files.
func (ca *CA) Issue(name string, uris ...string) Files {
	ca.t.Helper()
	return ca.IssueWithUsage(name, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, uris...)
}

// IssueWithUsage is Issue with explicit extended key usages (e.g. client auth only, to prove a server
// certificate check).
func (ca *CA) IssueWithUsage(name string, usage []x509.ExtKeyUsage, uris ...string) Files {
	ca.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(ca.t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serial.Add(1)),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  usage,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	for _, u := range uris {
		parsed, parseErr := url.Parse(u)
		require.NoError(ca.t, parseErr)
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(ca.t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(ca.t, err)
	f := Files{Cert: filepath.Join(ca.dir, name+".crt"), Key: filepath.Join(ca.dir, name+".key")}
	require.NoError(ca.t, os.WriteFile(f.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(ca.t, os.WriteFile(f.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return f
}
