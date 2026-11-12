# Event contracts

Protobuf definitions live in `contracts/proto/infrapilot/<domain>/v1/`. This file documents conventions.

- Every message on Kafka or RabbitMQ is an `infrapilot.events.v1.Envelope`.
- `event_id`: UUIDv7, unique per event, used for dedup.
- `event_type`: `<domain>.<entity>.<past-tense-verb>` for events (e.g. `incident.incident.opened`),
  `<domain>.<verb>` for tasks (e.g. `notifications.deliver`).
- `schema_version`: integer, starts at 1. Consumers must support all versions still produced.
- `organization_id`: always set for tenant data.
- `trace_parent`: W3C traceparent of the producing span; also set as a Kafka/AMQP header.
- Kafka key: see `docs/architecture/data.md` §3. Ordering is per key only.
- Adding optional fields is non-breaking. Removing/renaming/retyping fields is breaking → new `v2`.
- `buf breaking` against the main branch blocks breaking changes in CI.
