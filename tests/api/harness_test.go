//go:build integration

// Package api_test runs the public API end to end: both binaries built from source, real PostgreSQL, Redis and
// Keycloak (the committed realm), mTLS between gateway and control-plane. No mocks (AGENTS.md §10).
package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/zodixdev/infrapilot/pkg/testutil/certtest"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
)

var bins struct{ controlPlane, gateway string }

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "infrapilot-api-bins-")
	if err != nil {
		panic(err)
	}
	build := func(pkgDir, name string) string {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, "./cmd/"+name)
		cmd.Dir = pkgDir
		if b, err := cmd.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("build %s: %v\n%s", name, err, b))
		}
		return out
	}
	bins.controlPlane = build("../../services/control-plane", "control-plane")
	bins.gateway = build("../../services/api-gateway", "api-gateway")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// process is a supervised service binary whose output is kept for diagnostics.
type process struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	logs bytes.Buffer
	done chan struct{}
}

func (p *process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.Write(b)
}

func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.String()
}

func run(t *testing.T, bin string, env map[string]string, args ...string) *process {
	t.Helper()
	p := &process{done: make(chan struct{})}
	p.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), bin, args...)
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	p.cmd.Stdout, p.cmd.Stderr = p, p
	require.NoError(t, p.cmd.Start())
	go func() { _ = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("--- %s output ---\n%s", filepath.Base(bin), tail(p.output(), 60))
		}
	})
	return p
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return addr.Port
}

