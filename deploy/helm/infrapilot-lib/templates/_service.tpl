{{- define "infrapilot-lib.serviceaccount" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.serviceAccount.create }}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "infrapilot-lib.serviceAccountName" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
  {{- with $v.serviceAccount.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
automountServiceAccountToken: {{ $v.serviceAccount.automountToken }}
{{- end }}
{{- end -}}

{{- define "infrapilot-lib.service" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
  {{- with $v.service.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  type: {{ $v.service.type }}
  selector:
    {{- include "infrapilot-lib.workloadSelectorLabels" . | nindent 4 }}
  ports:
    - name: http
      port: {{ $v.ports.http }}
      targetPort: http
      protocol: TCP
    - name: admin
      port: {{ $v.ports.admin }}
      targetPort: admin
      protocol: TCP
    {{- if $v.ports.grpc }}
    - name: grpc
      port: {{ $v.ports.grpc }}
      targetPort: grpc
      protocol: TCP
      appProtocol: grpc
    {{- end }}
{{- end -}}

{{- define "infrapilot-lib.pdb" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- $minReplicas := ternary (int $v.hpa.minReplicas) (int $v.replicaCount) $v.hpa.enabled -}}
{{- if and $v.pdb.enabled (gt $minReplicas 1) }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
spec:
  maxUnavailable: {{ $v.pdb.maxUnavailable }}
  # Pods that are not ready may always be evicted: they serve nothing and must not block a drain.
  unhealthyPodEvictionPolicy: AlwaysAllow
  selector:
    matchLabels:
      {{- include "infrapilot-lib.workloadSelectorLabels" . | nindent 6 }}
{{- end }}
{{- end -}}

{{- define "infrapilot-lib.hpa" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.hpa.enabled }}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ include "infrapilot-lib.fullname" . }}
  minReplicas: {{ $v.hpa.minReplicas }}
  maxReplicas: {{ $v.hpa.maxReplicas }}
  metrics:
    {{- if $v.hpa.targetCPUUtilizationPercentage }}
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ $v.hpa.targetCPUUtilizationPercentage }}
    {{- end }}
    {{- if $v.hpa.targetMemoryUtilizationPercentage }}
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: {{ $v.hpa.targetMemoryUtilizationPercentage }}
    {{- end }}
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
{{- end }}
{{- end -}}

{{- define "infrapilot-lib.servicemonitor" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if and $v.serviceMonitor.enabled (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1") }}
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
    {{- with $v.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      {{- include "infrapilot-lib.selectorLabels" . | nindent 6 }}
  namespaceSelector:
    matchNames: [{{ .Release.Namespace }}]
  # Copied onto every scraped series (incl. `up`) as app_kubernetes_io_part_of; alerts select on it.
  targetLabels: [app.kubernetes.io/part-of]
  endpoints:
    - port: {{ $v.serviceMonitor.port }}
      path: {{ $v.serviceMonitor.path }}
      interval: {{ $v.serviceMonitor.interval }}
      scrapeTimeout: {{ $v.serviceMonitor.scrapeTimeout }}
{{- end }}
{{- end -}}
