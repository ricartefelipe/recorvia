SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

GO ?= go
# Pacotes Go do projeto (web/node_modules pode conter pacotes Go de dependências npm).
GO_PKGS = $(shell $(GO) list ./... | grep -v '/web/')
BIN := $(CURDIR)/bin
# Ferramentas ficam fora do repositório (o checkout pode estar em disco noexec).
TOOLS_BIN ?= $(shell $(GO) env GOPATH)/bin
export PATH := $(TOOLS_BIN):$(PATH)

# Versões fixadas das ferramentas
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
SQLC_VERSION          := v1.31.1
GITLEAKS_VERSION      := v8.30.1
TRIVY_IMAGE           := aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb

COMPOSE := docker compose
COVERAGE_THRESHOLD ?= 90

.PHONY: help
help: ## Lista os alvos disponíveis
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-22s %s\n", $$1, $$2}'

## ---------- Ferramentas ----------
.PHONY: tools tools-lint tools-vuln tools-sqlc tools-gitleaks
tools: tools-lint tools-vuln tools-sqlc tools-gitleaks ## Instala ferramentas Go fixadas em $(TOOLS_BIN)

tools-lint:
	GOBIN=$(TOOLS_BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

tools-vuln:
	GOBIN=$(TOOLS_BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

tools-sqlc:
	GOBIN=$(TOOLS_BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

tools-gitleaks:
	GOBIN=$(TOOLS_BIN) $(GO) install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

## ---------- Qualidade ----------
.PHONY: fmt
fmt: ## Formata o código Go
	gofmt -w $$(git ls-files '*.go')

.PHONY: fmt-check
fmt-check: ## Falha se houver arquivos Go não formatados
	@out=$$(gofmt -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "Arquivos não formatados:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet (unitário e integração)
	$(GO) vet $(GO_PKGS)
	$(GO) vet -tags integration $(GO_PKGS)

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run ./...

.PHONY: vulncheck
vulncheck: ## govulncheck nas dependências alcançáveis
	govulncheck $(GO_PKGS)

.PHONY: secrets-scan
secrets-scan: ## Detecção de segredos no repositório (gitleaks)
	gitleaks detect --source . --no-banner --redact --config .gitleaks.toml

## ---------- Testes ----------
.PHONY: test
test: ## Testes unitários com detector de corridas (sem Docker)
	$(GO) test -race -count=1 $(GO_PKGS)

.PHONY: coverage
coverage: ## Cobertura dos pacotes de domínio e aplicação (mínimo $(COVERAGE_THRESHOLD)%)
	$(GO) test -count=1 -covermode=atomic -coverprofile=coverage.out $(GO_PKGS)
	$(GO) run ./tools/covercheck -profile coverage.out -threshold $(COVERAGE_THRESHOLD)

.PHONY: fuzz
fuzz: ## Execução curta dos fuzz tests críticos
	@set -e; for f in $$(grep -rl --include='*_test.go' '^func Fuzz' internal); do \
	  pkg=./$$(dirname $$f); \
	  for fn in $$(grep -o '^func Fuzz[A-Za-z0-9_]*' $$f | sed 's/func //'); do \
	    echo "== $$pkg $$fn"; $(GO) test -run=NONE -fuzz="^$$fn$$" -fuzztime=$${FUZZTIME:-10s} $$pkg; \
	  done; \
	done

.PHONY: test-integration
test-integration: ## Testes de integração com PostgreSQL, RabbitMQ e Redis reais (Docker)
	$(GO) test -race -count=1 -tags integration -timeout 20m $(GO_PKGS)

## ---------- Geração de código ----------
.PHONY: sqlc
sqlc: ## Gera código a partir de db/queries
	sqlc generate

.PHONY: sqlc-check
sqlc-check: ## Verifica se o código gerado pelo sqlc está atualizado
	sqlc diff

## ---------- Build ----------
.PHONY: build
build: ## Compila os executáveis em ./bin
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/ ./cmd/...

## ---------- Ambiente local ----------
.PHONY: env
env: ## Cria .env a partir de .env.example (se ausente)
	@test -f .env || cp .env.example .env

.PHONY: web-install web-verify web-gen-check web-e2e
web-install: ## Instala as dependências da interface web (npm ci)
	cd web && npm ci

web-verify: ## Tipos, lint, testes de componente e build da interface web
	cd web && npm run verify

web-gen-check: ## Verifica se os tipos gerados do OpenAPI estão atualizados
	cd web && npm run gen:api && git diff --exit-code -- src/api/schema.d.ts

web-e2e: ## Roteiros E2E (Playwright) contra o ambiente em execução (make up)
	cd web && npx playwright test

.PHONY: up
up: env ## Sobe o ambiente local completo e carrega os dados de demonstração
	$(COMPOSE) up -d --build --wait
	$(COMPOSE) --profile seed run --rm --build seed

.PHONY: up-obs
up-obs: env ## Sobe o ambiente com OpenTelemetry Collector, Jaeger, Prometheus e Grafana
	OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 $(COMPOSE) --profile observability up -d --build --wait
	$(COMPOSE) --profile seed run --rm --build seed

.PHONY: down
down: ## Derruba o ambiente (mantém volumes)
	$(COMPOSE) --profile observability down

.PHONY: reset
reset: ## Derruba o ambiente e remove volumes
	$(COMPOSE) --profile observability down -v

.PHONY: logs
logs: ## Acompanha logs da API e do worker
	$(COMPOSE) logs -f api worker

## ---------- Operação ----------
.PHONY: obs-check
obs-check: ## Valida as configurações de observabilidade (otelcol, jaeger, promtool, dashboards)
	bash deploy/observability/validate.sh

.PHONY: obs-stack-check
obs-stack-check: ## Confere a stack em execução (make up-obs): alvos, regras, séries, dashboards e rastros
	bash deploy/observability/stack-check.sh

.PHONY: backup
backup: ## Backup lógico do PostgreSQL do ambiente (pg_dump no container) em backups/
	bash deploy/backup/backup.sh

.PHONY: restore-check
restore-check: ## Restaura o último backup (ou DUMP=arquivo) num banco novo e confere o manifesto
	bash deploy/backup/restore-check.sh $(DUMP)

.PHONY: dr-check
dr-check: ## Exercício de recuperação de desastre de ponta a ponta num projeto isolado do compose (relatório em backups/dr-check)
	bash deploy/backup/dr-check.sh

## ---------- Imagens ----------
GO_IMAGES := api worker migrate seed paysim
IMAGES    := $(addprefix recorvia-,$(GO_IMAGES)) recorvia-web recorvia-keycloak
# Etiqueta das imagens construídas e analisadas (as do compose usam "local").
IMAGE_TAG ?= local

.PHONY: images
images: ## Constrói as imagens da aplicação (api, worker, migrate, seed, paysim, web, keycloak)
	@for cmd in $(GO_IMAGES); do \
	  docker build -f deploy/docker/go.Dockerfile --build-arg CMD=$$cmd -t recorvia-$$cmd:$(IMAGE_TAG) . ; \
	done
	docker build -t recorvia-web:$(IMAGE_TAG) web
	docker build -t recorvia-keycloak:$(IMAGE_TAG) deploy/keycloak

.PHONY: image-scan
image-scan: ## Analisa as imagens com Trivy (falha em HIGH/CRITICAL com correção disponível)
	TRIVY_IMAGE=$(TRIVY_IMAGE) bash deploy/security/image-scan.sh $(addsuffix :$(IMAGE_TAG),$(IMAGES))
