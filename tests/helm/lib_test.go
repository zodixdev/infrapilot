// Package helm_test locks the security and operability invariants of the infrapilot-lib chart by
// rendering its reference consumer and inspecting the manifests (complements kubeconform, which
// only checks schema validity).
package helm_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type object struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   map[string]any `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

func render(t *testing.T, extraArgs ...string) []object {
	t.Helper()
	chart := chartDir(t)
	if _, statErr := os.Stat(filepath.Join(chart, "charts")); statErr != nil {
		out, err := exec.CommandContext(t.Context(), "helm", "dependency", "update", chart).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	args := append([]string{"template", "conformance", chart, "--namespace", "infrapilot-test", "--api-versions", "monitoring.coreos.com/v1"}, extraArgs...)
	out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
	require.NoError(t, err, string(out))

	var objs []object
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var o object
		require.NoError(t, yaml.Unmarshal(doc, &o), string(doc))
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	return objs
}

func chartDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	return filepath.Join(root, "tests", "helm", "conformance")
}

func find(t *testing.T, objs []object, kind, name string) object {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && o.Metadata["name"] == name {
			return o
		}
	}
	t.Fatalf("%s/%s not rendered", kind, name)
	return object{}
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		require.True(t, ok, "path %v: %T is not a map", path, cur)
		cur, ok = mm[p]
		require.True(t, ok, "path %v missing at %q", path, p)
	}
	return cur
}

func podSpec(t *testing.T, o object) map[string]any {
	t.Helper()
	spec, ok := dig(t, o.Spec, "template", "spec").(map[string]any)
	require.True(t, ok)
	return spec
}

func TestLibChart_SecurityDefaults(t *testing.T) {
	objs := render(t)
	dep := find(t, objs, "Deployment", "conformance")
	ps := podSpec(t, dep)

	require.Equal(t, false, ps["automountServiceAccountToken"])
	require.Equal(t, true, dig(t, ps, "securityContext", "runAsNonRoot"))
	require.Equal(t, "RuntimeDefault", dig(t, ps, "securityContext", "seccompProfile", "type"))

	containers, ok := ps["containers"].([]any)
	require.True(t, ok)
	require.Len(t, containers, 1)
	c, ok := containers[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, dig(t, c, "securityContext", "readOnlyRootFilesystem"))
	require.Equal(t, false, dig(t, c, "securityContext", "allowPrivilegeEscalation"))
	require.Equal(t, []any{"ALL"}, dig(t, c, "securityContext", "capabilities", "drop"))
	require.NotNil(t, dig(t, c, "resources", "requests", "cpu"))
	require.NotNil(t, dig(t, c, "resources", "limits", "memory"))
	for _, probe := range []string{"startupProbe", "livenessProbe", "readinessProbe"} {
		require.Contains(t, c, probe)
		require.Equal(t, "admin", dig(t, c, probe, "httpGet", "port"), probe+" uses the admin port")
	}
	require.Equal(t, "/readyz", dig(t, c, "readinessProbe", "httpGet", "path"))
	require.Equal(t, "/healthz", dig(t, c, "livenessProbe", "httpGet", "path"))

	// Grace period covers delay + drain timeout (+5s slack): 2 + 20 + 5 in the conformance values.
	require.InDelta(t, 27, ps["terminationGracePeriodSeconds"], 0)
	envs := envMap(t, c)
	require.Equal(t, "2s", envs["SHUTDOWN_DELAY"])
	require.Equal(t, "20s", envs["SHUTDOWN_TIMEOUT"])
	require.Equal(t, "conformance", envs["SERVICE_NAME"])
	require.Equal(t, "dev", envs["SERVICE_VERSION"], "version follows the deployed image tag, not appVersion")
	require.Equal(t, ":8081", envs["ADMIN_ADDR"])

	require.InDelta(t, 0, dig(t, dep.Spec, "strategy", "rollingUpdate", "maxUnavailable"), 0.001)
	spread := as[[]any](t, ps["topologySpreadConstraints"])
	require.Len(t, spread, 2, "zone and hostname")
	for _, c := range spread {
		require.Equal(t, []any{"pod-template-hash"}, as[map[string]any](t, c)["matchLabelKeys"])
	}
}

func TestLibChart_NetworkPolicyDefaultDeny(t *testing.T) {
	objs := render(t)
	np := find(t, objs, "NetworkPolicy", "conformance")
	require.ElementsMatch(t, []any{"Ingress", "Egress"}, np.Spec["policyTypes"])
	egress, ok := np.Spec["egress"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, egress, "DNS egress rule present")
	first, ok := egress[0].(map[string]any)
	require.True(t, ok)
	ports, ok := first["ports"].([]any)
	require.True(t, ok)
	require.Len(t, ports, 2, "DNS over UDP and TCP")
	ingress, ok := np.Spec["ingress"].([]any)
	require.True(t, ok)
	for _, r := range ingress {
		rule, ok := r.(map[string]any)
		require.True(t, ok)
		require.Contains(t, rule, "from", "no wildcard ingress rule")
		require.Contains(t, rule, "ports", "ingress rules are port-scoped")
	}

	// The workload policy selects only serving pods.
	sel := as[map[string]any](t, dig(t, np.Spec, "podSelector", "matchLabels"))
	require.Equal(t, "server", sel["app.kubernetes.io/component"])

	// The test-pod exception is scoped to this release's test pods, and absent when tests are disabled.
	found := false
	for _, r := range ingress {
		for _, peer := range as[[]any](t, as[map[string]any](t, r)["from"]) {
			podSel, _ := as[map[string]any](t, peer)["podSelector"].(map[string]any)
			labels, ok := podSel["matchLabels"].(map[string]any)
			if ok && labels["app.kubernetes.io/component"] == "test" {
				found = true
				require.Equal(t, "conformance-test", labels["app.kubernetes.io/name"])
				require.Equal(t, "conformance", labels["app.kubernetes.io/instance"])
			}
		}
	}
	require.True(t, found, "tests.enabled=true adds the scoped test-pod rule")
	noTests := find(t, render(t, "--set", "tests.enabled=false"), "NetworkPolicy", "conformance")
	for _, r := range as[[]any](t, noTests.Spec["ingress"]) {
		for _, peer := range as[[]any](t, as[map[string]any](t, r)["from"]) {
			labels, _ := as[map[string]any](t, peer)["podSelector"].(map[string]any)
			require.NotContains(t, labels, "matchLabels", "no test-pod rule without tests.enabled")
		}
	}

	// The migration job gets its own policy (it runs before the release's policy exists on install).
	mig := find(t, objs, "NetworkPolicy", "conformance-migrate")
	require.Equal(t, "migration", dig(t, mig.Spec, "podSelector", "matchLabels", "app.kubernetes.io/component"))

	// Disabling the workload policy removes it (opt-out must be explicit).
	for _, o := range render(t, "--set", "networkPolicy.enabled=false") {
		if o.Kind == "NetworkPolicy" {
			require.NotEqual(t, "conformance", o.Metadata["name"])
		}
	}
}

func TestLibChart_AvailabilityObjects(t *testing.T) {
	objs := render(t)
	pdb := find(t, objs, "PodDisruptionBudget", "conformance")
	require.InDelta(t, 1, pdb.Spec["maxUnavailable"], 0.001)
	require.Equal(t, "AlwaysAllow", pdb.Spec["unhealthyPodEvictionPolicy"])
	require.Equal(t, "server", dig(t, pdb.Spec, "selector", "matchLabels", "app.kubernetes.io/component"),
		"migration and test pods must not count as available replicas")
	// A single replica gets no PDB (it would block every node drain).
	for _, o := range render(t, "--set", "replicaCount=1") {
		require.NotEqual(t, "PodDisruptionBudget", o.Kind)
	}

	svc := find(t, objs, "Service", "conformance")
	names := map[string]bool{}
	ports, ok := svc.Spec["ports"].([]any)
	require.True(t, ok)
	for _, p := range ports {
		port, ok := p.(map[string]any)
		require.True(t, ok)
		name, ok := port["name"].(string)
		require.True(t, ok)
		names[name] = true
	}
	require.True(t, names["http"] && names["admin"] && names["grpc"], "%v", names)

	sm := find(t, objs, "ServiceMonitor", "conformance")
	ep := as[map[string]any](t, as[[]any](t, sm.Spec["endpoints"])[0])
	require.Equal(t, "admin", ep["port"])
	require.Equal(t, "/metrics", ep["path"])

	sa := find(t, objs, "ServiceAccount", "conformance")
	require.NotNil(t, sa.Metadata["name"])

	// HPA replaces the static replica count when enabled.
	withHPA := render(t, "--set", "hpa.enabled=true")
	dep := find(t, withHPA, "Deployment", "conformance")
	require.NotContains(t, dep.Spec, "replicas")
	hpa := find(t, withHPA, "HorizontalPodAutoscaler", "conformance")
	require.Equal(t, "Deployment", dig(t, hpa.Spec, "scaleTargetRef", "kind"))

	// No ServiceMonitor without the CRD.
	out, err := exec.CommandContext(t.Context(), "helm", "template", "conformance", chartDir(t), "--namespace", "x").CombinedOutput()
	require.NoError(t, err, string(out))
	require.NotContains(t, string(out), "kind: ServiceMonitor")
}

func TestLibChart_MigrationHookAndTests(t *testing.T) {
	objs := render(t)
	job := find(t, objs, "Job", "conformance-migrate")
	ann := as[map[string]any](t, job.Metadata["annotations"])
	require.Equal(t, "pre-install,pre-upgrade", ann["helm.sh/hook"])
	require.Contains(t, ann["helm.sh/hook-delete-policy"], "before-hook-creation")
	ps := podSpec(t, job)
	require.Equal(t, "Never", ps["restartPolicy"])
	c := as[map[string]any](t, as[[]any](t, ps["containers"])[0])
	require.Equal(t, []any{"migrate"}, c["args"])
	require.Equal(t, true, dig(t, c, "securityContext", "readOnlyRootFilesystem"))
	require.NotNil(t, job.Spec["activeDeadlineSeconds"])
	require.NotNil(t, job.Spec["backoffLimit"])

	tests := 0
	for _, o := range objs {
		if o.Kind != "Pod" {
			continue
		}
		ann, _ := o.Metadata["annotations"].(map[string]any)
		if ann["helm.sh/hook"] == "test" {
			tests++
			require.Equal(t, "Never", o.Spec["restartPolicy"])
		}
	}
	require.Equal(t, 2, tests, "http and grpc test pods")
}

func TestLibChart_ImageIsRequired(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "x", chartDir(t), "--set", "image.repository=").CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "image.repository is required")
}

// as performs a checked type assertion.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	out, ok := v.(T)
	require.True(t, ok, "%T is not %T", v, out)
	return out
}

func envMap(t *testing.T, c map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range as[[]any](t, c["env"]) {
		m := as[map[string]any](t, e)
		if v, ok := m["value"].(string); ok {
			out[as[string](t, m["name"])] = v
		}
	}
	return out
}

func TestLibChart_TestPodsAreNotWorkloadPods(t *testing.T) {
	objs := render(t)
	dep := find(t, objs, "Deployment", "conformance")
	selector := as[map[string]any](t, dig(t, dep.Spec, "selector", "matchLabels"))
	for _, o := range objs {
		if o.Kind != "Pod" {
			continue
		}
		labels := as[map[string]any](t, o.Metadata["labels"])
		require.NotEqual(t, selector["app.kubernetes.io/name"], labels["app.kubernetes.io/name"],
			"test pod %s must not match the workload selector (Service endpoints, NetworkPolicy subject)", o.Metadata["name"])
		require.Equal(t, "test", labels["app.kubernetes.io/component"])
	}
}

func TestLibChart_EnvironmentIsRequired(t *testing.T) {
	t.Parallel()
	chart := chartDir(t)
	out, err := exec.CommandContext(t.Context(), "helm", "template", "x", chart, "--set", "environment=").CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "environment is required", "production safety rails depend on ENVIRONMENT")
	objs := render(t)
	env := containerEnv(t, find(t, objs, "Deployment", "conformance"))
	require.Equal(t, "test", env["ENVIRONMENT"])
}

func TestLibChart_HeadlessGRPCService(t *testing.T) {
	t.Parallel()
	svc := find(t, render(t), "Service", "conformance-grpc")
	require.Equal(t, "None", svc.Spec["clusterIP"], "clients balance across pods by DNS")
}

func TestLibChart_WorkloadCertificate(t *testing.T) {
	t.Parallel()
	objs := render(t, "--set", "tls.enabled=true", "--api-versions", "cert-manager.io/v1")
	cert := find(t, objs, "Certificate", "conformance")
	require.Equal(t, []any{"spiffe://infrapilot.local/ns/infrapilot-test/sa/conformance"}, cert.Spec["uris"])
	require.Equal(t, "conformance-tls", cert.Spec["secretName"])
	dep := find(t, objs, "Deployment", "conformance")
	env := containerEnv(t, dep)
	require.Equal(t, "/etc/infrapilot/tls/tls.crt", env["GRPC_TLS_CERT_FILE"])
	require.Equal(t, "/etc/infrapilot/tls/ca.crt", env["GRPC_TLS_CLIENT_CA_FILE"], "mTLS: client certificates are required")
	require.Contains(t, toJSON(t, podSpec(t, dep)["volumes"]), `"secretName":"conformance-tls"`)
}

func TestLibChart_CronJobsAreHardenedAndIsolated(t *testing.T) {
	t.Parallel()
	objs := render(t, "--set", "cronJobs[0].name=purge", "--set", "cronJobs[0].schedule=17 * * * *", "--set", "cronJobs[0].args[0]=purge")
	cj := find(t, objs, "CronJob", "conformance-purge")
	require.Equal(t, "Forbid", cj.Spec["concurrencyPolicy"])
	pod := as[map[string]any](t, dig(t, cj.Spec, "jobTemplate", "spec", "template", "spec"))
	require.Equal(t, false, pod["automountServiceAccountToken"])
	require.Equal(t, true, dig(t, pod, "securityContext", "runAsNonRoot"))
	c := as[map[string]any](t, as[[]any](t, pod["containers"])[0])
	require.Equal(t, true, dig(t, c, "securityContext", "readOnlyRootFilesystem"))
	require.Equal(t, []any{"purge"}, c["args"])
	np := find(t, objs, "NetworkPolicy", "conformance-purge")
	require.Equal(t, []any{}, np.Spec["ingress"], "jobs accept no connections")
	require.Equal(t, "job-purge", dig(t, np.Spec, "podSelector", "matchLabels", "app.kubernetes.io/component"))
}

func containerEnv(t *testing.T, o object) map[string]string {
	t.Helper()
	c := as[map[string]any](t, as[[]any](t, podSpec(t, o)["containers"])[0])
	out := map[string]string{}
	for _, e := range as[[]any](t, c["env"]) {
		m := as[map[string]any](t, e)
		if v, ok := m["value"].(string); ok {
			out[as[string](t, m["name"])] = v
		}
	}
	return out
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestLibChart_DisabledPortsRenderNothing(t *testing.T) {
	t.Parallel()
	objs := render(t, "--set", "ports.grpc=0")
	for _, o := range objs {
		require.False(t, o.Kind == "Service" && o.Metadata["name"] == "conformance-grpc", "no headless gRPC Service without a gRPC port")
		require.False(t, o.Kind == "Pod" && o.Metadata["name"] == "conformance-test-grpc", "no gRPC test without a gRPC port")
	}
	env := containerEnv(t, find(t, objs, "Deployment", "conformance"))
	_, hasGRPC := env["GRPC_ADDR"]
	require.False(t, hasGRPC)
}
