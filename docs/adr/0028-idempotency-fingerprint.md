# ADR-0028: Fingerprint creates from the control-plane service input

- Status: accepted (partially supersedes ADR-0024, request fingerprint encoding) · Decider: claude-senior · Date: 2026-11-15

## Context

ADR-0024 specifies a canonical Protobuf encoding for idempotent creates. M02 fingerprints the typed inputs
that control-plane receives after converting the gRPC request. Existing fingerprints may still be live
for 24 hours in an M02 development database. A retry must compare against the same representation as the
original request.

## Options considered

1. Fingerprint deterministic Protobuf in the gRPC adapter. This would couple the persisted fingerprint to
   the transport schema and require a compatibility path for fingerprints already stored by M02.
2. Fingerprint deterministic JSON of the typed service input. The service can compute the fingerprint where
   it enforces idempotency. The input types and the encoding become part of the persisted format for the
   lifetime of a record.

## Decision

Use option 2. For control-plane, the request hash is SHA-256 over the operation name, one zero byte, and
the Go `encoding/json` encoding of the typed input. JSON object keys originating from Go maps are sorted by
the encoder. For project-scoped resource creation, the operation name includes the target project ID.
Preserve the existing input shapes and encoding for live records. Any future change that affects these
bytes must support comparison with the previous encoding for at least the idempotency window.

## Verification

`TestIdempotencyFingerprintCompatibility` locks down the byte representation for each create operation.
The service and API integration tests exercise same-key replay and a changed input conflict against PostgreSQL.
