#!/usr/bin/env bash
# Run `buf breaking` against the last published state (origin/main), falling back to HEAD.
# Skips silently when the baseline has no protobuf module yet (first milestone).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
against="${BUF_AGAINST:-}"
if [[ -z "$against" ]]; then
  if git rev-parse -q --verify origin/main >/dev/null 2>&1; then against=".git#ref=origin/main,subdir=contracts";
  else against=".git#ref=HEAD,subdir=contracts"; fi
fi
ref="${against#.git#ref=}"; ref="${ref%%,*}"
if ! git ls-tree -r --name-only "$ref" -- contracts/buf.yaml 2>/dev/null | grep -q .; then
  echo "buf breaking: no contracts/buf.yaml at $ref, nothing to compare"; exit 0
fi
cd contracts && go tool buf breaking --against "../$against"
