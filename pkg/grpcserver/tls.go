package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// transportCredentials builds TLS (or mTLS with a client CA) from PEM files; plaintext when no certificate
// is configured (local development and tests; config validation requires TLS in staging and prod).
func transportCredentials(cfg Config) (credentials.TransportCredentials, error) {
	if cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		if cfg.TLSClientCAFile != "" {
			return nil, errors.New("grpcserver: TLSClientCAFile requires a server certificate")
		}
		return insecure.NewCredentials(), nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
		return nil, errors.New("grpcserver: TLSCertFile and TLSKeyFile must be set together")
	}
	// Fail fast on unreadable material instead of at the first handshake.
	if _, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil {
		return nil, fmt.Errorf("grpcserver: load key pair: %w", err)
	}
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Re-read on each handshake: cert-manager rotates the mounted files in place.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
			if err != nil {
				return nil, fmt.Errorf("grpcserver: reload key pair: %w", err)
			}
			return &c, nil
		},
	}
	if cfg.TLSClientCAFile != "" {
		pem, err := os.ReadFile(cfg.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("grpcserver: read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("grpcserver: client CA file contains no certificates")
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tc), nil
}
