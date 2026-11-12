---
description: Publish a closed milestone to GitHub (main + milestone tag)
argument-hint: <milestone id, e.g. M01>
---

Publish milestone **$ARGUMENTS** to GitHub.

1. Confirm the milestone is closed: roadmap status `done`, report written, `/milestone-verify` passed.
2. Review what will be published: `git log origin/main..main --oneline` (or the full log on first publish) and
   `git diff --stat origin/main..main`. Make sure there are no secrets, private IPs, personal data, review notes
   or `.local-*` files, and that README/docs reflect the current state (update the README status line and
   feature list for $ARGUMENTS if needed, commit).
3. Run `scripts/publish.sh $ARGUMENTS`.
4. If it fails, fix the root cause and re-run. Never bypass a check, never force-push, never change the remote.
5. Print a short summary for the owner: what was published, the tag, and the commit range.
