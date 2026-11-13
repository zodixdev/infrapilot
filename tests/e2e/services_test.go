//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	servicesNS = env("SERVICES_NAMESPACE", "infrapilot")
	depsNS     = env("DEPS_NAMESPACE", "infrapilot-deps")
	gatewayURL = env("GATEWAY_URL", "http://127.0.0.1:28080/v1")
	keycloak   = env("KEYCLOAK_URL", "http://localhost:28180")
)

// eventDriven metrics appear only when their event happens (fail-open, 429, peer denial, retention runs);
// integration tests prove they are emitted (F03 suite, peer policy tests) and the purge metrics are checked below
// after a manual run.
var eventDriven = map[string]bool{
	"gateway_ratelimit_bypassed_total": true, "gateway_ratelimit_limited_total": true, "gateway_redis_errors_total": true,
	"grpc_server_peer_denied_total": true, "controlplane_purge_rows_total": true, "controlplane_purge_last_success_timestamp_seconds": true,
}

func kube(t *testing.T, ns string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--context", kubeContext, "-n", ns}, args...)...).CombinedOutput()
	require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func httpDo(t *testing.T, method, rawURL, bearer string, form url.Values, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader = http.NoBody
	ct := ""
	switch {
	case form != nil:
		rd, ct = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	case body != nil:
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd, ct = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, rd)
	require.NoError(t, err)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, b
}

func field(t *testing.T, b []byte, key string) string {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m), string(b))
	v, _ := m[key].(string)
	return v
}

// userToken signs a fresh verified user in through the test-only password-grant client.
func userToken(t *testing.T) string {
	t.Helper()
	adminPwd := kube(t, depsNS, "get", "secret", "deps-credentials", "-o", "jsonpath={.data.keycloak-admin-password}")
	decoded, err := exec.CommandContext(t.Context(), "sh", "-c", "printf %s \"$0\" | base64 -d", adminPwd).Output()
	require.NoError(t, err)
	st, b := httpDo(t, http.MethodPost, keycloak+"/realms/master/protocol/openid-connect/token", "",
		url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {string(decoded)}}, nil)
	require.Equal(t, http.StatusOK, st, string(b))
	admin := field(t, b, "access_token")

	secret := fmt.Sprintf("s%d", time.Now().UnixNano())
	st, _ = httpDo(t, http.MethodPost, keycloak+"/admin/realms/infrapilot/clients", admin, nil, map[string]any{
		"clientId": "infrapilot-e2e", "enabled": true, "publicClient": false, "secret": secret, "standardFlowEnabled": false,
		"directAccessGrantsEnabled": true, "fullScopeAllowed": false, "defaultClientScopes": []string{"basic", "profile", "email", "infrapilot-api"},
	})
	if st == http.StatusConflict {
		st, b = httpDo(t, http.MethodGet, keycloak+"/admin/realms/infrapilot/clients?clientId=infrapilot-e2e", admin, nil, nil)
		require.Equal(t, http.StatusOK, st)
		var clients []map[string]any
		require.NoError(t, json.Unmarshal(b, &clients))
		id, _ := clients[0]["id"].(string)
		st, b = httpDo(t, http.MethodPost, keycloak+"/admin/realms/infrapilot/clients/"+id+"/client-secret", admin, nil, nil)
		require.Equal(t, http.StatusOK, st, string(b))
		secret = field(t, b, "value")
	} else {
		require.Equal(t, http.StatusCreated, st)
	}
	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	pwd := fmt.Sprintf("Pw-%d-e2e", time.Now().UnixNano())
	st, b = httpDo(t, http.MethodPost, keycloak+"/admin/realms/infrapilot/users", admin, nil, map[string]any{
		"username": name, "email": name + "@example.com", "emailVerified": true, "enabled": true, "firstName": "E2E", "lastName": "User",
		"credentials": []map[string]any{{"type": "password", "value": pwd, "temporary": false}},
	})
	require.Equal(t, http.StatusCreated, st, string(b))
	st, b = httpDo(t, http.MethodPost, keycloak+"/realms/infrapilot/protocol/openid-connect/token", "",
		url.Values{"grant_type": {"password"}, "client_id": {"infrapilot-e2e"}, "client_secret": {secret}, "username": {name}, "password": {pwd}, "scope": {"openid"}}, nil)
	require.Equal(t, http.StatusOK, st, string(b))
	return field(t, b, "access_token")
}

