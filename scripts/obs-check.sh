#!/usr/bin/env bash
# Validate observability-as-code: Prometheus rules/config, collector config, Grafana provisioning
# and dashboards. Uses the same pinned images as docker compose so results match runtime.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
PROM_IMAGE="$(grep -E 'image: prom/prometheus:' deploy/compose/docker-compose.yml | awk '{print $2}')"
OTEL_IMAGE="$(grep -E 'image: otel/opentelemetry-collector-contrib:' deploy/compose/docker-compose.yml | awk '{print $2}')"
docker pull -q "$PROM_IMAGE" >/dev/null
docker pull -q "$OTEL_IMAGE" >/dev/null

echo "obs-check: promtool check rules"
rules=()
for f in observability/prometheus/rules/*.yaml; do rules+=("/obs/rules/$(basename "$f")"); done
docker run --rm -v "$PWD/observability/prometheus:/obs:ro" --entrypoint promtool "$PROM_IMAGE" check rules "${rules[@]}"
echo "obs-check: promtool check config"
docker run --rm -v "$PWD/observability/prometheus:/etc/prometheus:ro" --entrypoint promtool "$PROM_IMAGE" check config /etc/prometheus/prometheus.yml
echo "obs-check: otelcol validate"
docker run --rm -v "$PWD/observability/otel-collector/config.yaml:/etc/otelcol/config.yaml:ro" "$OTEL_IMAGE" validate --config=/etc/otelcol/config.yaml

echo "obs-check: grafana dashboards are valid JSON with uid, title and panels"
for f in observability/grafana/dashboards/*.json; do
  jq -e '.uid and .title and (.panels | length > 0)' "$f" >/dev/null || { echo "invalid dashboard: $f"; exit 1; }
done
echo "obs-check: grafana provisioning parses"
for f in observability/grafana/provisioning/*/*.yaml; do
  yq -e '.apiVersion == 1' "$f" >/dev/null || { echo "invalid provisioning file: $f"; exit 1; }
done
echo "obs-check: every alert has severity and runbook_url"
yq -e '[.groups[].rules[] | select(.alert) | select(.labels.severity == null or .annotations.runbook_url == null)] | length == 0' \
  observability/prometheus/rules/go-service.yaml >/dev/null || { echo "alert missing severity or runbook_url"; exit 1; }
echo "obs-check: ok"
