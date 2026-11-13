#!/usr/bin/env bash
# Lint the library chart through its reference consumer and validate rendered manifests with kubeconform.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
K8S_VERSION="${K8S_VERSION:-1.34.0}"
helm lint deploy/helm/infrapilot-lib
helm dependency update tests/helm/conformance >/dev/null
helm lint --strict tests/helm/conformance
helm template conformance tests/helm/conformance --namespace infrapilot-test --api-versions monitoring.coreos.com/v1 |
  go tool kubeconform -strict -summary -kubernetes-version "$K8S_VERSION" \
    -schema-location default \
    -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
# Service charts: strict lint and schema validation as a production render (environment=prod, TLS on). Database
# egress is environment-specific; a representative private range stands in for it here.
prod=(--set environment=prod --set 'migration.egress[0].to[0].ipBlock.cidr=10.0.0.0/8')
for chart in deploy/helm/*/; do
  name="$(basename "$chart")"
  [[ "$name" == infrapilot-lib ]] && continue
  helm dependency update "$chart" >/dev/null
  helm lint --strict "$chart" "${prod[@]}"
  helm template "$name" "$chart" --namespace infrapilot "${prod[@]}" \
    --api-versions monitoring.coreos.com/v1 --api-versions cert-manager.io/v1 |
    go tool kubeconform -strict -summary -kubernetes-version "$K8S_VERSION" \
      -schema-location default \
      -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
done
echo "helm-check: ok"
