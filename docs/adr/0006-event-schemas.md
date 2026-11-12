# ADR-0006: Protobuf event schemas managed with buf

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
All Kafka events and RabbitMQ tasks are Protobuf messages inside a common `Envelope`
(see `docs/architecture/data.md` §6). Schemas live in `contracts/proto/infrapilot/<domain>/v1`.
`buf lint` and `buf breaking --against` the main branch run in CI. Breaking change ⇒ new `v2` message/topic.

## Alternatives rejected
JSON (no enforced schema, larger payloads). Avro + Schema Registry (extra infrastructure; we already
use Protobuf for gRPC, one IDL is simpler).

## Consequences
Consumers must ignore unknown fields and handle all supported `schema_version`s.
