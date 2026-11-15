#!/usr/bin/env bash
# Publish a finished milestone to GitHub. The ONLY sanctioned way to push in this repo.
# Usage: scripts/publish.sh M01 [--repair]
set -euo pipefail

ms="${1:-}"
REMOTE_PATTERN="${INFRAPILOT_REMOTE_PATTERN:-^git@github-new:zodixdev/}"

fail() { echo "publish: $*" >&2; exit 1; }

usage="usage: scripts/publish.sh MXX [--repair]"
[[ "$ms" =~ ^M[0-9]{2}$ ]] || fail "$usage"
repair=false
if [[ $# -gt 1 ]]; then
  [[ $# -eq 2 && "$2" == "--repair" ]] || fail "$usage"
  repair=true
fi
cd "$(git rev-parse --show-toplevel)"
nn="${ms#M}"
base_tag="milestone/${ms}"
tag="$base_tag"
tag_message="InfraPilot ${ms} complete"

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

# A repair is a single follow-up to the current published milestone. Check the
# original tag against GitHub and require the repair to descend from it.
if $repair; then
  tag="${base_tag}-repair.1"
  tag_message="InfraPilot ${ms} repair 1"
  base_ref="refs/tags/$base_tag"
  repair_ref="refs/tags/$tag"
  local_base=$(git rev-parse -q --verify "$base_ref") || fail "missing local original tag $base_tag"
  refs=$(git ls-remote --refs origin refs/heads/main "$base_ref" "$repair_ref") ||
    fail "could not verify published refs"
  remote_base=$(awk -v ref="$base_ref" '$2 == ref {print $1}' <<< "$refs")
  remote_main=$(awk '$2 == "refs/heads/main" {print $1}' <<< "$refs")
  remote_repair=$(awk -v ref="$repair_ref" '$2 == ref {print $1}' <<< "$refs")
  [[ "$remote_base" == "$local_base" ]] || fail "local $base_tag does not match the published tag"
  [[ -z "$remote_repair" ]] || fail "repair tag $tag is already published"
  published=$(git rev-parse "${base_ref}^{commit}")
  [[ "$remote_main" == "$published" ]] || fail "remote main is not at the original $base_tag commit"
  [[ "$(git rev-parse HEAD)" != "$published" ]] || fail "repair needs a new commit"
  git merge-base --is-ancestor "$published" HEAD || fail "repair must fast-forward the original milestone"
fi

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
if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
  [[ "$(git rev-list -n1 "$tag")" == "$(git rev-parse HEAD)" ]] || fail "tag ${tag} already exists on another commit"
else
  git tag -a "$tag" -m "$tag_message"
fi

if $repair; then
  git push --atomic origin refs/heads/main "$repair_ref"
else
  git push origin main
  git push origin "$tag"
fi
echo "publish: ${ms} pushed to ${url} (main + ${tag})"
