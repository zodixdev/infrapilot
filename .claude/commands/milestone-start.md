---
description: Plan and start an InfraPilot milestone (e.g. /milestone-start M01)
argument-hint: <milestone id, e.g. M01>
---

Start milestone **$ARGUMENTS**.

1. Read `docs/roadmap.md` (the $ARGUMENTS section), `docs/architecture/*.md`, and every ADR relevant to its scope.
2. Inspect the current repository state (`git log --oneline -20`, tree, existing services) and confirm previous milestones are `done`.
3. Write `.local-reviews/$ARGUMENTS-plan.md` containing:
   - scope restated as concrete deliverables
   - ordered task list (small vertical steps, each ending green and committed)
   - traceability: each acceptance item and F-ID → planned test(s)
   - new decisions needed (→ ADRs to write) and new dependencies with justification
   - risks and how you will verify each
4. Set the milestone status to `in progress` in `docs/roadmap.md`.
5. Begin implementing the first task. Continue autonomously through the plan, using the review subagents as described in CLAUDE.md.
