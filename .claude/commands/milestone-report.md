---
description: Write the local-only milestone report and screenshot manifest, then close the milestone
argument-hint: <milestone id, e.g. M01>
---

Close milestone **$ARGUMENTS**.

1. Write `.local-reports/milestone-<NN>-report.md` with every section required by AGENTS.md §12, using real
   command output from this milestone (tests, benchmarks, profiles, EXPLAIN plans, review findings).
2. Capture genuine screenshots where possible (Grafana dashboards, Tempo traces, `kubectl get pods` / k9s, test output,
   UI pages) into `.local-reports/screenshots/milestone-<NN>/` and write `manifest.md`. If a capture is impossible,
   record why. Never fabricate.
3. Confirm `git check-ignore .local-reports .local-reviews .local-evidence` reports all three, and `git status`
   shows none of them.
4. Update `docs/roadmap.md` status to `done`, update `docs/adr/README.md` with any new ADRs.
5. Make a final local commit. Then run `/milestone-publish $ARGUMENTS`.
