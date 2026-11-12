#!/usr/bin/env bash
# Publish a finished milestone to GitHub. The ONLY sanctioned way to push in this repo.
# Usage: scripts/publish.sh M01
set -euo pipefail

ms="${1:-}"
REMOTE_PATTERN="${INFRAPILOT_REMOTE_PATTERN:-^git@github-new:zodixdev/}"

fail() { echo "publish: $*" >&2; exit 1; }

[[ "$ms" =~ ^M[0-9]{2}$ ]] || fail "usage: scripts/publish.sh MXX (e.g. M01)"
cd "$(git rev-parse --show-toplevel)"
nn="${ms#M}"

# 1. Right branch and remote
[[ "$(git rev-parse --abbrev-ref HEAD)" == "main" ]] || fail "must be on branch main"
url="$(git remote get-url origin 2>/dev/null)" || fail "remote 'origin' is not configured (owner sets it once)"
[[ "$url" =~ $REMOTE_PATTERN ]] || fail "origin ($url) does not match $REMOTE_PATTERN"

# 2. Clean tree, nothing local-only tracked
[[ -z "$(git status --porcelain)" ]] || fail "working tree not clean — commit or discard changes first"
if git ls-files | grep -Eq '^\.local-(reports|reviews|evidence)/'; then
  fail "local-only files are tracked — remove them from the index"
fi
for d in .local-reports .local-reviews .local-evidence; do
  git check-ignore -q "$d/probe" || fail "$d/ is not git-ignored"
done
if git ls-files | grep -E '(^|/)\.env(\..+)?$' | grep -vq '\.env\.example$'; then
  fail ".env file is tracked"
fi

# 3. Milestone really closed
grep -Eq "^## ${ms} .*status: done" docs/roadmap.md || fail "${ms} is not marked 'status: done' in docs/roadmap.md"
[[ -f ".local-reports/milestone-${nn}-report.md" ]] || fail "missing .local-reports/milestone-${nn}-report.md"

# 4. Secrets scan over the full history
command -v gitleaks >/dev/null || fail "gitleaks not installed (add it to 'make bootstrap')"
if gitleaks git --help >/dev/null 2>&1; then
  gitleaks git --redact --no-banner . || fail "gitleaks found secrets"
else
  gitleaks detect --redact --no-banner || fail "gitleaks found secrets"
fi

# 5. Full verification, if the Makefile exists yet
if [[ -f Makefile ]] && make -n verify >/dev/null 2>&1; then
  make verify || fail "make verify failed"
fi

# 6. Tag and push (never force)
tag="milestone/${ms}"
if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
  [[ "$(git rev-list -n1 "$tag")" == "$(git rev-parse HEAD)" ]] || fail "tag ${tag} already exists on another commit"
else
  git tag -a "$tag" -m "InfraPilot ${ms} complete"
fi

git push origin main
git push origin "$tag"
echo "publish: ${ms} pushed to ${url} (main + ${tag})"
