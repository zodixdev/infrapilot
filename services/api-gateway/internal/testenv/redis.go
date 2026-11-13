// Package testenv starts the gateway's real dependencies for integration tests (no mocks, AGENTS.md §10).
package testenv

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
)

// RedisImage matches the compose core profile.
const RedisImage = "redis:7.4-alpine"

// Redis is a disposable Redis container.
type Redis struct {
	c    *tcredis.RedisContainer
	Addr string
}

// StartRedis launches Redis; it is terminated when the test ends.
func StartRedis(t *testing.T) *Redis {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	c, err := tcredis.Run(ctx, RedisImage)
	require.NoError(t, err, "start redis (is Docker running?)")
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(c)) })
	ep, err := c.Endpoint(ctx, "")
	require.NoError(t, err)
	return &Redis{c: c, Addr: ep}
}

// Client returns a client closed at the end of the test.
func (r *Redis) Client(t *testing.T) *redis.Client {
	t.Helper()
	c := redisx.NewClient(&redis.Options{Addr: r.Addr})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Pause freezes Redis (calls hang until their deadline), Unpause resumes it.
func (r *Redis) Pause(t *testing.T) { t.Helper(); require.NoError(t, r.docker(t, "pause")) }

// Unpause resumes a paused container; it is idempotent so it can also run from t.Cleanup.
func (r *Redis) Unpause(t *testing.T) {
	t.Helper()
	if err := r.docker(t, "unpause"); err != nil && !strings.Contains(err.Error(), "is not paused") {
		require.NoError(t, err)
	}
}

func (r *Redis) docker(t *testing.T, verb string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", verb, r.c.GetContainerID()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w: %s", verb, err, out)
	}
	return nil
}
