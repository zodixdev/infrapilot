#!/usr/bin/env bash
# Generate .env from .env.example with random secrets. Idempotent: refuses to overwrite unless FORCE=1.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
if [[ -f .env && "${FORCE:-0}" != "1" ]]; then
  echo "gen-env: .env exists (set FORCE=1 to regenerate)"; exit 0
fi
while IFS= read -r line; do
  if [[ "$line" =~ ^([A-Z0-9_]+(PASSWORD|SECRET))=$ ]]; then
    printf '%s=%s\n' "${BASH_REMATCH[1]}" "$(openssl rand -hex 24)"
  else
    printf '%s\n' "$line"
  fi
done < .env.example > .env
chmod 600 .env
echo "gen-env: wrote .env (git-ignored)"
