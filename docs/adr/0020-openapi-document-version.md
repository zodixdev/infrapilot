# ADR-0020: Public OpenAPI document stays on 3.0.3 until the generator proves 3.1 support

- Status: accepted (partially supersedes ADR-0003 — the "OpenAPI 3.1" clause only) · Decider: claude-senior · Date: 2026-11-12

## Context
ADR-0003 specifies "REST + OpenAPI 3.1" and requires server code to be generated from the document with
oapi-codegen (strict server) so spec and code cannot drift. The two requirements conflict today:
the pinned oapi-codegen v2.8.2 accepts an `openapi: 3.1.0` document with exit code 0 but **silently drops every
schema that uses 3.1 syntax** (type arrays such as `type: [string, "null"]`, `const`, numeric `exclusiveMinimum`).
Reproduced in M01: the skeleton converted to 3.1 lost `PageInfo`; a document with only 3.1 keywords generated an
empty package. Evidence is kept in the M01 local evidence folder and summarized in the M01 report.

A generator that drops schemas without error is a correctness hazard: a field could disappear from the Go types
while the published contract still advertises it.

## Options considered
1. **Author the document as OpenAPI 3.0.3** — fully supported by oapi-codegen and the widest tool ecosystem
   (validators, SDK generators, API gateways). Loses 3.1 niceties (`webhooks`, full JSON Schema 2020-12, `const`,
   type arrays); nullability is expressed with `nullable: true`.
2. Author 3.1 and down-convert to 3.0 at generation time — an extra tool in the pipeline, lossy for the very
   features that motivate 3.1, and the published document would differ from the generated one.
3. Switch generators to one with native 3.1 support — ADR-0003/0016 chose oapi-codegen's strict server
   deliberately (spec/code coupling, stdlib router); other generators are either not strict or not stdlib-based.
   Re-evaluate when oapi-codegen's 3.1 support emits every schema.

## Decision
Option 1. `contracts/openapi/public.yaml` declares `openapi: 3.0.3`. Nullability uses `nullable: true`.
`make gen` regenerates the shared types and CI fails if the generated code is stale (`git diff --exit-code`).

## Consequences
- Good: generated code is complete and the document validates with every mainstream OpenAPI 3.0 tool.
- Bad / trade-offs we accept: no `webhooks` object (signed webhooks are documented in a separate 3.0 document when
  M04 introduces them), JSON Schema draft-04-style keywords only.
- Verification: a repository test regenerates from the committed document and compares; the upgrade trigger is a
  passing run of the M01 3.1 probe documents through the pinned generator with **all** schemas present.

## Interview one-liner
A generator that silently loses schemas on 3.1 is a contract bug waiting to happen, so we ship 3.0.3 and gate the
upgrade on a reproducible generator test, not on the spec's headline version.
