#!/usr/bin/env bash
# Validate observability-as-code: Prometheus rules (syntax + unit tests), Prometheus config, collector
# config, Grafana provisioning and dashboards, and that every alert links to an existing runbook section.
# Uses the same pinned images as docker compose so results match runtime.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
YQ=(go tool yq)
PROM_IMAGE="$(grep -E 'image: prom/prometheus:' deploy/compose/docker-compose.yml | awk '{print $2}')"
OTEL_IMAGE="$(grep -E 'image: otel/opentelemetry-collector-contrib:' deploy/compose/docker-compose.yml | awk '{print $2}')"
docker pull -q "$PROM_IMAGE" >/dev/null
docker pull -q "$OTEL_IMAGE" >/dev/null
promtool() { docker run --rm -v "$PWD/observability/prometheus:/obs:ro" -w /obs --entrypoint promtool "$PROM_IMAGE" "$@"; }

echo "obs-check: promtool check rules"
rules=()
for f in observability/prometheus/rules/*.yaml; do rules+=("rules/$(basename "$f")"); done
promtool check rules "${rules[@]}"
echo "obs-check: promtool test rules"
tests=()
for f in observability/prometheus/tests/*.yaml; do tests+=("tests/$(basename "$f")"); done
promtool test rules "${tests[@]}"
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
  "${YQ[@]}" -e '.apiVersion == 1' "$f" >/dev/null || { echo "invalid provisioning file: $f"; exit 1; }
done
echo "obs-check: every alert has severity and a runbook_url pointing at an existing section"
for f in observability/prometheus/rules/*.yaml; do
  "${YQ[@]}" -e '[.groups[].rules[] | select(.alert) | select(.labels.severity == null or .annotations.runbook_url == null)] | length == 0' "$f" >/dev/null \
    || { echo "$f: alert missing severity or runbook_url"; exit 1; }
  while IFS= read -r url; do
    doc="${url#*/blob/main/}"; doc="${doc%%#*}"; anchor="${url##*#}"
    [[ -f "$doc" ]] || { echo "$f: runbook $doc does not exist"; exit 1; }
    # GitHub anchors: lowercase heading text, spaces → '-', punctuation dropped.
    found=0
    while IFS= read -r h; do
      a="$(printf '%s' "$h" | sed -E 's/^#+ +//' | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9 _-]//g; s/ /-/g')"
      [[ "$a" == "$anchor" ]] && { found=1; break; }
    done < <(grep -E '^#{1,6} ' "$doc")
    [[ $found == 1 ]] || { echo "$f: runbook anchor #$anchor not found in $doc"; exit 1; }
  done < <("${YQ[@]}" '.groups[].rules[] | select(.alert) | .annotations.runbook_url' "$f" | sort -u)
done
echo "obs-check: ok"
