{{- define "infrapilot-lib.networkpolicy" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.networkPolicy.enabled }}
---
# Default deny for both directions; only listed peers may talk to the serving pods and they may only reach
# DNS plus the explicitly listed destinations (AGENTS.md §8).
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
      {{- include "infrapilot-lib.workloadSelectorLabels" . | nindent 6 }}
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
    # helm test / e2e pods of THIS release only (label set by the test templates, see _tests.tpl).
    - from:
        - podSelector:
            matchLabels:
              {{- include "infrapilot-lib.testSelectorLabels" . | nindent 14 }}
      ports:
        - port: http
          protocol: TCP
        - port: admin
          protocol: TCP
        {{- if $v.ports.grpc }}
        - port: grpc
          protocol: TCP
        {{- end }}
    {{- end }}
    # kubelet probes originate from the node and are not subject to NetworkPolicy.
  egress:
    {{- include "infrapilot-lib.dnsEgress" $v | nindent 4 }}
    {{- with $v.networkPolicy.egress.rules }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}
{{- end -}}

{{- define "infrapilot-lib.dnsEgress" -}}
{{- if .networkPolicy.egress.dns -}}
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
{{- end -}}
{{- end -}}

{{- define "infrapilot-lib.testSelectorLabels" -}}
app.kubernetes.io/name: {{ include "infrapilot-lib.name" . }}-test
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: test
{{- end -}}
