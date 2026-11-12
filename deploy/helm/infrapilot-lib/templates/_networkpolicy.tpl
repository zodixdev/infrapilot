{{- define "infrapilot-lib.networkpolicy" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.networkPolicy.enabled }}
---
# Default deny for both directions; only listed peers may talk to the pod and the pod may only reach DNS
# plus the explicitly listed destinations (AGENTS.md §8).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "infrapilot-lib.selectorLabels" . | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    {{- with $v.networkPolicy.ingress }}
    - from:
        {{- toYaml . | nindent 8 }}
      ports:
        - port: http
          protocol: TCP
        {{- if $v.ports.grpc }}
        - port: grpc
          protocol: TCP
        {{- end }}
    {{- end }}
    {{- with $v.networkPolicy.metricsScrapers }}
    - from:
        {{- toYaml . | nindent 8 }}
      ports:
        - port: admin
          protocol: TCP
    {{- end }}
    {{- if $v.tests.enabled }}
    # helm test / e2e pods in the release namespace may call the admin endpoints.
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: test
      ports:
        - port: admin
          protocol: TCP
    {{- end }}
    # kubelet probes originate from the node and are not subject to NetworkPolicy.
  egress:
    {{- if $v.networkPolicy.egress.dns }}
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
    {{- end }}
    {{- with $v.networkPolicy.egress.rules }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}
{{- end -}}
