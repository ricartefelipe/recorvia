# Fluxo de contribuição

O Recorvia adota GitFlow com pull requests obrigatórios.

## Branches

| Branch | Origem | Destino | Uso |
|---|---|---|---|
| `main` | | | versões estáveis; cada merge corresponde a uma tag `vX.Y.Z` |
| `develop` | `main` | | integração contínua do desenvolvimento |
| `feature/<descricao>` | `develop` | `develop` | funcionalidades e melhorias |
| `release/<versao>` | `develop` | `main` e `develop` | estabilização; somente correções e ajustes da versão |
| `hotfix/<versao>` | `main` | `main`, `develop` e release ativa | correções urgentes de produção |

Regras:

- Não há commits diretos em `main` ou `develop`.
- Toda integração ocorre por pull request com merge commit (equivalente a
  `--no-ff`), preservando a identificação da branch no histórico.
- Os checks da CI precisam passar antes do merge. Não se contorna proteção,
  hook ou verificação.
- Não se faz force push em branches compartilhadas nem se reescreve histórico
  publicado.
- Conflitos são resolvidos preservando as regras de negócio, e as verificações
  afetadas são executadas novamente.
- Branches `feature/*`, `release/*` e `hotfix/*` são apagadas depois do
  merge; no repositório ficam somente `main` e `develop`.
- Arquivos do ambiente de cada pessoa (configuração de editor e de ferramentas
  locais, `.env`, relatórios e caches) ficam fora do repositório, no
  `.gitignore`.

## Commits

[Conventional Commits](https://www.conventionalcommits.org/pt-br/v1.0.0/):

```
feat(billing): gera faturas atrasadas após indisponibilidade do agendador
fix(payments): impede reembolso acima do saldo elegível
docs: atualiza roteiro de demonstração
```

Tipos usados: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`,
`ci`, `chore`. Commits são pequenos, coerentes e sem alterações alheias ao seu
propósito.

## Versionamento

[Versionamento semântico](https://semver.org/lang/pt-BR/). Releases e hotfixes
recebem tags anotadas (`git tag -a v0.1.0 -m "Recorvia 0.1.0"`). O
`CHANGELOG.md` segue o formato *Keep a Changelog* e é atualizado na branch de
release.

### Release

1. `git switch develop && git switch -c release/X.Y.Z`
2. Ajustes de estabilização e atualização do `CHANGELOG.md`.
3. PR `release/X.Y.Z → main`; após checks, merge commit.
4. Tag anotada `vX.Y.Z` no merge em `main`.
5. Reintegração: avance a branch da release até o merge de `main`
   (`git switch release/X.Y.Z && git merge --ff-only origin/main`) e abra o PR
   `release/X.Y.Z → develop`. Assim `develop` contém o merge da release e nunca
   fica atrás de `main`.

### Hotfix

1. `git switch main && git switch -c hotfix/X.Y.Z`
2. Correção com teste de regressão.
3. PR para `main`, tag anotada `vX.Y.Z`.
4. Reintegração em `develop` como na release: avance `hotfix/X.Y.Z` até o
   merge de `main` e abra o PR para `develop` (e para a release ativa, se
   houver).

## Pull requests

A descrição contém objetivo, escopo, fora de escopo, evidências de validação
(comandos executados e resultados) e riscos. Os checks obrigatórios estão
listados em `docs/STATUS.md`.

## Checklist local antes do PR

```bash
make fmt-check vet lint test coverage
make test-integration   # exige Docker
make sqlc-check         # quando alterar db/queries ou migrations
```
