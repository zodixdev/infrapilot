{{- define "infrapilot-lib.name" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- default .Chart.Name $v.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "infrapilot-lib.fullname" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.fullnameOverride -}}
{{- $v.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name $v.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "infrapilot-lib.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "infrapilot-lib.selectorLabels" -}}
app.kubernetes.io/name: {{ include "infrapilot-lib.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "infrapilot-lib.labels" -}}
{{ include "infrapilot-lib.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | default "0.0.0" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: infrapilot
helm.sh/chart: {{ include "infrapilot-lib.chart" . }}
{{- end -}}

{{- define "infrapilot-lib.serviceAccountName" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.serviceAccount.create -}}{{ include "infrapilot-lib.fullname" . }}{{- else -}}default{{- end -}}
{{- end -}}

{{- define "infrapilot-lib.image" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- $repo := required "image.repository is required" $v.image.repository -}}
{{- if $v.image.digest -}}
{{- printf "%s@%s" $repo $v.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (default .Chart.AppVersion $v.image.tag | toString) -}}
{{- end -}}
{{- end -}}

{{/* Standard env injected into every container: identity, ports, shutdown timing. */}}
{{- define "infrapilot-lib.standardEnv" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
- name: SERVICE_NAME
  value: {{ include "infrapilot-lib.name" . | quote }}
- name: SERVICE_VERSION
  value: {{ .Chart.AppVersion | default "0.0.0" | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: HTTP_ADDR
  value: {{ printf ":%d" (int $v.ports.http) | quote }}
- name: ADMIN_ADDR
  value: {{ printf ":%d" (int $v.ports.admin) | quote }}
{{- if $v.ports.grpc }}
- name: GRPC_ADDR
  value: {{ printf ":%d" (int $v.ports.grpc) | quote }}
{{- end }}
- name: SHUTDOWN_DELAY
  value: {{ printf "%ds" (int $v.shutdown.delaySeconds) | quote }}
- name: SHUTDOWN_TIMEOUT
  value: {{ printf "%ds" (int $v.shutdown.timeoutSeconds) | quote }}
{{- end -}}

{{- define "infrapilot-lib.probe" -}}
httpGet:
  path: {{ .path }}
  port: {{ .port }}
{{- with .periodSeconds }}
periodSeconds: {{ . }}
{{- end }}
{{- with .timeoutSeconds }}
timeoutSeconds: {{ . }}
{{- end }}
{{- with .failureThreshold }}
failureThreshold: {{ . }}
{{- end }}
{{- with .successThreshold }}
successThreshold: {{ . }}
{{- end }}
{{- end -}}

{{/* Everything a service chart needs; include from templates/all.yaml of the consuming chart. */}}
{{- define "infrapilot-lib.all" -}}
{{ include "infrapilot-lib.serviceaccount" . }}
{{ include "infrapilot-lib.deployment" . }}
{{ include "infrapilot-lib.service" . }}
{{ include "infrapilot-lib.pdb" . }}
{{ include "infrapilot-lib.hpa" . }}
{{ include "infrapilot-lib.networkpolicy" . }}
{{ include "infrapilot-lib.servicemonitor" . }}
{{ include "infrapilot-lib.migration" . }}
{{ include "infrapilot-lib.tests" . }}
{{- end -}}
