package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/pkg/postgres"
)

func TestConfig_Defaults(t *testing.T) {
	t.Parallel()
	c := postgres.Config{}.Defaults()
	require.Equal(t, int32(10), c.MaxConns)
	require.Equal(t, 30*time.Second, c.StatementTimeout)
	require.Equal(t, 5*time.Second, c.LockTimeout)
	require.Equal(t, 60*time.Second, c.IdleInTransactionSessionTimeout)

	custom := postgres.Config{MaxConns: 3, StatementTimeout: time.Second}.Defaults()
	require.Equal(t, int32(3), custom.MaxConns)
	require.Equal(t, time.Second, custom.StatementTimeout)
}

func TestOpen_FailsFastOnBadInput(t *testing.T) {
	t.Parallel()
	_, err := postgres.Open(t.Context(), postgres.Config{})
	require.Error(t, err)
	_, err = postgres.Open(t.Context(), postgres.Config{URL: "postgres://app:secret@localhost/x", MinConns: 5, MaxConns: 2})
	require.ErrorContains(t, err, "MinConns")
	_, err = postgres.Open(t.Context(), postgres.Config{URL: "://not a url"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}
