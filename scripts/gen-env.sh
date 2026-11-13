#!/usr/bin/env bash
# Create or complete .env from .env.example. Every empty *_PASSWORD / *_SECRET / *_TOKEN / *_KEY gets a
# random value. An existing .env is never overwritten: keys missing from it are appended, so new settings
# arrive without rotating secrets that compose volumes already hold. FORCE=1 regenerates everything.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

value_for() {
  local key="$1" default="$2"
  if [[ -z "$default" && "$key" =~ (PASSWORD|SECRET|TOKEN|KEY)$ ]]; then
    openssl rand -hex 24
  else
    printf '%s' "$default"
  fi
}

if [[ "${FORCE:-0}" == "1" || ! -f .env ]]; then
  [[ -f .env ]] && echo "gen-env: secrets changed — existing compose volumes still hold the old passwords; run: make down VOLUMES=1"
  umask 077
  while IFS= read -r line; do
    if [[ "$line" =~ ^([A-Z0-9_]+)=(.*)$ ]]; then
      printf '%s=%s\n' "${BASH_REMATCH[1]}" "$(value_for "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}")"
    else
      printf '%s\n' "$line"
    fi
  done < .env.example > .env
  echo "gen-env: wrote .env (git-ignored)"
  exit 0
fi

added=0
while IFS= read -r line; do
  if [[ "$line" =~ ^([A-Z0-9_]+)=(.*)$ ]] && ! grep -q "^${BASH_REMATCH[1]}=" .env; then
    printf '%s=%s\n' "${BASH_REMATCH[1]}" "$(value_for "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}")" >> .env
    echo "gen-env: added ${BASH_REMATCH[1]}"
    added=$((added + 1))
  fi
done < .env.example
chmod 600 .env
echo "gen-env: .env up to date (${added} key(s) added)"
