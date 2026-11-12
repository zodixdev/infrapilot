---
description: Run every acceptance check for a milestone and fix until green
argument-hint: <milestone id, e.g. M01>
---

Verify milestone **$ARGUMENTS**.

1. Launch the `test-engineer` subagent to produce the traceability table for $ARGUMENTS and fill gaps.
2. Run `make verify` plus every additional target the roadmap lists for $ARGUMENTS (e2e, load, chaos, helm tests).
3. Launch the relevant reviewers in parallel (go, distributed-systems, data, security, sre — whichever apply to the
   changed code). Save each output to `.local-reviews/$ARGUMENTS-<reviewer>.md`.
4. Fix every blocker/major/critical/high finding, re-run checks. For rejected findings, record the reason.
5. Repeat until all acceptance items pass. Print a final table: acceptance item → evidence (command + result).
Do not claim anything passed that you did not run.