// TestE2E_Services: the public API works through the deployed gateway and control-plane over mTLS; one request
// produces a single trace across both services in Tempo; the metrics the dashboards and alerts use are emitted;
// the retention job runs and reports its success.
func TestE2E_Services(t *testing.T) {
	ctx := t.Context()
	token := userToken(t)

	t.Log("API round trip through the gateway")
	st, b := httpDo(t, http.MethodPost, gatewayURL+"/organizations", token, nil, map[string]any{"name": "E2E"})
	require.Equal(t, http.StatusCreated, st, string(b))
	org := field(t, b, "id")
	st, b = httpDo(t, http.MethodPost, gatewayURL+"/organizations/"+org+"/projects", token, nil, map[string]any{"name": "Web", "slug": "web"})
	require.Equal(t, http.StatusCreated, st, string(b))
	project := field(t, b, "id")
	for i := range 20 {
		st, b = httpDo(t, http.MethodPost, gatewayURL+"/organizations/"+org+"/projects/"+project+"/resources", token, nil,
			map[string]any{"kind": "host", "name": fmt.Sprintf("h%d", i)})
		require.Equal(t, http.StatusCreated, st, string(b))
		st, _ = httpDo(t, http.MethodGet, gatewayURL+"/organizations/"+org+"/projects/"+project+"/resources", token, nil, nil)
		require.Equal(t, http.StatusOK, st)
	}

	t.Log("one trace spans api-gateway and control-plane (context propagated over mTLS gRPC)")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		var res struct {
			Traces []struct {
				TraceID string `json:"traceID"`
			} `json:"traces"`
		}
		now := time.Now()
		q := `{ resource.service.name = "api-gateway" } && { resource.service.name = "control-plane" }`
		u := tempoURL + "/api/search?q=" + url.QueryEscape(q) + "&limit=5" +
			"&start=" + strconv.FormatInt(now.Add(-15*time.Minute).Unix(), 10) + "&end=" + strconv.FormatInt(now.Unix(), 10)
		require.NoError(c, getJSON(ctx, u, &res))
		require.NotEmpty(c, res.Traces)
	}, 2*time.Minute, 5*time.Second)

	t.Log("metrics used by the service dashboards and alerts are emitted")
	for job, files := range map[string][]string{
		"api-gateway":   {"prometheus/rules/api-gateway.yaml", "grafana/dashboards/api-gateway.json"},
		"control-plane": {"prometheus/rules/control-plane.yaml", "grafana/dashboards/control-plane.json"},
	} {
		assert.EventuallyWithT(t, func(c *assert.CollectT) {
			res, err := promQuery(ctx, fmt.Sprintf(`count by (__name__) ({job=%q})`, job))
			require.NoError(c, err)
			emitted := map[string]bool{}
			for _, r := range res.Data.Result {
				emitted[r.Metric["__name__"]] = true
			}
			for _, name := range referencedMetrics(t, files...) {
				if eventDriven[name] {
					continue
				}
				// Histogram buckets and gauges from other jobs (e.g. purge) are attributed per job above.
				require.True(c, emitted[name], "metric %q used for %s is not emitted", name, job)
			}
		}, 2*time.Minute, 5*time.Second)
	}

	t.Log("retention job runs as the batch role and reports success")
	job := fmt.Sprintf("purge-e2e-%d", time.Now().Unix())
	kube(t, servicesNS, "create", "job", "--from=cronjob/control-plane-purge", job)
	kube(t, servicesNS, "wait", "--for=condition=complete", "job/"+job, "--timeout=180s")
	logs := kube(t, servicesNS, "logs", "job/"+job)
	require.Contains(t, logs, "retention purge completed")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		v, err := promValue(ctx, `max(controlplane_purge_last_success_timestamp_seconds)`)
		require.NoError(c, err)
		require.Greater(c, v, float64(time.Now().Add(-10*time.Minute).Unix()))
	}, 2*time.Minute, 5*time.Second)
	kube(t, servicesNS, "delete", "job", job, "--wait=false")
}
