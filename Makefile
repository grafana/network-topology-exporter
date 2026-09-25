SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BINARY      := topology-exporter
PKG         := github.com/grafana/network-topology-exporter
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
               -X $(PKG)/internal/version.Version=$(VERSION) \
               -X $(PKG)/internal/version.Commit=$(COMMIT)   \
               -X $(PKG)/internal/version.BuildDate=$(DATE)

.PHONY: help
help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into bin/
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/topology-exporter

.PHONY: build-hub
build-hub: ## Build the federation-hub binary (federation.role: hub) into bin/
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/topology-hub ./cmd/topology-hub

.PHONY: run
run: build ## Build and run with the example config
	./bin/$(BINARY) --config.file=config/example.yaml

.PHONY: test
test: ## Run unit tests
	go test ./... -race -count=1

.PHONY: cover
cover: ## Generate coverage report
	go test ./... -race -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

.PHONY: lint
lint: ## golangci-lint
	@command -v golangci-lint >/dev/null || { echo "install: https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run ./...

.PHONY: bench
bench: ## Run Go benchmarks (compare across commits with benchstat)
	go test -run='^$$' -bench=. -benchmem -count=6 ./...

.PHONY: licenses
licenses: ## Check dependency licenses (forbidden/unknown fail)
	@command -v go-licenses >/dev/null || go install github.com/google/go-licenses@latest
	go-licenses check ./... --disallowed_types=forbidden,unknown --ignore github.com/grafana/network-topology-exporter

.PHONY: fmt
fmt: ## Format
	gofmt -s -w .
	@command -v goimports >/dev/null && goimports -w . || true

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: docker
docker: ## Build the container image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t network-topology-exporter:$(VERSION) \
	  -t network-topology-exporter:dev \
	  .

.PHONY: docker-hub
docker-hub: ## Build the federation-hub container image (see Dockerfile.hub)
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -f Dockerfile.hub \
	  -t network-topology-hub:$(VERSION) \
	  -t network-topology-hub:dev \
	  .

.PHONY: test-integration
test-integration: ## Run integration tests (requires no extra dependencies)
	go test ./tests/integration/... -tags integration -race -count=1 -timeout 60s

.PHONY: e2e-image
e2e-image: ## Build the lightweight test-node image (Alpine + lldpd + snmpd)
	docker build -t nte-testnode:latest tests/e2e/testnode/

.PHONY: test-e2e
test-e2e: ## Run e2e tests against a live containerlab topology (requires Docker + containerlab)
	CLAB_DOCKER=1 go test ./tests/e2e/... -tags e2e -v -count=1 -timeout 15m

.PHONY: test-e2e-srl
test-e2e-srl: ## Run SR Linux e2e tests (requires Docker + containerlab + x86; 26.3.2+ for LLDP SNMP)
	CLAB_SUDO=1 go test ./tests/e2e/srl/... -tags e2e_srl -v -count=1 -timeout 20m

.PHONY: lint-scripts
lint-scripts: ## Run shellcheck on colleague-capture scripts and ruff on the redactor
	@set -e; \
	files=""; \
	[ -f scripts/colleague-capture.sh ] && files="$$files scripts/colleague-capture.sh"; \
	for f in scripts/colleague-capture-lib/*.sh lab/*/colleague-capture.sh; do \
	  [ -f "$$f" ] && files="$$files $$f"; \
	done; \
	if [ -n "$$files" ]; then shellcheck $$files; fi
	@if [ -f scripts/redact-snmp-capture.py ]; then ruff check scripts/redact-snmp-capture.py; fi

.PHONY: test-scripts
test-scripts: ## Run bats tests for shell libs
	bats tests/scripts/test_lib_*.bats

.PHONY: test-redactor
test-redactor: ## Run pytest for the redactor
	pytest tests/scripts/test_redact_snmp_capture.py -v

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out coverage.html

# ─── Test Harness ───────────────────────────────────────────────────────────
GRAFANA_URL   ?= https://networko11ydev.grafana.net
GRAFANA_TOKEN ?= $(shell cat $(HOME)/Code/grafana/network-o11y-demo/grafana-cloud-api.token 2>/dev/null || echo "MISSING_TOKEN")

.PHONY: dashboards-apply
dashboards-apply: ## Apply test-harness dashboards to Grafana Cloud using grafana-cli
	@if [ "$(GRAFANA_TOKEN)" = "MISSING_TOKEN" ]; then \
		echo "Error: Provisioning token not found at ~/Code/grafana/network-o11y-demo/grafana-cloud-api.token"; \
		exit 1; \
	fi
	@command -v grafana-cli >/dev/null || { echo "Error: grafana-cli not found. Install it from: https://grafana.com/docs/grafana/latest/as-code/observability-as-code/grafana-cli/"; exit 1; }
	@for file in dashboards/test-harness/*.json; do \
		echo "Applying $$file..."; \
		grafana-cli dashboards apply --url $(GRAFANA_URL) --token $(GRAFANA_TOKEN) --folder "topology-exporter-testing" $$file; \
	done
