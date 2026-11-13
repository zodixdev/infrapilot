# InfraPilot — developer and CI entry points.
# Every target is reproducible on a clean machine after `make bootstrap`.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
MAKEFLAGS += --no-builtin-rules --no-print-directory

ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
GO ?= go
# Go-based tools are pinned as `tool` directives in tools/go.mod and run through `go tool`.
GOTOOL := $(GO) tool
GO_TOOLS := golangci-lint buf protoc-gen-go protoc-gen-go-grpc sqlc goose oapi-codegen kubeconform govulncheck gitleaks kind yq
# Go modules that contain code we lint and test (tools/ holds only tool directives): top-level modules
# (pkg, gen, tests) and one module per service under services/ (ADR-0001).
GO_MODULES := $(filter-out tools,$(patsubst %/go.mod,%,$(wildcard */go.mod services/*/go.mod)))

PROFILE ?= core
comma := ,
COMPOSE_PROFILES := $(subst $(comma), ,$(PROFILE))
# "all" selects every profile; otherwise one --profile flag per requested profile.
PROFILE_FLAGS := $(if $(filter all,$(COMPOSE_PROFILES)),--profile '*',$(foreach p,$(COMPOSE_PROFILES),--profile $(p)))
COMPOSE := docker compose --project-name infrapilot --env-file $(ROOT)/.env -f $(ROOT)/deploy/compose/docker-compose.yml
KIND_CLUSTER ?= infrapilot
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
# Unique tag per revision so `make deploy` always rolls the pods (a fixed tag + IfNotPresent would not).
# Dirty trees get a hash of the diff so two different uncommitted builds never share a tag.
GIT_REV := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)$(shell git diff --quiet HEAD 2>/dev/null || echo -dirty-$$(git diff HEAD | shasum | cut -c1-8))
CONFORMANCE_IMAGE ?= infrapilot/conformance:$(GIT_REV)
# OTLP endpoint the conformance pods push to (host compose stack). Linux CI uses the docker bridge gateway.
OTLP_ENDPOINT ?= host.docker.internal:4317
# NetworkPolicy egress for OTLP: the collector host's exact address as seen from the kind node.
OTLP_EGRESS_CIDR ?= $(shell docker exec $(KIND_CLUSTER)-control-plane getent ahostsv4 $(firstword $(subst :, ,$(OTLP_ENDPOINT))) 2>/dev/null | awk 'NR==1{print $$1"/32"}')
HELM_RELEASE ?= conformance
HELM_NAMESPACE ?= infrapilot-test

.PHONY: help bootstrap gen fmt fmt-check lint lint-go lint-proto lint-helm lint-obs lint-docker vuln db-provision deploy-only undeploy \
        test test-integration verify secrets up down ps logs cluster cluster-down image deploy e2e clean

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

bootstrap: ## Install pinned toolchain (brew packages + Go tools) and git hooks
	@command -v docker >/dev/null || { echo "docker is required (Docker Desktop with >= 8 GB RAM)"; exit 1; }
	@command -v kubectl >/dev/null || { echo "kubectl is required"; exit 1; }
	@if command -v brew >/dev/null; then brew bundle --file=$(ROOT)/Brewfile --no-upgrade; \
	 else echo "homebrew not found: install the packages listed in Brewfile manually"; fi
	@for t in $(GO_TOOLS); do printf '%-20s' "$$t"; case $$t in kubeconform) flag=-v;; sqlc|gitleaks|kind) flag=version;; *) flag=--version;; esac; \
	  v="$$($(GOTOOL) $$t $$flag 2>/dev/null | head -1)"; echo "$${v:-built}"; done
	@if [[ -d .git ]]; then pre-commit install --install-hooks >/dev/null && echo "pre-commit hooks installed"; fi

gen: ## Generate code from contracts (protobuf via buf, OpenAPI via oapi-codegen) and SQL (sqlc)
	@cd $(ROOT)/contracts && $(GOTOOL) buf generate
	@cd $(ROOT)/contracts/openapi && $(GOTOOL) oapi-codegen -config oapi-codegen.yaml public.yaml
	@gofmt -w $(ROOT)/gen/go
	@cd $(ROOT)/gen && $(GO) mod tidy
	@for c in $(wildcard $(ROOT)/services/*/sqlc.yaml); do (cd $$(dirname $$c) && $(GOTOOL) sqlc generate) || exit 1; done
	@for c in $(wildcard $(ROOT)/services/*/oapi-codegen.yaml); do (cd $$(dirname $$c) && $(GOTOOL) oapi-codegen -config oapi-codegen.yaml $(ROOT)/contracts/openapi/public.yaml) || exit 1; done

fmt: ## Format Go and protobuf sources
	@gofmt -l -w $(addprefix $(ROOT)/,$(GO_MODULES))
	@cd $(ROOT)/contracts && $(GOTOOL) buf format -w

fmt-check:
	@out="$$(gofmt -l $(addprefix $(ROOT)/,$(GO_MODULES)))"; if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@cd $(ROOT)/contracts && $(GOTOOL) buf format --diff --exit-code

lint: fmt-check lint-go lint-proto lint-helm lint-obs lint-docker ## All linters

lint-go:
	@for m in $(GO_MODULES); do echo "golangci-lint: $$m"; (cd $(ROOT)/$$m && $(GOTOOL) golangci-lint run ./...) || exit 1; done

lint-proto:
	@cd $(ROOT)/contracts && $(GOTOOL) buf lint
	@$(ROOT)/scripts/buf-breaking.sh

lint-helm:
	@$(ROOT)/scripts/helm-check.sh

lint-obs:
	@$(ROOT)/scripts/obs-check.sh

lint-docker:
	@if command -v hadolint >/dev/null; then find $(ROOT) -name Dockerfile -not -path '*/node_modules/*' -print0 | xargs -0 -r hadolint; else echo "hadolint not installed, skipping"; fi

vuln: ## govulncheck over all Go modules and the pinned tools (`tool` pattern)
	@for m in $(GO_MODULES); do echo "govulncheck: $$m"; (cd $(ROOT)/$$m && $(GOTOOL) govulncheck ./...) || exit 1; done
	@# One tool at a time: a single scan of every tool peaks near 6 GB and is OOM-killed on CI runners
	@# (measured: largest single tool, sqlc, peaks at 3.6 GB).
	@cd $(ROOT)/tools && for p in $$($(GO) list tool); do echo "govulncheck: $$p"; $(GOTOOL) govulncheck "$$p" || exit 1; done

test: ## Unit tests with the race detector
	@for m in $(GO_MODULES); do echo "go test: $$m"; (cd $(ROOT)/$$m && $(GO) test -race -count=1 -timeout 10m ./...) || exit 1; done

test-integration: ## Integration tests against real dependencies (testcontainers; needs Docker)
	@for m in $(GO_MODULES); do echo "go test (integration): $$m"; (cd $(ROOT)/$$m && $(GO) test -race -count=1 -p 1 -timeout 20m -tags integration ./...) || exit 1; done

verify: lint test test-integration ## Everything CI runs; must stay green

secrets: ## Create .env with random local secrets, or add keys missing from an existing one
	@$(ROOT)/scripts/gen-env.sh

.env:
	@$(ROOT)/scripts/gen-env.sh

up: .env ## Start compose profiles, e.g. make up PROFILE=core,obs
	@$(COMPOSE) $(PROFILE_FLAGS) up -d --wait --wait-timeout 180
	@if $(COMPOSE) --profile '*' ps --status running --services | grep -qx postgres; then $(MAKE) db-provision; fi
	@$(COMPOSE) --profile '*' ps

db-provision: .env ## (Re)apply Postgres roles and databases for INFRAPILOT_SERVICES (idempotent, ADR-0018)
	@$(COMPOSE) --profile core exec -T postgres bash /docker-entrypoint-initdb.d/01-roles.sh

down: .env ## Stop every InfraPilot compose service (VOLUMES=1 also deletes data)
	@$(COMPOSE) --profile '*' down --remove-orphans $(if $(VOLUMES),--volumes,)

ps: .env
	@$(COMPOSE) --profile '*' ps

logs: .env
	@$(COMPOSE) --profile '*' logs -f --tail=100

cluster: ## Create the local kind cluster (idempotent)
	@if $(GOTOOL) kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then echo "kind cluster $(KIND_CLUSTER) exists"; \
	 else $(GOTOOL) kind create cluster --name $(KIND_CLUSTER) --config $(ROOT)/deploy/kind/cluster.yaml --wait 120s; fi
	@kubectl --context $(KUBE_CONTEXT) get nodes

cluster-down: ## Delete the local kind cluster
	@$(GOTOOL) kind delete cluster --name $(KIND_CLUSTER)

image: ## Build the conformance image used to exercise the Helm library chart
	@docker build -f $(ROOT)/tests/platform/conformance/Dockerfile -t $(CONFORMANCE_IMAGE) $(ROOT)

deploy: image deploy-only ## Build the conformance image, install the chart (built on infrapilot-lib) into kind, run helm test

deploy-only: ## Install an already built CONFORMANCE_IMAGE (CI deploys the image it scanned) and run helm test
	@$(GOTOOL) kind load docker-image $(CONFORMANCE_IMAGE) --name $(KIND_CLUSTER)
	@kubectl --context $(KUBE_CONTEXT) create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | kubectl --context $(KUBE_CONTEXT) apply -f - >/dev/null
	@kubectl --context $(KUBE_CONTEXT) label namespace $(HELM_NAMESPACE) --overwrite \
	    pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted >/dev/null
	@helm dependency update $(ROOT)/tests/helm/conformance >/dev/null
	@helm --kube-context $(KUBE_CONTEXT) upgrade --install $(HELM_RELEASE) $(ROOT)/tests/helm/conformance \
	    --namespace $(HELM_NAMESPACE) --wait --timeout 180s \
	    --set image.repository=$(firstword $(subst :, ,$(CONFORMANCE_IMAGE))) --set image.tag=$(lastword $(subst :, ,$(CONFORMANCE_IMAGE))) \
	    --set otlp.endpoint=$(OTLP_ENDPOINT) \
	    $(if $(OTLP_EGRESS_CIDR),--set otlp.egressCIDR=$(OTLP_EGRESS_CIDR) --set 'otlp.egressExcept=null',)
	@helm --kube-context $(KUBE_CONTEXT) test $(HELM_RELEASE) --namespace $(HELM_NAMESPACE) --logs

undeploy: ## Remove the conformance release and its namespace (including the kept test database)
	@helm --kube-context $(KUBE_CONTEXT) uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE) --ignore-not-found
	@kubectl --context $(KUBE_CONTEXT) delete namespace $(HELM_NAMESPACE) --ignore-not-found --wait

e2e: ## Verify the deployed conformance service reports traces/metrics to the obs profile (needs make up PROFILE=obs)
	@cd $(ROOT)/tests && KUBE_CONTEXT=$(KUBE_CONTEXT) HELM_NAMESPACE=$(HELM_NAMESPACE) HELM_RELEASE=$(HELM_RELEASE) \
	    $(GO) test -count=1 -timeout 10m -tags e2e -run 'E2E' ./e2e/...

clean: ## Remove build artefacts
	@rm -rf $(ROOT)/bin $(ROOT)/coverage.*
