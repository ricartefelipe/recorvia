# Recorvia — interface web

SPA em React + TypeScript que consome a API do Recorvia descrita em
`../api/openapi.yaml`. Interface em português do Brasil; identificadores de
código em inglês.

## Comandos

Requer Node 22 (a imagem Docker usa `node:22.23.5-alpine3.24`).

| Comando | Uso |
|---|---|
| `npm ci` | instala exatamente o que está no `package-lock.json` |
| `npm run dev` | Vite em `http://localhost:5173`, com proxy de `/api`, `/auth`, `/portal/access` e `/webhooks` para `http://localhost:8081` (altere com `RECORVIA_API_URL`) |
| `npm run build` | `tsc --noEmit` e build de produção em `dist/` |
| `npm run typecheck` | verificação de tipos |
| `npm run lint` | ESLint 9 com typescript-eslint e regras de hooks |
| `npm test` / `npm run test:watch` | Vitest + Testing Library (jsdom; MSW só nos testes) |
| `npm run e2e` | Playwright contra o stack completo em `E2E_BASE_URL` (padrão `http://localhost:8080`) |
| `npm run gen:api` | regenera `src/api/schema.d.ts` a partir de `../api/openapi.yaml` |
| `npm run verify` | typecheck, lint, testes e build |

Pipeline sugerido para CI:

```sh
npm ci
npm run gen:api && git diff --exit-code src/api/schema.d.ts   # contrato e tipos em sincronia
npm run verify
npx playwright install --with-deps chromium && npm run e2e     # apenas com o stack completo no ar
```

Os E2E (`e2e/`) seguem o roteiro demonstrativo: login no Keycloak como
`ana.owner@acme.test`, cadastro, faturamento e pagamento pelo checkout do
simulador; isolamento entre Acme e Beta; auditor sem botões de escrita. Eles não
sobem servidores: o ambiente precisa estar rodando. `npx playwright test --list`
valida a compilação sem backend.

### Discos sem symlinks

`npm ci` cria symlinks em `node_modules/.bin`. Em discos sem suporte a symlinks
ou sem permissão de execução (vfat/exFAT, montagens `noexec`), use uma worktree
git em outro sistema de arquivos (por exemplo ext4 em `/tmp`) ou rode o build
via Docker (`docker build -t recorvia-web:local web`).

## Docker e nginx

`Dockerfile` multiestágio: build em `node:22.23.5-alpine3.24` (`npm ci && npm
run build`) e imagem final `nginxinc/nginx-unprivileged:1.31.7-alpine3.24`,
sem root (usuário 101), na porta 8080, com `/healthz`.

`nginx.conf`:

- SPA com fallback para `index.html`; `/assets/` nunca cai no fallback.
- Proxy de `/api/`, `/auth/`, `/portal/access` e `/webhooks/` para
  `http://api:8081`, preservando `Host` e `X-Forwarded-*`, sem buffering de
  requisição nem de resposta e com corpo limitado a 1 MB. O nome `api` é
  resolvido pelo DNS do Docker (`127.0.0.11`) a cada requisição; fora do Docker
  Compose ajuste o `resolver`.
- `Cache-Control: public, max-age=31536000, immutable` para assets com hash e
  `no-cache` para `index.html` e rotas da SPA; respostas da API mantêm os
  cabeçalhos do backend.
- Segurança: CSP sem scripts inline (`script-src 'self'`), `frame-ancestors
  'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`
  (links de convite e do portal levam tokens na URL), `X-Frame-Options`,
  `Cross-Origin-Opener-Policy` e `Permissions-Policy`.
  `style-src` inclui `'unsafe-inline'` porque o Mantine injeta variáveis CSS em
  `<style>` e usa atributos `style` em tempo de execução, e o documento HTML da
  fatura (exibido em iframe `sandbox` sem scripts) usa estilos inline. Não há
  nenhuma exceção para scripts.

## Estrutura

