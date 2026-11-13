{{- define "infrapilot-lib.tests" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.tests.enabled }}
{{- $svc := include "infrapilot-lib.fullname" . }}
---
# helm test: liveness, readiness and metrics through the Service (admin port).
apiVersion: v1
kind: Pod
metadata:
  name: {{ $svc }}-test-http
  namespace: {{ .Release.Namespace }}
  labels:
    # Deliberately NOT the workload labels: the pod must not become a Service endpoint or NetworkPolicy subject.
    {{- include "infrapilot-lib.testSelectorLabels" . | nindent 4 }}
    app.kubernetes.io/part-of: infrapilot
  annotations:
    helm.sh/hook: test
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    {{- toYaml $v.podSecurityContext | nindent 4 }}
  containers:
    - name: curl
      image: {{ $v.tests.curlImage }}
      securityContext:
        {{- toYaml $v.securityContext | nindent 8 }}
      command: ["/bin/sh", "-ec"]
      args:
        - |
          base="http://{{ $svc }}:{{ $v.ports.admin }}"
          # First lookups in a fresh cluster can be slow: retry on any error for up to ~30 s.
          c() { curl -fsS --retry 6 --retry-all-errors --retry-delay 2 --max-time 10 "$@"; }
          c "$base/healthz"
          c "$base/readyz"
          c "$base/metrics" | grep -q '^go_goroutines'
          echo "http conformance ok"
      resources:
        requests: {cpu: 10m, memory: 16Mi}
        limits: {memory: 64Mi}
{{- if $v.ports.grpc }}
---
# helm test: grpc.health.v1 reports SERVING.
apiVersion: v1
kind: Pod
metadata:
  name: {{ $svc }}-test-grpc
  namespace: {{ .Release.Namespace }}
  labels:
    # Deliberately NOT the workload labels: the pod must not become a Service endpoint or NetworkPolicy subject.
    {{- include "infrapilot-lib.testSelectorLabels" . | nindent 4 }}
    app.kubernetes.io/part-of: infrapilot
  annotations:
    helm.sh/hook: test
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    {{- toYaml $v.podSecurityContext | nindent 4 }}
  containers:
    - name: grpc-health-probe
      image: {{ $v.tests.grpcHealthProbeImage }}
      securityContext:
        {{- toYaml $v.securityContext | nindent 8 }}
      args: ["-addr={{ $svc }}:{{ $v.ports.grpc }}", "-connect-timeout=30s", "-rpc-timeout=5s"]
      resources:
        requests: {cpu: 10m, memory: 16Mi}
        limits: {memory: 64Mi}
{{- end }}
{{- end }}
{{- end -}}
