package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
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
	m := &tlsMaterial{certFile: cfg.TLSCertFile, keyFile: cfg.TLSKeyFile, caFile: cfg.TLSClientCAFile}
	if err := m.reload(); err != nil { // fail fast on unreadable material
		return nil, err
	}
	base := &tls.Config{MinVersion: tls.VersionTLS13}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		// Per handshake, return a config built from cached material; files are re-read only when their
		// modification time changes (cert-manager rotates them in place), and the last good material is kept
		// if a rotation is caught half-written.
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert, pool := m.current()
			c := base.Clone()
			c.Certificates = []tls.Certificate{cert}
			if pool != nil {
				c.ClientCAs = pool
				c.ClientAuth = tls.RequireAndVerifyClientCert
			}
			return c, nil
		},
	}), nil
}

type tlsMaterial struct {
	certFile, keyFile, caFile string

	mu      sync.Mutex
	cert    tls.Certificate
	pool    *x509.CertPool
	mtimes  [3]time.Time
	checked time.Time
}

// current returns cached material, re-checking file mtimes at most once per second.
func (m *tlsMaterial) current() (tls.Certificate, *x509.CertPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.checked) >= time.Second {
		m.checked = time.Now()
		if m.changed() {
			_ = m.reloadLocked() // on error keep serving the last good material
		}
	}
	return m.cert, m.pool
}

func (m *tlsMaterial) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadLocked()
}

func (m *tlsMaterial) reloadLocked() error {
	cert, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		return fmt.Errorf("grpcserver: load key pair: %w", err)
	}
	var pool *x509.CertPool
	if m.caFile != "" {
		pem, err := os.ReadFile(m.caFile)
		if err != nil {
			return fmt.Errorf("grpcserver: read client CA: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("grpcserver: client CA file contains no certificates")
		}
	}
	m.cert, m.pool, m.mtimes = cert, pool, m.stat()
	return nil
}

func (m *tlsMaterial) stat() [3]time.Time {
	var out [3]time.Time
	for i, f := range []string{m.certFile, m.keyFile, m.caFile} {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(f); err == nil {
			out[i] = fi.ModTime()
		}
	}
	return out
}

func (m *tlsMaterial) changed() bool { return m.stat() != m.mtimes }
