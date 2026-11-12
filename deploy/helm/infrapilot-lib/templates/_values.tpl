{{/*
Defaults for every InfraPilot service. A consuming chart overrides any key in its own values.yaml.
Maps are deep-merged, lists are replaced.
*/}}
{{- define "infrapilot-lib.defaults" -}}
nameOverride: ""
fullnameOverride: ""
image:
  repository: ""
  tag: ""
  digest: ""
  pullPolicy: IfNotPresent
imagePullSecrets: []
replicaCount: 2
revisionHistoryLimit: 3
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 0
    maxSurge: 1
command: []
args: []
env: []
envFrom: []
# Container ports by name. Set grpc to null for services without a gRPC listener.
ports:
  http: 8080
  admin: 8081
  grpc: 9090
service:
  type: ClusterIP
  annotations: {}
serviceAccount:
  create: true
  annotations: {}
  automountToken: false
podAnnotations: {}
podLabels: {}
priorityClassName: ""
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  privileged: false
  capabilities:
    drop: [ALL]
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    memory: 256Mi
probes:
  startup:
    path: /healthz
    port: admin
    periodSeconds: 2
    failureThreshold: 30
  liveness:
    path: /healthz
    port: admin
    periodSeconds: 10
    timeoutSeconds: 2
    failureThreshold: 3
  readiness:
    path: /readyz
    port: admin
    periodSeconds: 5
    timeoutSeconds: 2
    failureThreshold: 3
    successThreshold: 1
# Graceful shutdown (pkg/app): readiness off → delay → drain within timeout. Grace period = delay + timeout + 5.
shutdown:
  delaySeconds: 5
  timeoutSeconds: 25
pdb:
  enabled: true
  minAvailable: 1
hpa:
  enabled: false
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: null
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
nodeSelector: {}
tolerations: []
affinity: {}
extraVolumes: []
extraVolumeMounts: []
networkPolicy:
  enabled: true
  # Peers allowed to reach the http/grpc ports, e.g. [{podSelector: {matchLabels: {app.kubernetes.io/name: api-gateway}}}]
  ingress: []
  # Peers allowed to scrape the admin port (Prometheus).
  metricsScrapers:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
  egress:
    dns: true
    # Extra egress rules in NetworkPolicyEgressRule form.
    rules: []
serviceMonitor:
  enabled: true
  interval: 15s
  scrapeTimeout: 10s
  path: /metrics
  port: admin
  labels: {}
migration:
  enabled: false
  command: []
  args: []
  env: []
  envFrom: []
  backoffLimit: 3
  activeDeadlineSeconds: 600
  ttlSecondsAfterFinished: 600
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      memory: 256Mi
tests:
  enabled: true
  curlImage: curlimages/curl:8.17.0
  grpcHealthProbeImage: ghcr.io/grpc-ecosystem/grpc-health-probe:v0.4.40
{{- end -}}

{{/* Merged values: chart values override library defaults. */}}
{{- define "infrapilot-lib.values" -}}
{{- $defaults := include "infrapilot-lib.defaults" . | fromYaml -}}
{{- mergeOverwrite $defaults (deepCopy .Values) | toYaml -}}
{{- end -}}
