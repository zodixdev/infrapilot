package grpcserver

import (
	"crypto/tls"
	"errors"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zodixdev/infrapilot/pkg/internal/tlsreload"
)

// transportCredentials builds TLS (or mTLS with a client CA) from PEM files; plaintext when no certificate is
// configured (local development and tests; config validation requires TLS in staging and prod).
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
	m, err := tlsreload.Load(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile)
	if err != nil { // fail fast on unreadable material
		return nil, err
	}
	base := &tls.Config{MinVersion: tls.VersionTLS13}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		// Per handshake, return a config built from cached material that follows in-place rotation.
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := m.Current()
			c := base.Clone()
			c.Certificates = []tls.Certificate{*cert}
			if pool != nil {
				c.ClientCAs = pool
				c.ClientAuth = tls.RequireAndVerifyClientCert
			}
			return c, nil
		},
	}), nil
}
