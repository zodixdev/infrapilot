# ADR-0002: Service boundaries and database ownership

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Context
Domains differ in load profile (ingestion: very high write throughput; control plane: low-volume,
strongly consistent), failure tolerance and scaling needs.

## Options
1. Modular monolith — simplest ops, but ingestion load and control-plane availability are coupled.
2. **Microservices split by bounded context and load profile** (see overview §3).
3. Finer split (e.g. separate RBAC service, separate scheduler) — more network hops without a clear benefit.

## Decision
Option 2 with 12 deployables. Each owns one PostgreSQL database and role. Read-heavy analytics
are isolated in query-service so ingestion writers and API readers scale independently.
The gateway contains no business logic.

## Consequences
+ Independent scaling/failure domains; clear ownership. − Distributed consistency handled via
outbox/events; more operational surface, mitigated by one Helm library chart and one Go service template.
