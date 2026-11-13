//go:build e2e

// Package e2e verifies the conformance release in kind end to end: every pod ready, readiness reports the
// Postgres dependency, panics are 500s in metrics, and traffic produces traces in Tempo, metrics in
// Prometheus and logs in Loki (M01 acceptance). It also proves that every metric the alert rules and the
// dashboard use is actually emitted on the push path.
// Prerequisites: make up PROFILE=obs, make cluster deploy.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	promURL     = env("PROMETHEUS_URL", "http://127.0.0.1:9090")
	tempoURL    = env("TEMPO_URL", "http://127.0.0.1:3200")
	lokiURL     = env("LOKI_URL", "http://127.0.0.1:3100")
	curlImage   = env("CURL_IMAGE", "curlimages/curl:8.17.0")
	probeImage  = env("GRPC_PROBE_IMAGE", "ghcr.io/grpc-ecosystem/grpc-health-probe:v0.4.40")
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

// testPod runs a throw-away pod in the release namespace. It carries the release's test labels, the only
// peers (besides scrapers) the NetworkPolicy admits, and complies with the restricted Pod Security profile.
func testPod(t *testing.T, name, image string, cmd ...string) string {
	t.Helper()
	overrides := `{"spec":{"automountServiceAccountToken":false,"securityContext":{"runAsNonRoot":true,"runAsUser":65532,` +
		`"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"` + name + `","image":"` + image + `","args":` +
		mustJSON(cmd) + `,"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}`
	return kubectl(t, "run", name, "--rm", "-i", "--restart=Never", "--quiet", "--image="+image,
		"--labels=app.kubernetes.io/name="+release+"-test,app.kubernetes.io/instance="+release+",app.kubernetes.io/component=test",
		"--overrides", overrides)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// getJSON never fails the test itself: it is called from Eventually conditions, which must not call
// t.FailNow off the test goroutine.
func getJSON(ctx context.Context, rawURL string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d %s", rawURL, resp.StatusCode, body)
	}
	return json.Unmarshal(body, dst)
}

