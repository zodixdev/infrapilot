# ADR-0014: Custom durable workflow engine

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Options
1. Temporal — production-proven, but a large system to operate and hides the patterns we want to own.
2. Argo Workflows — Kubernetes-job oriented, wrong fit for remote agent actions.
3. **Custom engine:** workflow definitions (versioned, declarative YAML/JSON), executions as explicit
   state machines in Postgres, steps dispatched via outbox → RabbitMQ `workflows.step`.

## Decision
Option 3, deliberately narrow: linear + conditional steps, approvals, timeouts, retries, compensation
step on failure. Each step has an idempotency key `(execution_id, step_id, attempt)`. Policies decide
which actions need approval and which roles can approve.

## Consequences
We own correctness — covered by state-machine property tests and crash-recovery chaos tests (F14).
Revisit Temporal if workflows need long-running timers/fan-out at scale.
