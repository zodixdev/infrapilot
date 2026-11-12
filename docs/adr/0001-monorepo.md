# ADR-0001: Monorepo with go.work

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
12 deployables, shared contracts (proto/OpenAPI), one engineer + agents. Contract changes must be
atomic with the code that uses them.

## Options
1. Multi-repo per service — strong isolation, but cross-repo contract changes and versioning overhead.
2. **Monorepo, one Go module per service, `go.work` for local dev** — atomic changes, independent `go.mod` per service.
3. Monorepo, single Go module — simplest, but every service shares all dependencies and coupling grows.

## Decision
Option 2. Shared code lives in `pkg/` (its own module) and is restricted to infrastructure concerns
(config, logging, telemetry, server lifecycle, db/kafka/amqp helpers). Generated code lives in `gen/`.

## Consequences
+ Atomic contract changes, one CI, consistent tooling. − CI must detect changed services to stay fast
(path filters). Business logic must never move into `pkg/`.
