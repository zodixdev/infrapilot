{{- define "infrapilot-lib.migration" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.migration.enabled }}
---
# The release's NetworkPolicy does not exist yet when a pre-install hook runs, so the job (which holds the
# DDL-capable migrator credential) gets its own policy, created first: DNS plus migration.egress only.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}-migrate
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
    app.kubernetes.io/component: migration
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-6"
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  podSelector:
    matchLabels:
      {{- include "infrapilot-lib.selectorLabels" . | nindent 6 }}
      app.kubernetes.io/component: migration
  policyTypes: [Ingress, Egress]
  ingress: []
  egress:
    {{- include "infrapilot-lib.dnsEgress" $v | nindent 4 }}
    {{- with $v.migration.egress }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
---
# Runs schema migrations (expand → migrate → contract, ADR-0018 migrator role) before the new
# revision rolls out. A failed job blocks the release.
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}-migrate
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
    app.kubernetes.io/component: migration
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-5"
    helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded
spec:
  backoffLimit: {{ $v.migration.backoffLimit }}
  activeDeadlineSeconds: {{ $v.migration.activeDeadlineSeconds }}
  ttlSecondsAfterFinished: {{ $v.migration.ttlSecondsAfterFinished }}
  template:
    metadata:
      labels:
        {{- include "infrapilot-lib.selectorLabels" . | nindent 8 }}
        app.kubernetes.io/component: migration
    spec:
      restartPolicy: Never
      # Hooks run before the release's ServiceAccount exists; the job needs no API access, so it uses the
      # namespace default SA with no token mounted.
      serviceAccountName: default
      automountServiceAccountToken: false
      securityContext:
        {{- toYaml $v.podSecurityContext | nindent 8 }}
      {{- with $v.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      containers:
        - name: migrate
          image: {{ include "infrapilot-lib.image" . | quote }}
          imagePullPolicy: {{ $v.image.pullPolicy }}
          {{- with $v.migration.command }}
          command:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with $v.migration.args }}
          args:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            {{- toYaml $v.securityContext | nindent 12 }}
          env:
            {{- include "infrapilot-lib.standardEnv" . | nindent 12 }}
            {{- with $v.migration.env }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          {{- with $v.migration.envFrom }}
          envFrom:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          resources:
            {{- toYaml $v.migration.resources | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
{{- end }}
{{- end -}}