```
src/
  api/          cliente HTTP, tipos gerados do contrato, fábrica de chaves
  app/          provedores, roteador, AppShell, navegação, sessão
  components/   estados (carregando, vazio, erro, acesso negado, não encontrado),
                formatação, confirmação, segredos exibidos uma vez, documento
  lib/          dinheiro, datas, documentos, idempotência, erros, validação
  session/      contexto da sessão, permissões e troca de organização
  features/     uma pasta por área (clientes, catálogo, assinaturas, faturas,
                pagamentos, ledger, conciliação, cobrança, usuários,
                integrações, auditoria, operações, plataforma, organização, portal)
  test/         setup do jsdom, servidor MSW, fixtures e render com provedores
e2e/            Playwright
```

## Decisões

- **Stack e versões** fixadas sem `^`/`~` (`.npmrc` com `save-exact`). O
  `.npmrc` também usa `legacy-peer-deps=true`: o vitest 4.1 declara peers
  opcionais circulares (`@vitest/ui`, `@vitest/browser-*` → `vitest`) que
  derrubam o resolvedor do npm 9/10 com `Cannot read properties of null
  (reading 'edgesOut')`. Todas as peers necessárias estão declaradas
  explicitamente. O lockfile (v3) foi gerado com npm 10.9, o mesmo da imagem
  Node 22.
- **Cliente HTTP central** (`src/api/http.ts`): interpreta
  `application/problem+json` em `ApiError` (`code`, `detail`, `request_id`,
  `errors[]`); envia `X-CSRF-Token` em todo método não-GET e
  `X-Recorvia-Tenant` com a organização ativa. Os endpoints da própria sessão
  (`GET /api/v1/session` e `PUT /api/v1/session/tenant`) não enviam o header de
  organização, porque são eles que a definem. 401 leva o navegador inteiro a
  `/auth/login?return_to=<rota>`; 409 `tenant_mismatch` cancela e limpa o cache
  e recarrega a sessão.
- **Cache isolado por organização**: toda chave começa com `[userId, tenantId]`
  (`createQueryKeys`). Trocar de organização faz `PUT /api/v1/session/tenant`,
  `cancelQueries()`, `clear()`, recarga da sessão e navegação para o painel; a
  área interna é remontada com `key` do escopo para não sobrar estado local.
- **Erros**: `errors[]` vai para `setError` do React Hook Form; `detail` em
  notificação; `request_id` aparece em 5xx; 403 mostra acesso negado, 404 não
  encontrado e 412 abre o aviso de alteração concorrente com “Recarregar dados”.
- **Idempotência**: `useIdempotencyKey` gera `crypto.randomUUID()` por intenção
  e reutiliza a mesma chave enquanto os dados enviados forem os mesmos
  (repetição após falha de rede); muda a chave quando os dados mudam e após
  sucesso.
- **Dinheiro** sempre em centavos inteiros. Exibição com
  `Intl.NumberFormat('pt-BR', {style: 'currency'})` alimentado por texto decimal
  exato (sem `float`); entrada “1.234,56” convertida com aritmética de texto e
  `BigInt`. Preços unitários (`unit_amount_decimal`) aceitam frações de centavo.
- **Datas**: instantes no fuso `active_tenant.timezone`; datas civis
  (`AAAA-MM-DD`) exibidas sem conversão. Campos de data e hora digitados são
  convertidos do fuso da organização para UTC.
- **Permissões** da sessão escondem ou desabilitam ações (`<Can>` e
  `usePermission`); o backend continua sendo a autoridade.
- **Acessibilidade**: link para pular ao conteúdo, foco no conteúdo a cada troca
  de página, foco preso e devolvido nos modais (Mantine), `aria-label` em botões
  só com ícone, região `aria-live` que repete as notificações, rótulos e
  mensagens de validação associados aos campos.
- **Portal do cliente** em `/portal`, com layout, cliente HTTP e sessão próprios
  (`/api/portal/v1/*` com o CSRF da sessão do portal). Sem sessão, orienta o
  acesso pelo link recebido em vez de levar ao login interno.
- **Documento da fatura** exibido em iframe `sandbox="allow-same-origin
  allow-modals"` (sem scripts), com imprimir e baixar.
