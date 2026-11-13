//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type f01Result struct {
	status      int
	problemCode string
	id          string
	took        time.Duration
	err         error
	afterRecov  bool
}

// TestF01_PostgresRestart_API_Integration: seen from the public API, a PostgreSQL restart turns in-flight and new
// requests into fast 503 problem+json responses (never 500, never a hang), the stack recovers without restarting
// any process, and every project acknowledged with 201 is present afterwards.
func TestF01_PostgresRestart_API_Integration(t *testing.T) {
	s := startStack(t, stackOptions{pgFixedPort: true})
	alice := s.user(t, "alice")
	org, _ := s.fixture(t, alice, "Acme")
	path := "/organizations/" + org + "/projects"

	client := &http.Client{}
	defer client.CloseIdleConnections()
	create := func(ctx context.Context, slug string) f01Result {
		body, _ := json.Marshal(map[string]any{"name": "p", "slug": slug})
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, bytes.NewReader(body))
		if err != nil {
			return f01Result{err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+alice.token)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return f01Result{err: err, took: time.Since(start)}
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		res := f01Result{status: resp.StatusCode, took: time.Since(start)}
		var v struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &v)
		res.id, res.problemCode = v.ID, v.Code
		if resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Content-Type") != "application/problem+json" {
			res.problemCode = "not-problem-json"
		}
		return res
	}

	var (
		mu        sync.Mutex
		results   []f01Result
		recovered bool
		wg        sync.WaitGroup
	)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	count := func(pred func(f01Result) bool) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range results {
			if pred(r) {
				n++
			}
		}
		return n
	}
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ctx.Err() == nil; i++ {
				r := create(ctx, fmt.Sprintf("p-%d-%d", w, i))
				if ctx.Err() != nil {
					return // cancelled by the test itself, not an outcome
				}
				mu.Lock()
				r.afterRecov = recovered
				results = append(results, r)
				mu.Unlock()
			}
		})
	}
	any201 := func(r f01Result) bool { return r.status == http.StatusCreated }
	require.Eventually(t, func() bool { return count(any201) >= 8 }, 30*time.Second, 20*time.Millisecond, "load is flowing before the restart")

	s.pg.Restart(t)
	require.Eventually(t, func() bool {
		return s.call(t, alice.cred(), http.MethodGet, path, nil).status == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond, "the stack recovers on its own after Postgres is back")
	mu.Lock()
	recovered = true
	mu.Unlock()
	require.Eventually(t, func() bool {
		return count(func(r f01Result) bool { return r.afterRecov && any201(r) }) >= 8
	}, 30*time.Second, 20*time.Millisecond, "writes succeed again after recovery")
	stop()
	wg.Wait()

	var ok, unavailable int
	for _, r := range results {
		require.NoError(t, r.err, "the gateway always answers (no resets, no hangs)")
		switch r.status {
		case http.StatusCreated:
			ok++
		case http.StatusServiceUnavailable:
			unavailable++
			require.Equal(t, "unavailable", r.problemCode, "503s are problem+json with code unavailable")
			require.Less(t, r.took, 3*time.Second, "failures are fast, not timeouts")
		default:
			t.Fatalf("unexpected status %d after %s (only 201 or 503 are acceptable)", r.status, r.took)
		}
	}
	require.Positive(t, unavailable, "the restart must have been observed at the API")
	for _, r := range results {
		if r.status == http.StatusCreated {
			require.Equal(t, http.StatusOK, s.call(t, alice.cred(), http.MethodGet, path+"/"+r.id, nil).status,
				"acknowledged project %s must survive the restart", r.id)
		}
	}
	t.Logf("F01 via API: %d acknowledged, %d fast 503, 0 other", ok, unavailable)
}
