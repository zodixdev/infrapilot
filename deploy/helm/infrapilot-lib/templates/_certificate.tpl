{{- define "infrapilot-lib.certificate" -}}
{{- $v := include "infrapilot-lib.values" . | fromYaml -}}
{{- if $v.tls.enabled }}
{{- $fullname := include "infrapilot-lib.fullname" . -}}
{{- $ns := .Release.Namespace -}}
---
# Workload identity certificate (ADR-0025). The URI SAN is the identity the peer allow-lists compare; the DNS
# names let clients verify the server by its Service name.
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ $fullname }}
  namespace: {{ $ns }}
  labels:
    {{- include "infrapilot-lib.labels" . | nindent 4 }}
spec:
  secretName: {{ $fullname }}-tls
  duration: {{ $v.tls.duration }}
  renewBefore: {{ $v.tls.renewBefore }}
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  usages: [server auth, client auth, digital signature]
  uris:
    - {{ printf "spiffe://%s/ns/%s/sa/%s" $v.tls.trustDomain $ns (include "infrapilot-lib.serviceAccountName" .) }}
  dnsNames:
    - {{ $fullname }}
    - {{ printf "%s.%s.svc" $fullname $ns }}
    {{- if $v.ports.grpc }}
    - {{ printf "%s-grpc.%s.svc" $fullname $ns }}
    {{- end }}
  issuerRef:
    group: cert-manager.io
    kind: {{ $v.tls.issuerRef.kind }}
    name: {{ $v.tls.issuerRef.name }}
{{- end }}
{{- end -}}
