# ADR-0005: Transactional outbox and idempotent consumers

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
Dual writes (DB commit + broker publish) lose or duplicate messages on crashes.

## Options
1. Publish after commit — loses messages if the process dies in between.
2. **Transactional outbox + polling dispatcher** — simple, works with Postgres only.
3. CDC (Debezium) — robust but adds Kafka Connect to operate.

## Decision
Option 2. `outbox` table per service DB (`id uuidv7, destination, routing_key, headers jsonb, payload bytea,
created_at, sent_at, attempts`). Dispatcher goroutine in each replica claims batches with
`FOR UPDATE SKIP LOCKED`, publishes (Kafka `acks=all` / RabbitMQ confirms), then marks sent.
Consumers dedupe with a `processed_messages(consumer, message_id)` unique key in the same
transaction as their side effect. Metrics: outbox pending count, oldest pending age.

## Consequences
At-least-once delivery, effectively-once state changes. Outbox rows are pruned after 7 days.
Revisit CDC if outbox throughput becomes a bottleneck (measured in M10).
