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

{{/* Selector for the serving pods only: migration and test pods share name/instance but not the component. */}}
{{- define "infrapilot-lib.workloadSelectorLabels" -}}
{{ include "infrapilot-lib.selectorLabels" . }}
app.kubernetes.io/component: server
{{- end -}}

{{/* Version reported by the service: the image digest or tag being deployed, falling back to appVersion. */}}
{{- define "infrapilot-lib.version" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.image.digest -}}{{ $v.image.digest | trunc 19 }}{{- else -}}{{ default .Chart.AppVersion $v.image.tag | toString }}{{- end -}}
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
- name: ENVIRONMENT
  value: {{ required "environment is required (dev, test, staging or prod)" $v.environment | quote }}
- name: SERVICE_NAME
  value: {{ include "infrapilot-lib.name" . | quote }}
- name: SERVICE_VERSION
  value: {{ include "infrapilot-lib.version" . | quote }}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- if $v.ports.http }}
- name: HTTP_ADDR
  value: {{ printf ":%d" (int $v.ports.http) | quote }}
{{- end }}
- name: ADMIN_ADDR
  value: {{ printf ":%d" (int $v.ports.admin) | quote }}
{{- if $v.ports.grpc }}
- name: GRPC_ADDR
  value: {{ printf ":%d" (int $v.ports.grpc) | quote }}
{{- end }}
{{- if and $v.tls.enabled $v.ports.grpc }}
- name: GRPC_TLS_CERT_FILE
  value: {{ printf "%s/tls.crt" $v.tls.mountPath | quote }}
- name: GRPC_TLS_KEY_FILE
  value: {{ printf "%s/tls.key" $v.tls.mountPath | quote }}
- name: GRPC_TLS_CLIENT_CA_FILE
  value: {{ printf "%s/ca.crt" $v.tls.mountPath | quote }}
{{- end }}
- name: SHUTDOWN_DELAY
  value: {{ printf "%ds" (int $v.shutdown.delaySeconds) | quote }}
- name: SHUTDOWN_TIMEOUT
  value: {{ printf "%ds" (int $v.shutdown.timeoutSeconds) | quote }}
{{- end -}}

{{/*
GOMEMLIMIT at ~90 % of a container's memory limit ("256Mi", "1Gi"), so the Go GC works harder before the kernel
OOM-kills the container. Renders nothing for other units or no limit.
*/}}
{{- define "infrapilot-lib.gomemlimit" -}}
{{- $m := toString (default "" .) -}}
{{- $bytes := 0 -}}
{{- if hasSuffix "Mi" $m -}}{{- $bytes = div (mul (atoi (trimSuffix "Mi" $m)) 1048576 9) 10 -}}
{{- else if hasSuffix "Gi" $m -}}{{- $bytes = div (mul (atoi (trimSuffix "Gi" $m)) 1073741824 9) 10 -}}
{{- else if hasSuffix "Ki" $m -}}{{- $bytes = div (mul (atoi (trimSuffix "Ki" $m)) 1024 9) 10 -}}
{{- else if hasSuffix "M" $m -}}{{- $bytes = div (mul (atoi (trimSuffix "M" $m)) 1000000 9) 10 -}}
{{- else if hasSuffix "G" $m -}}{{- $bytes = div (mul (atoi (trimSuffix "G" $m)) 1000000000 9) 10 -}}
{{- else if $m -}}{{- fail (printf "resources.limits.memory %q: use Ki, Mi, Gi, M or G" $m) -}}
{{- end -}}
{{- if gt (int64 $bytes) 0 }}
- name: GOMEMLIMIT
  value: {{ $bytes | quote }}
{{- end -}}
{{- end -}}

{{/* Staging and prod must declare where pods may connect: an empty egress list would silently cut dependencies. */}}
{{- define "infrapilot-lib.requireEgress" -}}
{{- $v := .v -}}
{{- if has $v.environment (list "staging" "prod") -}}
{{- if not $v.networkPolicy.enabled -}}
{{- fail (printf "networkPolicy.enabled=false is not allowed in %s (default deny, AGENTS.md section 8)" $v.environment) -}}
{{- end -}}
{{- /* Only the workload's own rules count: environment-wide extraRules (telemetry) say nothing about its dependencies. */ -}}
{{- if not .rules -}}
{{- fail (printf "%s: egress must list the dependencies in %s (networkPolicy.egress.rules / cronJobs[].egress)" .what $v.environment) -}}
{{- end -}}
{{- end -}}
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
{{ include "infrapilot-lib.certificate" . }}
{{ include "infrapilot-lib.cronjobs" . }}
{{ include "infrapilot-lib.tests" . }}
{{- end -}}
