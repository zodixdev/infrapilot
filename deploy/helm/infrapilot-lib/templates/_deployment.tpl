{{- define "infrapilot-lib.deployment" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "infrapilot-lib.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
spec:
  {{- if not $v.hpa.enabled }}
  replicas: {{ $v.replicaCount }}
  {{- end }}
  revisionHistoryLimit: {{ $v.revisionHistoryLimit }}
  strategy:
    {{- toYaml $v.strategy | nindent 4 }}
  selector:
    matchLabels:
      {{- include "infrapilot-lib.workloadSelectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "infrapilot-lib.labels" . | nindent 8 }}
        app.kubernetes.io/component: server
        {{- with $v.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: {{ $v.ports.admin | quote }}
        prometheus.io/path: {{ $v.serviceMonitor.path | quote }}
        {{- with $v.podAnnotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      serviceAccountName: {{ include "infrapilot-lib.serviceAccountName" . }}
      automountServiceAccountToken: {{ $v.serviceAccount.automountToken }}
      terminationGracePeriodSeconds: {{ add (int $v.shutdown.delaySeconds) (int $v.shutdown.timeoutSeconds) 5 }}
      {{- with $v.priorityClassName }}
      priorityClassName: {{ . }}
      {{- end }}
      {{- with $v.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml $v.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ include "infrapilot-lib.name" . }}
          image: {{ include "infrapilot-lib.image" . | quote }}
          imagePullPolicy: {{ $v.image.pullPolicy }}
          {{- with $v.command }}
          command:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with $v.args }}
          args:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            {{- toYaml $v.securityContext | nindent 12 }}
          ports:
            {{- if $v.ports.http }}
            - name: http
              containerPort: {{ $v.ports.http }}
              protocol: TCP
            {{- end }}
            - name: admin
              containerPort: {{ $v.ports.admin }}
              protocol: TCP
            {{- if $v.ports.grpc }}
            - name: grpc
              containerPort: {{ $v.ports.grpc }}
              protocol: TCP
            {{- end }}
          env:
            {{- include "infrapilot-lib.standardEnv" . | nindent 12 }}
            {{- include "infrapilot-lib.gomemlimit" $v.resources.limits.memory | nindent 12 }}
            {{- with $v.env }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
            {{- with $v.extraEnv }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          {{- with $v.envFrom }}
          envFrom:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          startupProbe:
            {{- include "infrapilot-lib.probe" $v.probes.startup | nindent 12 }}
          livenessProbe:
            {{- include "infrapilot-lib.probe" $v.probes.liveness | nindent 12 }}
          readinessProbe:
            {{- include "infrapilot-lib.probe" $v.probes.readiness | nindent 12 }}
          resources:
            {{- toYaml $v.resources | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
            {{- if $v.tls.enabled }}
            - name: tls
              mountPath: {{ $v.tls.mountPath }}
              readOnly: true
            {{- end }}
            {{- with $v.extraVolumeMounts }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
      volumes:
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
        {{- if $v.tls.enabled }}
        - name: tls
          secret:
            secretName: {{ include "infrapilot-lib.fullname" . }}-tls
            defaultMode: 0440
        {{- end }}
        {{- with $v.extraVolumes }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with $v.topologySpreadConstraints }}
      topologySpreadConstraints:
        {{- range . }}
        - {{- toYaml . | nindent 10 }}
          labelSelector:
            matchLabels:
              {{- include "infrapilot-lib.workloadSelectorLabels" $ | nindent 14 }}
          # Count only pods of the same ReplicaSet so a rollout's old pods do not skew placement.
          matchLabelKeys: [pod-template-hash]
        {{- end }}
      {{- end }}
      {{- with $v.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $v.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $v.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end -}}
