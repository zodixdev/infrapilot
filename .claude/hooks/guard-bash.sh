#!/usr/bin/env bash
# Guards Bash tool calls. Claude Code passes the call as JSON on stdin;
# exit code 2 blocks it and sends stderr back to Claude.
# Publishing is allowed only through scripts/publish.sh (its internal git push is not a tool call).
input="$(cat)"

if printf '%s' "$input" | grep -Eq 'git([[:space:]]+(-[Cc][[:space:]]+[^[:space:]]+|--?[^[:space:]]+))*[[:space:]]+push'; then
  echo "Blocked: raw git push is not allowed. Publish a closed milestone with: scripts/publish.sh MXX (AGENTS.md §11)." >&2
  exit 2
fi

if printf '%s' "$input" | grep -Eq 'git[[:space:]]+remote[[:space:]]+(add|set-url|remove|rm|rename)|gh[[:space:]]+(pr|repo|release)[[:space:]]+(create|edit|delete)|git[[:space:]]+tag[[:space:]]+(-d|--delete)'; then
  echo "Blocked: changing remotes, repo settings, releases or deleting tags is not allowed." >&2
  exit 2
fi

if printf '%s' "$input" | grep -Eq 'git[[:space:]]+add[^"]*(\.local-(reports|reviews|evidence)|[[:space:]](-f|--force)([[:space:]]|"|$))'; then
  echo "Blocked: local-only evidence must never be staged, and force-adding ignored files is not allowed." >&2
  exit 2
fi

exit 0
