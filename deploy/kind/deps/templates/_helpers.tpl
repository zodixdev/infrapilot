{{/* Existing generated value, or a new random one (kept across upgrades through lookup). */}}
{{- define "deps.secretValue" -}}
{{- $existing := lookup "v1" "Secret" .ns "deps-credentials" -}}
{{- if and $existing (hasKey $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
