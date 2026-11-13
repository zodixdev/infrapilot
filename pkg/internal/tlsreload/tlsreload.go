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

	onError func(error)

	mu       sync.Mutex
	cert     tls.Certificate
	notAfter time.Time
	pool     *x509.CertPool
	mtimes   [3]time.Time
	checked  time.Time
}

// Option configures Load.
type Option func(*Material)

// OnError is called (outside any hot path guarantee, under the material lock) when a changed file cannot be
// loaded; the previous material stays in use. Use it to log: a stuck rotation is otherwise invisible until the
// certificate expires.
func OnError(fn func(error)) Option { return func(m *Material) { m.onError = fn } }

// Load reads the files once (fail fast on unreadable material). certFile/keyFile may be empty when only a CA is
// needed (TLS client without a client certificate); caFile may be empty when no peer verification pool is needed.
func Load(certFile, keyFile, caFile string, opts ...Option) (*Material, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("tlsreload: certificate and key files must be set together")
	}
	m := &Material{certFile: certFile, keyFile: keyFile, caFile: caFile, onError: func(error) {}}
	for _, o := range opts {
		o(m)
	}
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
			if err := m.reloadLocked(); err != nil {
				m.onError(err) // keep serving the last good material
			}
		}
	}
	if m.certFile == "" {
		return nil, m.pool
	}
	cert := m.cert
	return &cert, m.pool
}

// NotAfter is the expiry of the current certificate (zero when none is configured).
func (m *Material) NotAfter() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notAfter
}

func (m *Material) reloadLocked() error {
	// Record modification times before reading: if a rotation lands between the stat and the reads, the next
	// check sees a newer mtime and reloads again instead of pinning stale content forever.
	mtimes := m.stat()
	var cert tls.Certificate
	var notAfter time.Time
	if m.certFile != "" {
		c, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
		if err != nil {
			return fmt.Errorf("tlsreload: load key pair: %w", err)
		}
		cert = c
		if len(c.Certificate) > 0 {
			leaf, err := x509.ParseCertificate(c.Certificate[0])
			if err != nil {
				return fmt.Errorf("tlsreload: parse certificate: %w", err)
			}
			notAfter = leaf.NotAfter
		}
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
	m.cert, m.notAfter, m.pool, m.mtimes = cert, notAfter, pool, mtimes
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
