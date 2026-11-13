//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// keycloakImage matches the compose core profile.
const keycloakImage = "quay.io/keycloak/keycloak:26.4.5"

// keycloak runs the real realm file (deploy/keycloak) plus a test-only confidential client with password grant,
// which never exists in the committed realm.
type keycloak struct {
	c                   testcontainers.Container
	base                string // http://host:port
	adminUser, adminPwd string
	clientSecret        string
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func startKeycloak(t *testing.T) *keycloak {
	t.Helper()
	realm, err := filepath.Abs("../../deploy/keycloak/infrapilot-realm.json")
	require.NoError(t, err)
	k := &keycloak{adminUser: "admin-" + randomHex(t, 4), adminPwd: randomHex(t, 16)}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	c, err := testcontainers.Run(ctx, keycloakImage,
		testcontainers.WithCmd("start-dev", "--http-port=8080", "--import-realm"),
		testcontainers.WithEnv(map[string]string{
			"KC_BOOTSTRAP_ADMIN_USERNAME": k.adminUser, "KC_BOOTSTRAP_ADMIN_PASSWORD": k.adminPwd,
			"KC_HEALTH_ENABLED": "true", "JAVA_OPTS_KC_HEAP": "-Xms256m -Xmx512m",
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{HostFilePath: realm, ContainerFilePath: "/opt/keycloak/data/import/infrapilot-realm.json", FileMode: 0o644}),
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/realms/infrapilot/.well-known/openid-configuration").WithPort("8080/tcp").
			WithStartupTimeout(3*time.Minute)),
	)
	require.NoError(t, err, "start keycloak")
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	ep, err := c.PortEndpoint(ctx, "8080/tcp", "http")
	require.NoError(t, err)
	k.c, k.base = c, strings.Replace(ep, "localhost", "127.0.0.1", 1)
	k.setupTestClient(t)
	return k
}

func (k *keycloak) issuer() string  { return k.base + "/realms/infrapilot" }
func (k *keycloak) jwksURL() string { return k.issuer() + "/protocol/openid-connect/certs" }

func (k *keycloak) adminToken(t *testing.T) string {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {k.adminUser}, "password": {k.adminPwd}}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	k.post(t, k.base+"/realms/master/protocol/openid-connect/token", "", form, &out, http.StatusOK)
	return out.AccessToken
}

// setupTestClient adds the confidential "infrapilot-e2e" client with direct grants (tests only).
func (k *keycloak) setupTestClient(t *testing.T) {
	t.Helper()
	tok := k.adminToken(t)
	k.clientSecret = randomHex(t, 16)
	k.adminJSON(t, tok, http.MethodPost, "/admin/realms/infrapilot/clients", map[string]any{
		"clientId": "infrapilot-e2e", "enabled": true, "publicClient": false, "secret": k.clientSecret,
		"standardFlowEnabled": false, "directAccessGrantsEnabled": true, "fullScopeAllowed": false,
		"defaultClientScopes": []string{"basic", "profile", "email", "infrapilot-api"},
	}, http.StatusCreated)
}

// createUser creates a user with a verified e-mail and returns its password.
func (k *keycloak) createUser(t *testing.T, name string) string {
	t.Helper()
	pwd := "Pw-" + randomHex(t, 12)
	k.adminJSON(t, k.adminToken(t), http.MethodPost, "/admin/realms/infrapilot/users", map[string]any{
		"username": name, "email": name + "@example.com", "emailVerified": true, "enabled": true, "firstName": name, "lastName": "Test",
		"requiredActions": []string{},
		"credentials":     []map[string]any{{"type": "password", "value": pwd, "temporary": false}},
	}, http.StatusCreated)
	return pwd
}

// token signs a user in with the test client.
func (k *keycloak) token(t *testing.T, name, pwd string) string {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"infrapilot-e2e"}, "client_secret": {k.clientSecret},
		"username": {name}, "password": {pwd}, "scope": {"openid"}}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	k.post(t, k.issuer()+"/protocol/openid-connect/token", "", form, &out, http.StatusOK)
	require.NotEmpty(t, out.AccessToken)
	return out.AccessToken
}

func (k *keycloak) post(t *testing.T, u, bearer string, form url.Values, out any, want int) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, u, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	k.do(t, req, bearer, out, want)
}

func (k *keycloak) adminJSON(t *testing.T, bearer, method, path string, body any, want int) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), method, k.base+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	k.do(t, req, bearer, nil, want)
}

func (k *keycloak) do(t *testing.T, req *http.Request, bearer string, out any, want int) {
	t.Helper()
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, want, resp.StatusCode, "%s %s: %s", req.Method, req.URL.Path, b)
	if out != nil {
		require.NoError(t, json.Unmarshal(b, out))
	}
}

// stop and start the container (F02). The published port changes on restart, so tests that restart Keycloak
// only verify behavior while it is down.
func (k *keycloak) stop(t *testing.T) {
	t.Helper()
	out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "stop", "-t", "2", k.c.GetContainerID()).CombinedOutput()
	require.NoError(t, err, "docker stop: %s", out)
}
