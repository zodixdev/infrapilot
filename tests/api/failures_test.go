//go:build integration

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func dockerVerb(t *testing.T, verb, id string) {
	t.Helper()
	out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", verb, id).CombinedOutput()
	require.NoError(t, err, "docker %s: %s", verb, out)
}

// TestF03_RedisUnavailable_Integration: rate limiting fails open with a metric, API keys and permission
// decisions are resolved through control-plane, latency stays bounded, and limiting resumes after recovery.
func TestF03_RedisUnavailable_Integration(t *testing.T) {
	s := startStack(t, stackOptions{anonymousRPS: "1", anonymousBurst: "3", orgCreateRPS: "0.01", orgCreateBurst: "2", authFailureRPS: "0.1", authFailureBurst: "5"})
	w := newWorld(t, s)
	org := "/organizations/" + w.org

	t.Run("rate limiting works while Redis is up", func(t *testing.T) {
		var limited response
		for range 10 {
			if r := s.call(t, anonymous, http.MethodGet, "/health", nil); r.status == http.StatusTooManyRequests {
				limited = r
				break
			}
		}
		require.Equal(t, http.StatusTooManyRequests, limited.status, "anonymous burst of 3 must be enforced")
		require.Equal(t, "rate_limited", limited.problemCode(t))
		require.NotEmpty(t, limited.header.Get("Retry-After"))
	})

	t.Run("sensitive operations have their own budget", func(t *testing.T) {
		u := s.user(t, "creator")
		codes := make([]int, 0, 4)
		for i := range 4 {
			codes = append(codes, s.call(t, u.cred(), http.MethodPost, "/organizations", map[string]any{"name": fmt.Sprintf("o%d", i)}).status)
		}
		require.Equal(t, []int{http.StatusCreated, http.StatusCreated, http.StatusTooManyRequests, http.StatusTooManyRequests}, codes)
	})

	t.Run("credential guessing exhausts a per-client budget before any lookup", func(t *testing.T) {
		var last response
		for i := range 8 {
			last = s.call(t, key(fmt.Sprintf("ipk_%043d", i)), http.MethodGet, org, nil)
		}
		require.Equal(t, http.StatusTooManyRequests, last.status, "after the failure budget the client is refused outright")
		require.NotEmpty(t, last.header.Get("Retry-After"))
	})

	dockerVerb(t, "pause", s.redis.GetContainerID())
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "unpause", s.redis.GetContainerID()).Run()
	})
	before := s.metric(t, "gateway_ratelimit_bypassed_total", "")

	t.Run("requests are served while Redis is down", func(t *testing.T) {
		newKey := w.newKey(t, w.project, "read") // never cached: must be resolved through control-plane
		for i := range 20 {
			start := time.Now()
			require.Equal(t, http.StatusOK, s.call(t, w.viewer.cred(), http.MethodGet, org+"/projects", nil).status, "user request %d", i)
			require.Equal(t, http.StatusOK, s.call(t, key(newKey), http.MethodGet, org+"/projects/"+w.project+"/resources", nil).status, "key request %d", i)
			require.Equal(t, http.StatusOK, s.call(t, anonymous, http.MethodGet, "/health", nil).status, "anonymous limits are not enforced (fail open)")
			require.Less(t, time.Since(start), 2*time.Second, "the breaker keeps Redis timeouts off the request path")
		}
		require.Equal(t, http.StatusForbidden, s.call(t, w.viewer.cred(), http.MethodPost, org+"/projects", map[string]any{"name": "x", "slug": "nope"}).status,
			"authorization still applies without the cache")
		require.Greater(t, s.metric(t, "gateway_ratelimit_bypassed_total", ""), before, "fail-open is counted for the alert")
	})

	dockerVerb(t, "unpause", s.redis.GetContainerID())
	t.Run("limits apply again after recovery", func(t *testing.T) {
		require.Eventually(t, func() bool {
			return s.call(t, anonymous, http.MethodGet, "/health", nil).status == http.StatusTooManyRequests
		}, 20*time.Second, 50*time.Millisecond)
	})
}

// TestF02_KeycloakUnavailable_Integration: tokens issued before the outage keep validating from the cached JWKS;
// a gateway that never loaded the keys answers 503 (not 401) for tokens and still serves API keys.
func TestF02_KeycloakUnavailable_Integration(t *testing.T) {
	s := startStack(t, stackOptions{})
	w := newWorld(t, s)
	org := "/organizations/" + w.org
	require.Equal(t, http.StatusOK, s.call(t, w.owner.cred(), http.MethodGet, org, nil).status)

	s.kc.stop(t)
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.kc.issuer()+"/.well-known/openid-configuration", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(probe)
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "new logins are impossible while the identity provider is down")

	for _, u := range []user{w.owner, w.viewer, w.admin} {
		require.Equal(t, http.StatusOK, s.call(t, u.cred(), http.MethodGet, org, nil).status, "existing tokens keep working (cached JWKS)")
	}

	s.restartGateway(t)
	r := s.call(t, w.owner.cred(), http.MethodGet, org, nil)
	require.Equal(t, http.StatusServiceUnavailable, r.status, "without any keys the gateway cannot judge a token: 503, not 401")
	require.Equal(t, "unavailable", r.problemCode(t))
	require.NotEmpty(t, r.header.Get("Retry-After"))
	require.Equal(t, http.StatusOK, s.call(t, key(w.readKey), http.MethodGet, org+"/projects/"+w.project, nil).status,
		"API keys do not depend on the identity provider")
	require.Positive(t, s.metric(t, "gateway_jwks_refresh_total", `result="error"`), "refresh failures are visible")
}
