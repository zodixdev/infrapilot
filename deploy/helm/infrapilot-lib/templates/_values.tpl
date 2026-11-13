{{/*
Defaults for every InfraPilot service. A consuming chart overrides any key in its own values.yaml.
Maps are deep-merged, lists are replaced.
*/}}
{{- define "infrapilot-lib.defaults" -}}
# Deployment environment (dev, test, staging, prod) → ENVIRONMENT. Required: the production safety rails of
# pkg/config (TLS, no plaintext peers, no reflection) only apply when the service knows it runs in prod.
environment: ""
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
# Environment-specific variables appended after `env` (Helm replaces lists, so an environment's values file sets
# these without repeating the chart's own env).
extraEnv: []
envFrom: []
# Container ports by name. Set http or grpc to 0 for services without that listener (null is dropped by Helm
# and would fall back to these defaults).
ports:
  http: 8080
  admin: 8081
  grpc: 9090
service:
  type: ClusterIP
  annotations: {}
  # Fixed node ports by port name when type is NodePort (local clusters only; production uses an ingress).
  nodePorts: {}
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
# Rendered only when at least two replicas are guaranteed (replicaCount, or hpa.minReplicas with HPA):
# a PDB over a single replica blocks every node drain.
pdb:
  enabled: true
  maxUnavailable: 1
hpa:
  enabled: false
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70
  targetMemoryUtilizationPercentage: null
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
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
    # Environment-wide additions (e.g. the telemetry collector) applied to server pods and cron jobs; kept apart
    # from `rules` because Helm replaces lists instead of merging them.
    extraRules: []
serviceMonitor:
  enabled: true
  interval: 15s
  scrapeTimeout: 10s
  path: /metrics
  port: admin
  labels: {}
migration:
  enabled: false
  # Egress for the migration job (its own NetworkPolicy; DNS is always allowed). Typically the DB only.
  egress: []
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
# Workload certificate from cert-manager (ADR-0025): URI SAN spiffe://<trustDomain>/ns/<namespace>/sa/<sa>,
# DNS names of the Services, server + client usage. Mounted at mountPath (tls.crt, tls.key, ca.crt).
tls:
  enabled: false
  issuerRef:
    kind: ClusterIssuer
    name: infrapilot-internal-ca
  trustDomain: infrapilot.local
  duration: 24h
  renewBefore: 8h
  mountPath: /etc/infrapilot/tls
# Scheduled jobs of the service, each with its own NetworkPolicy (DNS + egress only, no ingress), e.g.
#   - name: purge
#     schedule: "17 * * * *"
#     args: [purge]
#     env: []
#     envFrom: []
#     egress: []            # NetworkPolicyEgressRule list; the job's own policy, not the server's
#     activeDeadlineSeconds: 3300
cronJobs: []
cronJobDefaults:
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 300
  successfulJobsHistoryLimit: 1
  failedJobsHistoryLimit: 3
  backoffLimit: 1
  activeDeadlineSeconds: 3300
  ttlSecondsAfterFinished: 86400
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      memory: 256Mi
# helm test pods. Off by default: enabling them also opens the http/admin/grpc ports to this release's test
# pods (and only those) in the NetworkPolicy. Test and CI environments turn it on.
tests:
  enabled: false
  curlImage: curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40
  grpcHealthProbeImage: ghcr.io/grpc-ecosystem/grpc-health-probe:v0.4.40@sha256:38d74888e537b1a4d581ffa9281dec6c24e55429f3a20d943c9a118e7f3fab7f
{{- end -}}

{{/* Merged values: chart values override library defaults. */}}
{{- define "infrapilot-lib.values" -}}
{{- $defaults := include "infrapilot-lib.defaults" . | fromYaml -}}
{{- mergeOverwrite $defaults (deepCopy .Values) | toYaml -}}
{{- end -}}