func waitReady(t *testing.T, p *process, adminPort int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", adminPort)
	require.Eventually(t, func() bool {
		select {
		case <-p.done:
			t.Fatalf("process exited:\n%s", tail(p.output(), 40))
		default:
		}
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 60*time.Second, 100*time.Millisecond, "service did not become ready")
}

// stack is a running gateway + control-plane with their dependencies.
type stack struct {
	t          *testing.T
	pg         *pgtest.DB
	redis      *tcredis.RedisContainer
	kc         *keycloak
	base       string // gateway public URL
	adminURL   string // gateway admin URL (/metrics)
	gatewayEnv map[string]string
	gateway    *process
}

type stackOptions struct {
	anonymousRPS, anonymousBurst string
}

func startStack(t *testing.T, opt stackOptions) *stack {
	t.Helper()
	s := &stack{t: t, pg: pgtest.Start(t), kc: startKeycloak(t)}
	ctx := t.Context()
	var err error
	s.redis, err = tcredis.Run(ctx, "redis:7.4-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(s.redis) })
	redisAddr, err := s.redis.Endpoint(ctx, "")
	require.NoError(t, err)

	ca := certtest.NewCA(t)
	cpID, gwID := certtest.SPIFFE("infrapilot", "control-plane"), certtest.SPIFFE("infrapilot", "api-gateway")
	cpCert, gwCert := ca.Issue("control-plane", cpID), ca.Issue("api-gateway", gwID)
	common := map[string]string{"ENVIRONMENT": "test", "LOG_LEVEL": "info", "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s"}

	mig := run(t, bins.controlPlane, merge(common, map[string]string{"SERVICE_NAME": "control-plane", "MIGRATION_DATABASE_URL": s.pg.MigratorURL}), "migrate")
	<-mig.done
	require.Equal(t, 0, mig.cmd.ProcessState.ExitCode(), "migrate failed:\n%s", mig.output())

	cpGRPC, cpAdmin := freePort(t), freePort(t)
	cp := run(t, bins.controlPlane, merge(common, map[string]string{
		"SERVICE_NAME": "control-plane", "DATABASE_URL": s.pg.AppURL, "EDGE_IDENTITIES": gwID,
		"GRPC_ADDR": fmt.Sprintf("127.0.0.1:%d", cpGRPC), "ADMIN_ADDR": fmt.Sprintf("127.0.0.1:%d", cpAdmin), "HTTP_ADDR": "127.0.0.1:0",
		"GRPC_TLS_CERT_FILE": cpCert.Cert, "GRPC_TLS_KEY_FILE": cpCert.Key, "GRPC_TLS_CLIENT_CA_FILE": ca.File,
	}))
	waitReady(t, cp, cpAdmin)

	gwHTTP, gwAdmin := freePort(t), freePort(t)
	s.gatewayEnv = merge(common, map[string]string{
		"SERVICE_NAME": "api-gateway", "HTTP_ADDR": fmt.Sprintf("127.0.0.1:%d", gwHTTP), "ADMIN_ADDR": fmt.Sprintf("127.0.0.1:%d", gwAdmin),
		"GRPC_DISABLED": "true",
		"OIDC_ISSUER":   s.kc.issuer(), "OIDC_JWKS_URL": s.kc.jwksURL(),
		"REDIS_ADDR":          redisAddr,
		"CONTROLPLANE_TARGET": fmt.Sprintf("127.0.0.1:%d", cpGRPC), "CONTROLPLANE_SERVER_NAME": "localhost",
		"CONTROLPLANE_SERVER_IDENTITY": cpID, "CONTROLPLANE_TLS_CERT_FILE": gwCert.Cert, "CONTROLPLANE_TLS_KEY_FILE": gwCert.Key,
		"CONTROLPLANE_TLS_CA_FILE": ca.File,
		"CORS_ALLOWED_ORIGINS":     "http://localhost:3000",
		// Generous limits so functional tests are not throttled; dedicated tests lower them.
		"RATE_LIMIT_USER_RPS": "1000", "RATE_LIMIT_USER_BURST": "1000", "RATE_LIMIT_API_KEY_RPS": "1000", "RATE_LIMIT_API_KEY_BURST": "1000",
		"RATE_LIMIT_MEMBER_CHANGE_RPS": "1000", "RATE_LIMIT_MEMBER_CHANGE_BURST": "1000", "RATE_LIMIT_ORG_CREATE_RPS": "1000",
		"RATE_LIMIT_ORG_CREATE_BURST": "1000", "RATE_LIMIT_KEY_CREATE_RPS": "1000", "RATE_LIMIT_KEY_CREATE_BURST": "1000",
		"RATE_LIMIT_ANONYMOUS_RPS": or(opt.anonymousRPS, "1000"), "RATE_LIMIT_ANONYMOUS_BURST": or(opt.anonymousBurst, "1000"),
	})
	s.base, s.adminURL = fmt.Sprintf("http://127.0.0.1:%d/v1", gwHTTP), fmt.Sprintf("http://127.0.0.1:%d", gwAdmin)
	s.gateway = run(t, bins.gateway, s.gatewayEnv)
	waitReady(t, s.gateway, gwAdmin)
	return s
}

// restartGateway replaces the gateway process (same ports and configuration).
func (s *stack) restartGateway(t *testing.T) {
	t.Helper()
	_ = s.gateway.cmd.Process.Signal(syscall.SIGTERM)
	<-s.gateway.done
	s.gateway = run(t, bins.gateway, s.gatewayEnv)
	var port int
	_, err := fmt.Sscanf(s.adminURL, "http://127.0.0.1:%d", &port)
	require.NoError(t, err)
	waitReady(t, s.gateway, port)
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// user is a signed-in caller.
type user struct {
	name, token string
}

func (s *stack) user(t *testing.T, name string) user {
	t.Helper()
	pwd := s.kc.createUser(t, name)
	return user{name: name, token: s.kc.token(t, name, pwd)}
}

// credential is how a request authenticates.
type credential struct{ bearer, apiKey string }

func (u user) cred() credential { return credential{bearer: u.token} }
func key(k string) credential   { return credential{apiKey: k} }

var anonymous = credential{}

// response of the API.
type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, v), string(r.body))
}

func (r response) problemCode(t *testing.T) string {
	t.Helper()
	require.Equal(t, "application/problem+json", r.header.Get("Content-Type"), "status %d: %s", r.status, r.body)
	var p struct {
		Code string `json:"code"`
	}
	r.json(t, &p)
	return p.Code
}

func (s *stack) call(t *testing.T, c credential, method, path string, body any, headers ...string) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.base+path, rd)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, header: resp.Header, body: b}
}

// metric returns the sum of a counter's samples from the gateway's /metrics whose label set contains filter.
func (s *stack) metric(t *testing.T, name, filter string) float64 {
	t.Helper()
	resp, err := http.Get(s.adminURL + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var sum float64
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name) || !strings.Contains(line, filter) {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%g", &v); err == nil {
			sum += v
		}
	}
	return sum
}
