{{- define "infrapilot-lib.cronjobs" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- $root := . -}}
{{- range $job := $v.cronJobs }}
{{- $j := mergeOverwrite (deepCopy $v.cronJobDefaults) $job -}}
{{- $name := printf "%s-%s" (include "infrapilot-lib.fullname" $root) (required "cronJobs[].name is required" $j.name) | trunc 52 | trimSuffix "-" -}}
{{- if $v.networkPolicy.enabled }}
---
# The job's own policy: no ingress, DNS plus the job's declared egress only.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ printf "job-%s" $j.name }}
spec:
  podSelector:
    matchLabels:
      {{- include "infrapilot-lib.selectorLabels" $root | nindent 6 }}
      app.kubernetes.io/component: {{ printf "job-%s" $j.name }}
  policyTypes: [Ingress, Egress]
  ingress: []
  egress:
    {{- include "infrapilot-lib.dnsEgress" $v | nindent 4 }}
    {{- with $j.egress }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- with $v.networkPolicy.egress.extraRules }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ $name }}
  namespace: {{ $root.Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ printf "job-%s" $j.name }}
spec:
  schedule: {{ required "cronJobs[].schedule is required" $j.schedule | quote }}
  concurrencyPolicy: {{ $j.concurrencyPolicy }}
  startingDeadlineSeconds: {{ $j.startingDeadlineSeconds }}
  successfulJobsHistoryLimit: {{ $j.successfulJobsHistoryLimit }}
  failedJobsHistoryLimit: {{ $j.failedJobsHistoryLimit }}
  jobTemplate:
    spec:
      backoffLimit: {{ $j.backoffLimit }}
      activeDeadlineSeconds: {{ $j.activeDeadlineSeconds }}
      ttlSecondsAfterFinished: {{ $j.ttlSecondsAfterFinished }}
      template:
        metadata:
          labels:
            {{- include "infrapilot-lib.selectorLabels" $root | nindent 12 }}
            app.kubernetes.io/component: {{ printf "job-%s" $j.name }}
        spec:
          restartPolicy: Never
          # Jobs need no Kubernetes API access.
          serviceAccountName: default
          automountServiceAccountToken: false
          securityContext:
            {{- toYaml $v.podSecurityContext | nindent 12 }}
          {{- with $v.imagePullSecrets }}
          imagePullSecrets:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          containers:
            - name: {{ $j.name }}
              image: {{ include "infrapilot-lib.image" $root | quote }}
              imagePullPolicy: {{ $v.image.pullPolicy }}
              {{- with $j.command }}
              command:
                {{- toYaml . | nindent 16 }}
              {{- end }}
              {{- with $j.args }}
              args:
                {{- toYaml . | nindent 16 }}
              {{- end }}
              securityContext:
                {{- toYaml $v.securityContext | nindent 16 }}
              env:
                {{- include "infrapilot-lib.standardEnv" $root | nindent 16 }}
                {{- with $j.env }}
                {{- toYaml . | nindent 16 }}
                {{- end }}
              {{- with $j.envFrom }}
              envFrom:
                {{- toYaml . | nindent 16 }}
              {{- end }}
              resources:
                {{- toYaml $j.resources | nindent 16 }}
              volumeMounts:
                - name: tmp
                  mountPath: /tmp
          volumes:
            - name: tmp
              emptyDir:
                sizeLimit: 64Mi
{{- end }}
{{- end -}}
