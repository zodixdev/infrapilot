# AGENTS.md — InfraPilot engineering rules

Permanent rules for every coding agent in this repository (Claude Code, its subagents, Codex).
Designs live in `docs/`; this file holds only stable rules.

## 1. Project

InfraPilot is a production-grade, multi-tenant infrastructure monitoring, incident management and
automation platform (Go, Python, Kafka, RabbitMQ, PostgreSQL, TimescaleDB, ClickHouse, Redis,
Kubernetes, OpenTelemetry). Read before working:

- `docs/architecture/overview.md` — services, communication, SLOs, capacity design point
- `docs/architecture/data.md` — ownership, topics, queues, delivery guarantees
- `docs/architecture/security.md` — identities, remote actions, SSRF, threat model
- `docs/architecture/failure-scenarios.md` — F01–F18, each must get a test
- `docs/adr/` — accepted decisions
- `docs/roadmap.md` — milestones, scope, acceptance criteria, current status

## 2. Authority and decisions

- The baseline architecture and ADRs are **accepted**. Implement them; do not re-litigate them.
- The project owner reviews at the end, not during. So when a decision is needed that no ADR covers:
  choose the option that best satisfies correctness → security → reliability → operability → performance → cost,
  write a new ADR (`status: accepted`, `decider: claude-senior`) with alternatives and trade-offs, and list it
  in the milestone report. Never leave a decision implicit in code.
- To deviate from an accepted ADR, write a superseding ADR first, with evidence (benchmark, failing test, incident).

## 3. Production bar

- Production quality only: no demos, no stubs, no placeholder handlers, no `TODO` instead of required behavior,
  no fake data paths, no "simplified for now" in scope items.
- Work only on the current milestone (`docs/roadmap.md`). Do not start the next one before acceptance passes.
- Every new dependency gets a one-line justification in the milestone report.
- Do not pre-create empty directories or services for future milestones.

## 4. Architecture rules

- Each service owns its database; never touch another service's tables. Cross-service data via gRPC or events.
- Public API: REST/OpenAPI (generated server). Internal: gRPC with deadlines. Agents: gRPC streams + mTLS.
- Kafka = events (facts). RabbitMQ = tasks (commands). Both via `pkg/` frameworks.
- DB change + message ⇒ transactional outbox. Consumers are idempotent (`processed_messages` or natural keys).
- All messages use the Protobuf `Envelope`; `buf breaking` must pass.
- Every network call has a timeout/deadline; context propagates end to end; retries only for idempotent operations, with backoff + jitter.
- `pkg/` contains infrastructure helpers only. No business logic shared across services.
- The gateway has no business logic. The AI service is advisory only.

## 5. Go

- Layout and libraries per ADR-0016. `cmd/` wires, `internal/domain` is pure (no I/O).
- Wrap errors with `%w`; map to gRPC/HTTP status at the transport edge only.
- Bounded concurrency only; every goroutine has an owner, a cancellation path and is covered by `goleak` in tests.
- Graceful shutdown: readiness → false, stop intake, drain with deadline, close clients in reverse order.
- `gofmt`, `golangci-lint`, `go test -race ./...` must pass. Table-driven tests. Benchmarks for hot paths.
- Optimize only with profiles (pprof/trace) attached to the report.

## 6. Python (intelligence-service)

Python 3.12+, FastAPI, Pydantic v2, uv, ruff, mypy --strict, pytest. Business logic outside handlers.
No blocking I/O in async code. LLM output is untrusted and schema-validated.

## 7. Data

- Migrations (goose) for every schema change; expand → migrate → contract; run as a Helm hook Job.
- Every tenant table has `organization_id` + RLS policy. Every service has cross-tenant tests.
- Indexes follow query patterns; record `EXPLAIN (ANALYZE, BUFFERS)` for key queries in the report.
- Define retention for every table/topic; no unbounded growth.

## 8. Security

