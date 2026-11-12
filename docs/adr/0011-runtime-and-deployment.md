# ADR-0011: Kubernetes + Helm in every environment

- Status: accepted · Decider: senior-backend · Date: 2026-11-12

## Decision
- **Local:** `kind` cluster created by `make cluster`; InfraPilot services deployed with the same Helm charts as prod.
  Stateful dependencies run via docker compose (profiles per milestone) for fast iteration, **and** have
  in-cluster equivalents for e2e/chaos runs.
- **Prod/staging:** Kubernetes provisioned with Terraform (provider decided in M10 ADR), operators:
  CloudNativePG (Postgres + Timescale image), Strimzi (Kafka), RabbitMQ Cluster Operator,
  Altinity ClickHouse Operator, cert-manager, External Secrets, ingress-nginx.
- One Helm **library chart** (`deploy/helm/infrapilot-lib`) defines Deployment, Service, PDB, HPA,
  probes, resources, NetworkPolicy, ServiceMonitor; each service chart just sets values.
- CI: GitHub Actions — lint, test (with `-race`), integration (testcontainers), buf breaking,
  govulncheck, gitleaks, image build + trivy, Helm lint + kubeconform, kind e2e.

## Alternatives rejected
Compose-only deployment (not production-representative). Raw manifests (duplication). ArgoCD now
(GitHub only receives milestone-level pushes, so GitOps adds little before a real cluster exists — revisit at M10).
