# ADR-0004: Kafka for events, RabbitMQ for tasks

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
Two different messaging needs:
- **High-volume facts** (telemetry, check results, state changes) consumed by several independent readers,
  needing replay and per-key ordering.
- **Work items** (send this notification, run this investigation, execute this workflow step) needing
  per-message ack, delayed retries, DLQ, and competing consumers.

## Options
1. Kafka only — retries/delays/DLQ per message are awkward; head-of-line blocking per partition.
2. RabbitMQ only — no cheap replay, weaker for 20k+ events/s fan-out to multiple consumer groups.
3. **Both, each for what it is good at.**
4. Redis Streams / NATS JetStream — viable, but fewer production references for our scale and team skills.

## Decision
Option 3. Kafka (KRaft mode, franz-go client) for `*.v1` event topics. RabbitMQ (quorum queues,
publisher confirms, amqp091-go) for task queues. Rule: **events are past-tense facts → Kafka;
commands/tasks → RabbitMQ.** Neither provides exactly-once business effects; see ADR-0005.

## Consequences
+ Right tool per pattern. − Two brokers to operate and monitor (lag, queue depth, DLQ size dashboards required).
