// Package tlsreload keeps TLS key material loaded from PEM files current: files are re-read when their
// modification time changes (cert-manager rotates them in place) and the last good material is kept if a
// rotation is caught half-written. Shared by grpcserver and grpcclient.
package tlsreload

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Material is a certificate/key pair plus an optional CA bundle, refreshed lazily.
type Material struct {
	certFile, keyFile, caFile string

	mu      sync.Mutex
	cert    tls.Certificate
	pool    *x509.CertPool
	mtimes  [3]time.Time
	checked time.Time
}

// Load reads the files once (fail fast on unreadable material). certFile/keyFile may be empty when only a CA is
// needed (TLS client without a client certificate); caFile may be empty when no peer verification pool is needed.
func Load(certFile, keyFile, caFile string) (*Material, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("tlsreload: certificate and key files must be set together")
	}
	m := &Material{certFile: certFile, keyFile: keyFile, caFile: caFile}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadLocked(); err != nil {
		return nil, err
	}
	return m, nil
}

// Current returns cached material, re-checking file mtimes at most once per second. Cert is nil when no
// certificate is configured; Pool is nil when no CA is configured.
func (m *Material) Current() (*tls.Certificate, *x509.CertPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.checked) >= time.Second {
		m.checked = time.Now()
		if m.stat() != m.mtimes {
			_ = m.reloadLocked() // on error keep serving the last good material
		}
	}
	if m.certFile == "" {
		return nil, m.pool
	}
	cert := m.cert
	return &cert, m.pool
}

func (m *Material) reloadLocked() error {
	var cert tls.Certificate
	if m.certFile != "" {
		c, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
		if err != nil {
			return fmt.Errorf("tlsreload: load key pair: %w", err)
		}
		cert = c
	}
	var pool *x509.CertPool
	if m.caFile != "" {
		pem, err := os.ReadFile(m.caFile)
		if err != nil {
			return fmt.Errorf("tlsreload: read CA: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("tlsreload: CA file contains no certificates")
		}
	}
	m.cert, m.pool, m.mtimes = cert, pool, m.stat()
	return nil
}

func (m *Material) stat() [3]time.Time {
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
