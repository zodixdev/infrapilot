# ADR-0017: Next.js dashboard

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
Next.js (App Router) + TypeScript (strict), Auth.js with the Keycloak provider, server-side data fetching
from api-gateway only, TanStack Query for client state, a typed API client generated from
`contracts/openapi/public.yaml`, Playwright e2e tests, pnpm. No direct access to internal services.

## Alternatives rejected
Vite SPA (would expose tokens to the browser and needs a separate BFF). Separate Node BFF (duplicate of gateway).
