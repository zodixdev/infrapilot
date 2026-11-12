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
GO_TOOLS := golangci-lint buf protoc-gen-go protoc-gen-go-grpc sqlc goose oapi-codegen kubeconform govulncheck gitleaks kind
# Go modules that contain code we lint and test (tools/ holds only tool directives).
GO_MODULES := $(filter-out tools,$(notdir $(patsubst %/,%,$(dir $(wildcard */go.mod)))))

PROFILE ?= core
comma := ,
COMPOSE_PROFILES := $(subst $(comma), ,$(PROFILE))
COMPOSE := docker compose --project-name infrapilot --env-file $(ROOT)/.env -f $(ROOT)/deploy/compose/docker-compose.yml
KIND_CLUSTER ?= infrapilot
KUBE_CONTEXT := kind-$(KIND_CLUSTER)
CONFORMANCE_IMAGE ?= infrapilot/conformance:dev
HELM_RELEASE ?= conformance
HELM_NAMESPACE ?= infrapilot-test

.PHONY: help bootstrap gen fmt fmt-check lint lint-go lint-proto lint-helm lint-obs lint-docker vuln \
        test test-integration verify secrets up down ps logs cluster cluster-down image deploy e2e clean

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

bootstrap: ## Install pinned toolchain (brew packages + Go tools) and git hooks
	@command -v docker >/dev/null || { echo "docker is required (Docker Desktop with >= 8 GB RAM)"; exit 1; }
	@command -v kubectl >/dev/null || { echo "kubectl is required"; exit 1; }
	@if command -v brew >/dev/null; then brew bundle --file=$(ROOT)/Brewfile --no-upgrade; \
	 else echo "homebrew not found: install the packages listed in Brewfile manually"; fi
	@for t in $(GO_TOOLS); do printf '%-20s' "$$t"; v="$$( { $(GOTOOL) $$t --version || $(GOTOOL) $$t version || $(GOTOOL) $$t -v; } 2>/dev/null | head -1)"; echo "$${v:-built}"; done
	@if [[ -d .git ]]; then pre-commit install --install-hooks >/dev/null && echo "pre-commit hooks installed"; fi

gen: ## Generate code from contracts (protobuf via buf, OpenAPI via oapi-codegen)
	@cd $(ROOT)/contracts && $(GOTOOL) buf generate
	@cd $(ROOT)/contracts/openapi && $(GOTOOL) oapi-codegen -config oapi-codegen.yaml public.yaml
	@gofmt -w $(ROOT)/gen/go
	@cd $(ROOT)/gen && $(GO) mod tidy

fmt: ## Format Go and protobuf sources
	@gofmt -l -w $(addprefix $(ROOT)/,$(GO_MODULES))
	@cd $(ROOT)/contracts && $(GOTOOL) buf format -w

fmt-check:
	@out="$$(gofmt -l $(addprefix $(ROOT)/,$(GO_MODULES)))"; if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@cd $(ROOT)/contracts && $(GOTOOL) buf format --diff --exit-code

lint: fmt-check lint-go lint-proto lint-helm lint-obs lint-docker ## All linters

lint-go:
	@for m in $(GO_MODULES); do echo "golangci-lint: $$m"; (cd $(ROOT)/$$m && $(GOTOOL) golangci-lint run ./...); done

lint-proto:
	@cd $(ROOT)/contracts && $(GOTOOL) buf lint
	@$(ROOT)/scripts/buf-breaking.sh

lint-helm:
	@$(ROOT)/scripts/helm-check.sh

lint-obs:
	@$(ROOT)/scripts/obs-check.sh

lint-docker:
	@if command -v hadolint >/dev/null; then find $(ROOT) -name Dockerfile -not -path '*/node_modules/*' -print0 | xargs -0 -r hadolint; else echo "hadolint not installed, skipping"; fi

vuln: ## govulncheck over all Go modules
	@for m in $(GO_MODULES); do echo "govulncheck: $$m"; (cd $(ROOT)/$$m && $(GOTOOL) govulncheck ./...); done

test: ## Unit tests with the race detector
	@for m in $(GO_MODULES); do echo "go test: $$m"; (cd $(ROOT)/$$m && $(GO) test -race -count=1 -timeout 10m ./...); done

test-integration: ## Integration tests against real dependencies (testcontainers; needs Docker)
	@for m in $(GO_MODULES); do echo "go test (integration): $$m"; (cd $(ROOT)/$$m && $(GO) test -race -count=1 -p 1 -timeout 20m -tags integration -run 'Integration|^TestF[0-9]' ./...); done

verify: lint test test-integration ## Everything CI runs; must stay green

secrets: ## Create .env with random local secrets (never overwrites an existing one)
	@$(ROOT)/scripts/gen-env.sh

.env:
	@$(ROOT)/scripts/gen-env.sh

up: .env ## Start compose profiles, e.g. make up PROFILE=core,obs
	@COMPOSE_PROFILES=$(subst $(comma) ,$(comma),$(strip $(PROFILE))) $(COMPOSE) up -d --wait --wait-timeout 180
	@$(COMPOSE) ps

down: .env ## Stop every InfraPilot compose service (VOLUMES=1 also deletes data)
	@COMPOSE_PROFILES=all $(COMPOSE) down --remove-orphans $(if $(VOLUMES),--volumes,)

ps: .env
	@COMPOSE_PROFILES=all $(COMPOSE) ps

logs: .env
	@COMPOSE_PROFILES=all $(COMPOSE) logs -f --tail=100

cluster: ## Create the local kind cluster (idempotent)
	@if $(GOTOOL) kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER); then echo "kind cluster $(KIND_CLUSTER) exists"; \
	 else $(GOTOOL) kind create cluster --name $(KIND_CLUSTER) --config $(ROOT)/deploy/kind/cluster.yaml --wait 120s; fi
	@kubectl --context $(KUBE_CONTEXT) get nodes

cluster-down: ## Delete the local kind cluster
	@$(GOTOOL) kind delete cluster --name $(KIND_CLUSTER)

image: ## Build the conformance image used to exercise the Helm library chart
	@docker build -f $(ROOT)/tests/platform/conformance/Dockerfile -t $(CONFORMANCE_IMAGE) $(ROOT)

deploy: image ## Install the conformance chart (built on infrapilot-lib) into kind and run helm test
	@$(GOTOOL) kind load docker-image $(CONFORMANCE_IMAGE) --name $(KIND_CLUSTER)
	@helm dependency update $(ROOT)/tests/helm/conformance >/dev/null
	@helm --kube-context $(KUBE_CONTEXT) upgrade --install $(HELM_RELEASE) $(ROOT)/tests/helm/conformance \
	    --namespace $(HELM_NAMESPACE) --create-namespace --wait --timeout 180s \
	    --set image.repository=$(firstword $(subst :, ,$(CONFORMANCE_IMAGE))) --set image.tag=$(lastword $(subst :, ,$(CONFORMANCE_IMAGE)))
	@helm --kube-context $(KUBE_CONTEXT) test $(HELM_RELEASE) --namespace $(HELM_NAMESPACE) --logs

e2e: ## Verify the deployed conformance service reports traces/metrics to the obs profile (needs make up PROFILE=obs)
	@cd $(ROOT)/tests && KUBE_CONTEXT=$(KUBE_CONTEXT) HELM_NAMESPACE=$(HELM_NAMESPACE) HELM_RELEASE=$(HELM_RELEASE) \
	    $(GO) test -count=1 -timeout 10m -tags e2e -run 'E2E' ./e2e/...

clean: ## Remove build artefacts
	@rm -rf $(ROOT)/bin $(ROOT)/coverage.*
