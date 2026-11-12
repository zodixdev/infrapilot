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
echo "helm-check: ok"
