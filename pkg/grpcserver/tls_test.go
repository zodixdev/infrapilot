package grpcserver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/zodixdev/infrapilot/pkg/grpcserver"
)

type certPair struct{ cert, key string }

// issue writes a PEM certificate signed by parent (self-signed when parent is nil) and returns its files.
func issue(t *testing.T, dir, name string, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (certPair, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isCA {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	p := certPair{filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")}
	require.NoError(t, os.WriteFile(p.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(p.key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return p, cert, key
}

func TestServer_TLSAndMutualTLS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caFiles, ca, caKey := issue(t, dir, "ca", true, nil, nil)
	srvFiles, _, _ := issue(t, dir, "server", false, ca, caKey)
	cliFiles, _, _ := issue(t, dir, "client", false, ca, caKey)
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	cfg := testConfig()
	cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile = srvFiles.cert, srvFiles.key, caFiles.cert
	s, err := grpcserver.New(t.Context(), cfg)
	require.NoError(t, err)
	s.SetServing(true)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.WithoutCancel(t.Context())) }()
	t.Cleanup(func() {
		require.NoError(t, s.Shutdown(context.WithoutCancel(t.Context())))
		require.NoError(t, <-runErr)
	})

	check := func(tc *tls.Config) error {
		conn, err := grpc.NewClient(s.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tc)))
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, err = healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
		return err
	}

	require.Error(t, check(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}),
		"mTLS: a client without a certificate is rejected")

	clientCert, loadErr := tls.LoadX509KeyPair(cliFiles.cert, cliFiles.key)
	require.NoError(t, loadErr)
	require.NoError(t, check(&tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert}}), "a client with a CA-signed certificate is accepted")
}

func TestNew_TLSValidation(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.TLSCertFile = "/does/not/exist.crt"
	cfg.TLSKeyFile = "/does/not/exist.key"
	_, err := grpcserver.New(t.Context(), cfg)
	require.ErrorContains(t, err, "load key pair")

	cfg = testConfig()
	cfg.TLSClientCAFile = "/ca.crt"
	_, err = grpcserver.New(t.Context(), cfg)
	require.ErrorContains(t, err, "requires a server certificate")

	cfg = testConfig()
	cfg.MaxConcurrentStreams = 0
	_, err = grpcserver.New(t.Context(), cfg)
	require.ErrorContains(t, err, "MaxConcurrentStreams")
}
