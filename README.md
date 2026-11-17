# Recorvia

Plataforma SaaS B2B multitenant para gestão de assinaturas, faturamento
recorrente, cobranças, recuperação de inadimplência e conciliação de
recebíveis, voltada a prestadores de serviços recorrentes.

> Projeto em construção. A situação de cada parte está em
> [`docs/STATUS.md`](docs/STATUS.md).

## Documentação

- [Requisitos e critérios de aceite](docs/REQUISITOS.md)
- [Decisões arquiteturais](docs/DECISOES.md)
- [Status, verificações e pendências](docs/STATUS.md)
- [Guia de desenvolvimento](CLAUDE.md)
- [Fluxo de contribuição (GitFlow)](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)

## Início rápido (fundação)

Pré-requisitos: Go 1.27.4, Docker com Compose v2 e GNU Make.

```bash
make tools          # golangci-lint e govulncheck com versões fixadas
make test           # testes unitários com detector de corridas
make coverage       # cobertura dos pacotes de domínio (mínimo 90%)
make test-integration
cp .env.example .env
docker compose up -d --wait postgres
docker compose run --rm migrate-bootstrap
docker compose run --rm migrate
```
