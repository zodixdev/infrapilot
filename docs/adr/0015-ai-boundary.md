# ADR-0015: AI is advisory; evidence first, LLM second

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
intelligence-service (Python/FastAPI) handles `investigations.run`:
1. Collect evidence deterministically through query-service and incident-engine gRPC/REST (metrics around the
   incident window, error logs, recent deploys/changes, related incidents).
2. Run deterministic analyzers (change-point detection, correlation of signals, top error signatures).
3. Ask the LLM to summarize **only the collected evidence** into a strict JSON schema
   (hypotheses with confidence, evidence references, suggested next checks/actions).
4. Validate the schema; reject hypotheses that cite no evidence; store report + bundle (S3).

The LLM never has tools, credentials or write access. Suggested actions are shown to humans and
must go through workflow-engine policy + approval. Provider is behind an interface; a deterministic
"no-LLM" mode is the fallback (F16). Token usage and cost are metrics.

## Alternatives rejected
Autonomous agent with tool access to customer infra (unacceptable blast radius).
