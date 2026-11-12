# ADR-0008: pgx + sqlc + goose (SQL-first, no ORM)

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
`pgx/v5` with `pgxpool`; queries written in SQL and generated with `sqlc` (type-safe, reviewable SQL,
easy `EXPLAIN ANALYZE`); migrations with `goose` (SQL files, embedded). Transactions via a small
`WithTx(ctx, fn)` helper that also does `SET LOCAL app.org_id`.

## Alternatives rejected
GORM/ent (hidden queries, harder to optimize and reason about locks). Raw `database/sql` everywhere
(boilerplate, no type safety).

## Consequences
Every important query has an `EXPLAIN ANALYZE` captured in the milestone report. Pool sizes are explicit
and exposed as metrics.