type promResult struct {
	Data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func promQuery(ctx context.Context, q string) (promResult, error) {
	var res promResult
	err := getJSON(ctx, promURL+"/api/v1/query?query="+url.QueryEscape(q), &res)
	return res, err
}

func promValue(ctx context.Context, q string) (float64, error) {
	res, err := promQuery(ctx, q)
	if err != nil {
		return 0, err
	}
	if len(res.Data.Result) == 0 {
		return 0, errors.New("no result")
	}
	s, _ := res.Data.Result[0].Value[1].(string)
	return strconv.ParseFloat(s, 64)
}

func TestE2E_ConformanceRelease(t *testing.T) {
	ctx := t.Context()

	t.Log("every pod of the release is ready")
	kubectl(t, "rollout", "status", "deployment/"+release, "--timeout=120s")
	pods := strings.Fields(kubectl(t, "get", "pods", "-l", "app.kubernetes.io/name="+release+",app.kubernetes.io/component=server",
		"-o", `jsonpath={range .items[*]}{.metadata.name}={.status.phase}/{.status.containerStatuses[0].ready}{"\n"}{end}`))
	require.Len(t, pods, 2)
	for _, p := range pods {
		require.True(t, strings.HasSuffix(p, "=Running/true"), "pod not ready: %s", p)
	}

	t.Log("readiness reports the postgres dependency and the migration left a probe row")
	out := testPod(t, "e2e-readyz", curlImage, "sh", "-ec", fmt.Sprintf(
		`curl -fsS --retry 6 --retry-all-errors --retry-delay 2 --max-time 10 http://%[1]s:8081/readyz; echo; `+
			`curl -fsS --retry 6 --retry-all-errors --retry-delay 2 --max-time 10 http://%[1]s:8080/v1/probe`, release))
	require.Contains(t, out, `"postgres":{"status":"up"`)
	require.Regexp(t, `"probe_rows":[1-9]`, out)

	t.Log("gRPC health reports SERVING")
	require.Contains(t, testPod(t, "e2e-grpc", probeImage, "-addr="+release+":9090", "-connect-timeout=20s"), "SERVING")

	t.Log("a panic is a problem+json 500")
	out = testPod(t, "e2e-panic", curlImage, "sh", "-c", fmt.Sprintf(`curl -sS --max-time 10 -w ' status=%%{http_code}' http://%s:8080/v1/panic`, release))
	require.Contains(t, out, "status=500")
	require.Contains(t, out, `"code":"internal"`)

	t.Log("generate traffic")
	testPod(t, "e2e-traffic", curlImage, "sh", "-ec", fmt.Sprintf(
		`for i in $(seq 1 30); do curl -fsS --max-time 5 -o /dev/null http://%[1]s:8080/v1/probe; curl -fsS --max-time 5 -o /dev/null "http://%[1]s:8080/v1/slow?ms=20"; done; echo done`, release))

	t.Log("metrics pushed over OTLP reach Prometheus; the panic counts as status 500")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		v, err := promValue(ctx, fmt.Sprintf(`sum(http_server_requests_total{job=%q,route="/v1/probe"})`, release))
		require.NoError(c, err)
		require.GreaterOrEqual(c, v, 30.0)
		v, err = promValue(ctx, fmt.Sprintf(`sum(http_server_requests_total{job=%q,route="/v1/panic",status="500"})`, release))
		require.NoError(c, err)
		require.GreaterOrEqual(c, v, 1.0)
	}, 2*time.Minute, 5*time.Second)

	t.Log("traces reach Tempo")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		var res struct {
			Traces []struct {
				TraceID       string `json:"traceID"`
				RootTraceName string `json:"rootTraceName"`
			} `json:"traces"`
		}
		now := time.Now()
		q := tempoURL + "/api/search?tags=" + url.QueryEscape("service.name="+release) + "&limit=20" +
			"&start=" + strconv.FormatInt(now.Add(-15*time.Minute).Unix(), 10) + "&end=" + strconv.FormatInt(now.Unix(), 10)
		require.NoError(c, getJSON(ctx, q, &res))
		names := make([]string, 0, len(res.Traces))
		for _, tr := range res.Traces {
			names = append(names, tr.RootTraceName)
		}
		require.Contains(c, names, "GET /v1/probe", "route-named root spans")
	}, 2*time.Minute, 5*time.Second)

	t.Log("logs reach Loki with trace ids and without secrets")
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		var res struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][]string        `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		q := lokiURL + "/loki/api/v1/query_range?query=" + url.QueryEscape(fmt.Sprintf(`{service_name=%q} |= "http request"`, release)) +
			"&limit=50&since=15m"
		require.NoError(c, getJSON(ctx, q, &res))
		require.NotEmpty(c, res.Data.Result, "no log streams for %s", release)
		withTrace := 0
		for _, s := range res.Data.Result {
			if s.Stream["trace_id"] != "" {
				withTrace += len(s.Values)
			}
		}
		require.Positive(c, withTrace, "log records carry trace_id as structured metadata")
	}, 2*time.Minute, 5*time.Second)

	t.Log("every metric used by alert rules and the dashboard exists for the release (push path)")
	res, err := promQuery(ctx, fmt.Sprintf(`count by (__name__) ({job=%q})`, release))
	require.NoError(t, err)
	emitted := map[string]bool{}
	for _, r := range res.Data.Result {
		emitted[r.Metric["__name__"]] = true
	}
	for _, name := range referencedMetrics(t) {
		assert.True(t, emitted[name], "metric %q is used by rules/dashboards but not emitted by %s", name, release)
	}
}

// referencedMetrics extracts the InfraPilot metric names used in alert rules and dashboards. `up` is
// excluded: it exists only on the pull path by design (ServiceTelemetryAbsent covers push).
func referencedMetrics(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(root, "observability", "prometheus", "rules", "*.yaml"))
	require.NoError(t, err)
	dash, err := filepath.Glob(filepath.Join(root, "observability", "grafana", "dashboards", "*.json"))
	require.NoError(t, err)
	re := regexp.MustCompile(`\b(?:http_server|grpc_server|health|pgxpool|go|process)_[a-z0-9_]+`)
	seen := map[string]bool{}
	var out []string
	for _, f := range append(files, dash...) {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, m := range re.FindAllString(string(b), -1) {
			if seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	require.NotEmpty(t, out)
	return out
}
