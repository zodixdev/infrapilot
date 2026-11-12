# Kickoff — starting InfraPilot with Claude Code

## 1. Prerequisites on your machine

- Docker (give it at least 8 GB RAM; 12–16 GB recommended once Kafka + ClickHouse run together)
- Go (latest stable), Node.js LTS + pnpm, Python 3.12 + uv
- kind, kubectl, helm
- Claude Code CLI (logged in)

Claude will install the rest (buf, sqlc, goose, golangci-lint, oapi-codegen, kubeconform, k6, gitleaks…) via `make bootstrap` in M01.

## 2. Set up the folder

First create an **empty** repo on GitHub under `zodixdev` (no README, no .gitignore, no license), e.g. `infrapilot`.

Option A — fresh folder from the kit:

```bash
unzip infrapilot-kit.zip && cd infrapilot
chmod +x .claude/hooks/guard-bash.sh scripts/publish.sh
git init -b main
git add -A && git commit -m "chore: architecture baseline and agent setup"
git remote add origin git@github-new:zodixdev/infrapilot.git
ssh -T git@github-new          # should greet zodixdev
git push -u origin main        # you push the baseline yourself, once
claude
```

Option B — you already cloned the empty repo:

```bash
git clone git@github-new:zodixdev/infrapilot.git && cd infrapilot
# copy the kit contents (including hidden .claude, .gitignore) into this folder
chmod +x .claude/hooks/guard-bash.sh scripts/publish.sh
git add -A && git commit -m "chore: architecture baseline and agent setup"
git push -u origin main
claude
```

After this, Claude publishes by itself at the end of each milestone with `scripts/publish.sh MXX`
(pushes `main` + tag `milestone/MXX`). It cannot run raw `git push`, change the remote, or force-push.
`publish.sh` only accepts an `origin` that starts with `git@github-new:zodixdev/` — edit `REMOTE_PATTERN`
in the script if your alias or account changes.

On first launch Claude Code asks you to trust the folder and shows the project hook — approve it.

## 3. First message to paste in Claude Code

```
You are the senior backend engineer and tech lead for InfraPilot. Read CLAUDE.md, AGENTS.md,
everything in docs/, and the .claude/agents definitions. Confirm in 10 lines what the system is,
what M01 delivers, and anything in the docs you consider inconsistent (fix it with an ADR if needed).
Then run /milestone-start M01 and work autonomously through the plan, using the review subagents.
When M01 acceptance passes, run /milestone-verify M01, /milestone-report M01 and /milestone-publish M01,
then stop and summarize.
```

## 4. Each next milestone

```
/milestone-start M02
```
…then `/milestone-verify M02`, `/milestone-report M02` and `/milestone-publish M02` when it says implementation is complete.
Start a fresh session (`/clear`) between milestones — the roadmap status and `.local-reviews/MXX-plan.md`
carry the context forward.

## 5. Tips

- Let it run long tasks; if it stops to ask for permission on routine commands, allow them for the session.
- If it ever says something "passed", ask for the exact command output — the rules require it.
- Reports and screenshots stay in `.local-reports/` (git-ignored) and are never published. Raw pushes are blocked
  by settings + a hook; only `scripts/publish.sh` can push, after all its checks pass.
- GitHub Actions (written in M01) will run on every milestone push — check the Actions tab.
- If your laptop struggles, tell it: "use only the compose profiles needed for this task".
