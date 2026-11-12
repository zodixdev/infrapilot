//go:build e2e

// Package e2e verifies the conformance release in kind end to end: pods ready, readiness reports the
// Postgres dependency, traffic produces traces in Tempo and metrics in Prometheus (M01 acceptance).
// Prerequisites: make up PROFILE=obs, make cluster deploy.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	kubeContext = env("KUBE_CONTEXT", "kind-infrapilot")
	namespace   = env("HELM_NAMESPACE", "infrapilot-test")
	release     = env("HELM_RELEASE", "conformance")
	promURL     = env("PROMETHEUS_URL", "http://localhost:9090")
	tempoURL    = env("TEMPO_URL", "http://localhost:3200")
	curlImage   = env("CURL_IMAGE", "curlimages/curl:8.17.0")
)

func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	full := append([]string{"--context", kubeContext, "-n", namespace}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), out)
	return string(out)
}

// inCluster runs a shell script in a throw-away curl pod inside the release namespace.
func inCluster(t *testing.T, name, script string) string {
	t.Helper()
	// The NetworkPolicy admits the admin port only from scrapers and test-labelled pods.
	return kubectl(t, "run", name, "--rm", "-i", "--restart=Never", "--quiet", "--image="+curlImage,
		"--labels=app.kubernetes.io/component=test",
		"--overrides", `{"spec":{"automountServiceAccountToken":false}}`, "--", "sh", "-ec", script)
}

func getJSON(t *testing.T, rawURL string, dst any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, dst), string(body))
}

func TestE2E_ConformanceRelease(t *testing.T) {
	t.Log("pods of the release are ready")
	kubectl(t, "rollout", "status", "deployment/"+release, "--timeout=120s")
	pods := kubectl(t, "get", "pods", "-l", "app.kubernetes.io/name=conformance", "-o", "jsonpath={range .items[*]}{.metadata.name}={.status.phase}{\"\\n\"}{end}")
	require.Contains(t, pods, "Running")
	t.Log(pods)

	t.Log("readiness document reports the postgres dependency and the migration left a probe row")
	out := inCluster(t, "e2e-readyz", fmt.Sprintf(`curl -fsS --retry 6 --retry-all-errors --retry-delay 2 --max-time 10 http://%s:8081/readyz; echo; curl -fsS --retry 6 --retry-all-errors --retry-delay 2 --max-time 10 http://%s:8080/v1/probe`, release, release))
	require.Contains(t, out, `"postgres":{"status":"up"`)
	require.Contains(t, out, `"probe_rows":`)
	require.NotContains(t, out, `"probe_rows":0`)
	t.Log(out)

	t.Log("recovery middleware turns a panic into problem+json")
	out = inCluster(t, "e2e-panic", fmt.Sprintf(`curl -sS --max-time 5 -o /dev/stdout -w ' status=%%{http_code}' http://%s:8080/v1/panic`, release))
	require.Contains(t, out, "status=500")
	require.Contains(t, out, `"code":"internal"`)

	t.Log("generate traffic")
	inCluster(t, "e2e-traffic", fmt.Sprintf(`for i in $(seq 1 30); do curl -fsS --max-time 5 -o /dev/null http://%s:8080/v1/probe; curl -fsS --max-time 5 -o /dev/null "http://%s:8080/v1/slow?ms=20"; done; echo done`, release, release))

	t.Log("metrics pushed over OTLP reach Prometheus")
	require.Eventually(t, func() bool {
		var res struct {
			Data struct {
				Result []struct {
					Value []any `json:"value"`
				} `json:"result"`
			} `json:"data"`
		}
		q := url.QueryEscape(`sum(http_server_requests_total{service_name="conformance",route="/v1/probe"})`)
		getJSON(t, promURL+"/api/v1/query?query="+q, &res)
		if len(res.Data.Result) == 0 {
			return false
		}
		v, _ := res.Data.Result[0].Value[1].(string)
		t.Logf("prometheus http_server_requests_total{/v1/probe} = %s", v)
		return v != "" && v != "0"
	}, 2*time.Minute, 5*time.Second, "conformance request metrics never appeared in Prometheus")

	t.Log("traces reach Tempo")
	require.Eventually(t, func() bool {
		var res struct {
			Traces []struct {
				TraceID         string `json:"traceID"`
				RootServiceName string `json:"rootServiceName"`
				RootTraceName   string `json:"rootTraceName"`
			} `json:"traces"`
		}
		getJSON(t, tempoURL+"/api/search?tags="+url.QueryEscape("service.name=conformance")+"&limit=5&start="+strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)+"&end="+strconv.FormatInt(time.Now().Unix(), 10), &res)
		if len(res.Traces) == 0 {
			return false
		}
		t.Logf("tempo trace %s root=%q", res.Traces[0].TraceID, res.Traces[0].RootTraceName)
		return true
	}, 2*time.Minute, 5*time.Second, "conformance traces never appeared in Tempo")
}