- No secrets in code, logs, test fixtures, reports or screenshots. Config via env; local `.env` is git-ignored.
- SSRF-safe dialer for every user-supplied target. Validate all untrusted input at the edge.
- Remote actions: allow-listed types only, signed, policy-checked, approved when sensitive, audited.
- Least privilege for DB roles, Kubernetes ServiceAccounts, NetworkPolicies (default deny).

## 9. Operability (definition of a shippable service)

Dockerfile (multi-stage, distroless, non-root) · Helm chart on `infrapilot-lib` · `/healthz` `/readyz` ·
RED + runtime metrics · OTel traces across HTTP/gRPC/Kafka/AMQP · JSON logs with trace IDs ·
Grafana dashboard + Prometheus alerts in `observability/` · runbook `docs/runbooks/<service>.md`.

## 10. Testing

- Unit tests for domain logic; integration tests with real dependencies (testcontainers or compose);
  contract tests (OpenAPI/buf); e2e on kind; load (k6) and chaos where the roadmap requires.
- Each failure scenario assigned to the milestone gets an automated test referencing its ID (e.g. `TestF06_...`).
- Never claim a test, benchmark or deployment passed unless it was executed in this session; include command + output summary.
- No mocks in integration tests. Mocks are allowed only at unit level for ports you own.

## 11. Git

- Commit locally during a milestone. **Publish to GitHub only once per milestone, after it is closed**, and only with
  `scripts/publish.sh MXX` (it checks branch, remote, clean tree, local-only files, roadmap status, report,
  gitleaks and `make verify`, then pushes `main` + tag `milestone/MXX`). Raw `git push` is blocked.
- With explicit owner approval, `scripts/publish.sh MXX --repair` may publish one repair for the current
  published milestone. It applies the same checks and atomically fast-forwards `main` and creates
  `milestone/MXX-repair.1`. The original tag remains on the initial milestone commit.
- Never force-push, rewrite published history, add/change remotes, open PRs, create releases or change repo settings.
  The owner configured `origin` (e.g. `git@github-new:zodixdev/<repo>.git`); use it as is.
- If `publish.sh` fails, fix the cause; never bypass a check.
- The repository is public-facing: commit messages, docs and code must be professional and contain no secrets,
  private IPs, personal data or internal review notes.
- Conventional commits (`feat(check-runner): ...`). Small, coherent commits. Run lint + tests before committing.
- Before committing: `git status`, ensure no secrets and no `.local-*` paths are staged.
- `.local-reports/`, `.local-evidence/`, `.local-reviews/` are git-ignored and stay untracked.

## 12. Milestone report (mandatory, local only)

Write `.local-reports/milestone-XX-report.md` at the end of each milestone:

1. Summary of what was built
2. Components and key files changed
3. Decisions (link ADRs, incl. new ones), alternatives, why the choice fits InfraPilot
4. System design principles applied and the guarantees actually provided
5. Verification: exact commands and results; benchmark numbers; profiles
6. Failure scenarios tested (IDs) and observed behavior
7. Problems encountered and fixes
8. Explain-it-to-the-owner: the 3–5 most important/trickiest pieces of code, explained plainly, plus likely interview questions with answers
9. Risks, limitations, tech debt
10. New dependencies and why
11. Next milestone plan

Reports, reviews and raw screenshots are never published. Screenshots (Grafana, traces, k9s/kubectl, test output, UI) go to `.local-reports/screenshots/milestone-XX/`
with `manifest.md` (file, what it shows, how captured, verified, README candidate). Never fabricate; if
capture is impossible, write why.

## 13. Definition of done

All roadmap acceptance items pass via `make verify` (and the e2e/load/chaos targets the milestone lists) ·
lint + race clean · docs/ADRs/runbooks updated · dashboards + alerts committed · roadmap status updated ·
report written · reviews done (see CLAUDE.md) · local-only paths untracked · published with `scripts/publish.sh`.
