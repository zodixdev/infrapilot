package tlsreload_test

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/internal/tlsreload"
	"github.com/zodixdev/infrapilot/pkg/testutil/certtest"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, b, 0o600))
}

func TestMaterial_FollowsRotationAndKeepsLastGood(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	first, second := ca.Issue("first"), ca.Issue("second")
	dir := t.TempDir()
	certFile, keyFile := dir+"/tls.crt", dir+"/tls.key"
	copyFile(t, first.Cert, certFile)
	copyFile(t, first.Key, keyFile)

	m, err := tlsreload.Load(certFile, keyFile, ca.File)
	require.NoError(t, err)
	cert, pool := m.Current()
	require.NotNil(t, pool)
	initial := cert.Certificate[0]

	// Rotate in place (new mtime), then wait past the 1 s re-check interval.
	copyFile(t, second.Cert, certFile)
	copyFile(t, second.Key, keyFile)
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(certFile, future, future))
	require.Eventually(t, func() bool {
		c, _ := m.Current()
		return !bytes.Equal(c.Certificate[0], initial)
	}, 5*time.Second, 100*time.Millisecond, "rotated certificate is picked up")
	rotated, _ := m.Current()

	// A half-written rotation (garbage) keeps the last good material.
	require.NoError(t, os.WriteFile(certFile, []byte("not a certificate"), 0o600))
	later := future.Add(2 * time.Second)
	require.NoError(t, os.Chtimes(certFile, later, later))
	time.Sleep(1100 * time.Millisecond)
	still, _ := m.Current()
	require.Equal(t, rotated.Certificate[0], still.Certificate[0])
}

func TestLoad_Validation(t *testing.T) {
	t.Parallel()
	_, err := tlsreload.Load("cert-only.crt", "", "")
	require.ErrorContains(t, err, "set together")
	_, err = tlsreload.Load("", "", "/does/not/exist")
	require.ErrorContains(t, err, "read CA")

	ca := certtest.NewCA(t)
	m, err := tlsreload.Load("", "", ca.File)
	require.NoError(t, err)
	cert, pool := m.Current()
	require.Nil(t, cert, "CA-only material has no certificate")
	require.NotNil(t, pool)
}

func TestNotAfter_FollowsRotationWithoutHandshakes(t *testing.T) {
	t.Parallel()
	ca := certtest.NewCA(t)
	first := ca.Issue("a")
	dir := t.TempDir()
	certFile, keyFile := dir+"/tls.crt", dir+"/tls.key"
	copyFile(t, first.Cert, certFile)
	copyFile(t, first.Key, keyFile)
	m, err := tlsreload.Load(certFile, keyFile, "")
	require.NoError(t, err)
	before := m.NotAfter()
	time.Sleep(1100 * time.Millisecond) // validity has one-second resolution: the next certificate expires later
	third := ca.Issue("c")
	copyFile(t, third.Cert, certFile)
	copyFile(t, third.Key, keyFile)
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(certFile, future, future))
	require.Eventually(t, func() bool { return m.NotAfter().After(before) }, 5*time.Second, 100*time.Millisecond,
		"the expiry gauge must not report a stale certificate")
}
