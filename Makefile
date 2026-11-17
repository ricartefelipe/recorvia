SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c

GO ?= go
BIN := $(CURDIR)/bin
# Ferramentas ficam fora do repositório (o checkout pode estar em disco noexec).
TOOLS_BIN ?= $(shell $(GO) env GOPATH)/bin
export PATH := $(TOOLS_BIN):$(PATH)

# Versões fixadas das ferramentas
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
SQLC_VERSION          := v1.31.1
GITLEAKS_VERSION      := v8.30.1
TRIVY_IMAGE           := aquasec/trivy:0.76.2

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
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run ./...

.PHONY: vulncheck
vulncheck: ## govulncheck nas dependências alcançáveis
	govulncheck ./...

.PHONY: secrets-scan
secrets-scan: ## Detecção de segredos no repositório (gitleaks)
	gitleaks detect --source . --no-banner --redact --config .gitleaks.toml

## ---------- Testes ----------
.PHONY: test
test: ## Testes unitários com detector de corridas (sem Docker)
	$(GO) test -race -count=1 ./...

.PHONY: coverage
coverage: ## Cobertura dos pacotes de domínio e aplicação (mínimo $(COVERAGE_THRESHOLD)%)
	$(GO) test -count=1 -covermode=atomic -coverprofile=coverage.out ./...
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
	$(GO) test -race -count=1 -tags integration -timeout 20m ./...

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

.PHONY: up
up: env ## Sobe o ambiente local completo
	$(COMPOSE) up -d --build --wait

.PHONY: up-obs
up-obs: env ## Sobe o ambiente com a stack de observabilidade
	$(COMPOSE) --profile observability up -d --build --wait

.PHONY: down
down: ## Derruba o ambiente (mantém volumes)
	$(COMPOSE) --profile observability down

.PHONY: reset
reset: ## Derruba o ambiente e remove volumes
	$(COMPOSE) --profile observability down -v

.PHONY: logs
logs: ## Acompanha logs da API e do worker
	$(COMPOSE) logs -f api worker

.PHONY: image-scan
image-scan: ## Analisa as imagens locais com Trivy
	@for img in recorvia-api:local recorvia-worker:local recorvia-paysim:local recorvia-web:local; do \
	  docker run --rm -v /var/run/docker.sock:/var/run/docker.sock $(TRIVY_IMAGE) image --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed $$img; \
	done
