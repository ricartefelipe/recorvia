# Segurança: modelo de ameaças e revisão antes da abertura

Este documento registra a revisão de segurança feita antes de o Recorvia ser aberto na internet, sobre o
`develop` que já inclui o pacote de produção (`deploy/producao`, ADR-086). Ele resume o modelo de ameaças, o
que foi verificado, o resultado, as correções e os riscos que continuam abertos. As decisões estão na
ADR-089; as anteriores que ela confirma são citadas em cada tema. Não é um teste de intrusão externo nem uma
certificação: é a revisão do código, da configuração e da borda, com testes automatizados que reproduzem os
ataques.

## Modelo de ameaças resumido

**O que proteger.** Dados financeiros de cada organização (faturas, pagamentos, ledger, conciliação), dados
pessoais dos clientes delas (nome, documento, e-mail, endereço), segredos de integração (chaves e tokens de
provedores de pagamento, segredos de webhooks, credenciais de API), sessões de usuários e do portal, e a
disponibilidade do faturamento.

**Quem ataca e de onde.**

| Ator | Alcança | Objetivo típico |
|---|---|---|
| Anônimo na internet | origem da aplicação (Caddy, nginx, API), domínio do Keycloak, `/webhooks/providers/...` | entrar numa conta, abusar de rotas públicas, esgotar recursos |
| Usuário autenticado de outra organização | toda a API com uma sessão válida | ler ou alterar dados de outra organização (IDOR, cursores, corpos) |
| Membro com papel limitado | rotas da própria organização | ir além do papel (operador que reembolsa, auditor que escreve) |
| Integração com credencial de API vazada | rotas permitidas pelos escopos | ampliar escopo, trocar de organização |
| Cliente final no portal | rotas do portal | ver faturas de outro cliente ou de outra organização |
| Quem forja eventos do provedor | `/webhooks/providers/{provider}/{token}` | marcar fatura como paga sem pagamento |
| Dono de um destino de webhook | o worker, pelas entregas | alcançar a rede interna (SSRF) |
| Quem induz o navegador de um usuário | qualquer site | agir com a sessão dele (CSRF, login CSRF, clickjacking) |

**Fronteiras.** Só o Caddy publica portas (80 e 443). Atrás dele, o nginx da imagem web serve a SPA e
encaminha `/api`, `/auth`, `/portal/access`, `/webhooks` e `/livez` à API; PostgreSQL, Redis e RabbitMQ ficam na
rede `data`, sem saída para a internet. O worker sai para a internet (provedor, SMTP, webhooks enviados) pela
rede `app`. Toda transação de dados de organização passa por `InTenantTx` (RLS com `set_config` local e filtro
`tenant_id` explícito, ADR-004).

## O que foi verificado e o resultado

Severidade: **alta** (explorável remotamente com dano direto), **média** (explorável com condição ou dano
limitado), **baixa** (endurecimento, sem exploração conhecida).

### 1. Isolamento entre organizações

| Verificação | Resultado |
|---|---|
| Consultas do sqlc sem `tenant_id` | Só as globais esperadas: usuários, outbox, agendador, catálogo da plataforma, descoberta de tenants com trabalho e as que localizam o tenant por hash de token (credencial, link do portal, convite, endpoint de webhook recebido). Todas as demais filtram `tenant_id`. |
| Transações fora de `InTenantTx` | Só `inGlobal` para as consultas acima; nenhuma lê dados de organização sem o contexto dela. |
| RLS | Toda tabela com `tenant_id` tem RLS e política (`TestEveryTenantTableHasRLS`); chaves estrangeiras compostas impedem relação entre organizações (`TestCompositeForeignKeysBlockCrossTenantRelations`). |
| Workers e consumidores | O tenant da mensagem vem da outbox gravada na transação da organização (`Emit` recusa evento de outro tenant); cada consumidor abre `InTenantTx` com esse tenant e carrega os recursos por identificador dentro dele. |
| Referência direta por identificador, corpo e cursor | **Sem achado.** Faltava teste para a maior parte das rotas; foram criados `TestCrossTenantReferencesOverHTTP` (cadastro, contatos, endereço, créditos, links do portal, produtos, planos e preços, assinaturas e ciclo de vida, faturas, documento, pagamento manual, cobrança, membros, convites, credenciais, endpoints e entregas de webhook, régua, assinatura e troca de plano com referências de outra organização, consumo com assinatura alheia, cursor de outra organização, credencial de API com o cabeçalho de outra organização) e `TestCrossTenantReceivablesOverHTTP` (cobrança, reembolso, disputa, baixa do valor a recuperar, reexecução de operação). Todas respondem 404 (ou 422 no corpo) e nada da outra organização muda. |
| Portal do cliente | Já coberto (`TestPortalCustomerFlowOverHTTP`): só faturas emitidas do cliente do link; outra organização e outro cliente respondem 404. |

### 2. Autenticação e sessão

| Verificação | Resultado |
|---|---|
| Cookies | Sessão `__Host-recorvia_session` (HttpOnly, Secure, `SameSite=Lax`, `Path=/`), portal `__Host-recorvia_portal` (`SameSite=Strict`); produção recusa subir sem `Secure` e sem o prefixo `__Host-`. |
| CSRF | Token por sessão em `X-CSRF-Token`, comparado em tempo constante, e conferência de `Origin` em todo método que altera estado, no painel e no portal; credenciais Bearer não usam cookie e dispensam o token. |
| Fixação e rotação | Login cria sessão nova; troca de organização e criação de organização rotacionam identificador e token CSRF; revogação por geração (ADR-078). |
| **Login CSRF** | **Achado, severidade média, corrigido.** O `state` do OIDC não estava ligado ao navegador: quem atacava iniciava um login, entrava no provedor com a própria conta e levava a vítima ao callback com o `state` e o código dele; o navegador dela recebia uma sessão na conta de quem atacou. Agora cada login grava um cookie de vínculo próprio, com nome derivado do `state` (`__Host-recorvia_session_login_<hash>`, HttpOnly, `SameSite=Lax`, 10 minutos), e o callback exige o mesmo valor; o `state` recusado fica inutilizado, o código não é trocado e o cookie do fluxo é removido em qualquer desfecho. Abas simultâneas não se sobrescrevem. Testes: `TestCompleteLoginRequiresSameBrowser`, `TestBeginLoginBindsBrowser`, `TestLoginCookiePerFlow`, `TestLoginCallbackRequiresTheBrowserThatStartedIt` e `TestLoginInConcurrentTabs` (este com duas abas em paralelo, sem cookie inicial, num navegador simulado que aplica as remoções de cookie). |
| OIDC | Authorization Code com PKCE S256, `state` e `nonce` aleatórios de 256 bits e de uso único, ID token só RS256 com emissor, audiência e expiração, `redirect_uri` fixo da configuração e exato no realm, e-mail verificado exigido, retorno após o login só para caminho relativo. |
| Credenciais de API | Token `rcv_` com segredo de 256 bits, guardado como SHA-256, comparação em tempo constante, escopos limitados ao papel de quem emitiu, tenant da credencial (cabeçalho divergente responde 409), rotação e revogação (ADR-046 a ADR-049). |
| Links do portal | Uso único, validade de 72 horas, hash do token no banco, revogação, sessão própria (ADR-050, ADR-068). |
| Limites de tentativas | Login e callback: 120 por IP por minuto. Troca de link do portal: 30 por IP por minuto e bloqueio após 10 falhas em 15 minutos. Credenciais: bloqueio após 30 falhas em 10 minutos por IP. Webhooks recebidos: 1200 por IP por minuto. Senhas: bloqueio temporário do Keycloak após 5 falhas. Demais rotas autenticadas: limite por usuário ou credencial. |

### 3. Cabeçalhos e borda

| Verificação | Resultado |
|---|---|
| nginx (`web/nginx.conf`) | CSP com `script-src 'self'`, `frame-ancestors 'none'`, `object-src 'none'`, `base-uri 'self'`, `form-action 'self'`; `nosniff`, `Referrer-Policy`, `X-Frame-Options`, COOP, `Permissions-Policy`, `server_tokens off`, corpo de 1 MiB, endereço do cliente só de proxies confiáveis. Alterado só para ocultar os cabeçalhos duplicados da API (abaixo). |
| Caddy (`deploy/producao/caddy/Caddyfile`) | HTTPS automático, HSTS de 2 anos, sem `Server`/`Via`, sem log de acesso (token do portal), domínio do Keycloak com lista do que é permitido e console restrito por CIDR. Sem alteração. |
| CSP na API | **Endurecimento, severidade baixa, corrigido.** As respostas da API não tinham CSP quando alcançadas sem o nginx. Agora levam `default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'` (o documento da fatura mantém a própria política). Atrás do nginx, `Referrer-Policy`, `X-Frame-Options` e `X-Content-Type-Options` saíam duplicados; o nginx oculta os da API e cada um sai uma vez. Testes: `TestSecurityHeadersOnEveryResponse` e a etapa 6 de `make prod-check`. |
| CORS | Só para `RECORVIA_ALLOWED_ORIGINS` (vazio em produção). **Endurecimento, severidade baixa, corrigido:** a variável aceitava qualquer texto, inclusive `null` (iframes em sandbox) e `*`, que receberiam CORS com credenciais e passariam na conferência de origem do CSRF. Agora só origens na forma que o navegador envia (sem porta padrão explícita nem porta vazia), nunca `null` nem `*`, e só `https` em produção; o erro não repete a senha de uma origem com credenciais. Teste: `TestAllowedOriginsValidation`. |
| Corpo e timeouts | Corpo de 1 MiB na API e no nginx, 256 KiB nos webhooks recebidos, 64 KiB de cabeçalhos; servidor HTTP com `ReadHeaderTimeout` 5 s, `ReadTimeout` 15 s, `WriteTimeout` 60 s e `IdleTimeout` 120 s; Caddy com `read_header` 10 s; nginx com os tempos padrão de corpo e envio. Um corpo lento é encerrado pela API em 15 s. |

### 4. Entrada e saída

| Verificação | Resultado |
|---|---|
| SSRF nos webhooks enviados | DNS resolvido e todos os endereços conferidos, conexão no IP validado e nova conferência no `Dialer.Control` (rebinding), sem proxy do ambiente, sem seguir redirecionamentos, https em produção, IPv4 mapeado e NAT64 convertidos. **Endurecimento, severidade baixa, corrigido:** formas de IPv6 com IPv4 embutido (`::a.b.c.d` e `::ffff:0:a.b.c.d`) e faixas de documentação e SRv6 passavam pela lista; agora são recusadas. Teste: `TestPolicyCheck`. |
| Documento HTML da fatura | `html/template` com escape automático, CSP própria sem scripts, exibido na interface em iframe com `srcdoc` e `sandbox` sem `allow-scripts`. |
| CSV | Número e nome do cliente neutralizam fórmulas (`=`, `+`, `-`, `@`, tabulação e retorno); demais colunas são datas, valores e códigos gerados pelo sistema. |
| E-mails | Templates com lista fechada de variáveis, HTML escapado, links só para variáveis de URL com http(s), assunto sem quebras de linha e cabeçalhos MIME conferidos. |
| Uploads | Não há rota de upload. |
| Links externos | O link de pagamento do portal só aceita http(s); a interface não usa `innerHTML` nem `eval`, e a CSP bloqueia `javascript:`. |

### 5. Webhooks recebidos do provedor

| Verificação | Resultado |
|---|---|
| Autenticação | Obrigatória: simulador com HMAC-SHA256 sobre timestamp e corpo, janela de 5 minutos e comparação em tempo constante; Asaas pelo token `asaas-access-token` (32 a 255 caracteres, diferente da chave de API), comparado em tempo constante (ADR-013). |
| Repetição | Evento único por provedor e identificador do evento; reenvio responde `duplicate`. Eventos fora de ordem não regridem a cobrança e saltos consultam o provedor. |
| Tamanho | 256 KiB, JSON válido exigido antes de qualquer consulta. |

### 6. Dependências

`make vulncheck` (govulncheck): nenhuma vulnerabilidade alcançável. `npm audit --omit=dev` e `npm audit`
completo: nenhuma vulnerabilidade. As imagens continuam analisadas na CI (ADR-057); a exceção conhecida é a do
Guava no Keycloak (ADR-086).

### 7. Segredos

`make secrets-scan` (gitleaks, 218 commits): nada encontrado. Segredos ficam no `.env` de produção, fora do Git
e do contexto das imagens; a aplicação redige chaves sensíveis e senhas de URLs nos logs; respostas da API não
trazem segredos além do segredo exibido uma única vez na criação e rotação de credenciais e de endpoints;
contas de provedor mostram só os quatro últimos caracteres da chave.

## Riscos que continuam abertos

| Risco | Severidade | Recomendação |
|---|---|---|
| O webhook do Asaas é autenticado só por token (o provedor não assina o corpo). Quem obtiver o token e o endereço do endpoint de uma organização pode forjar uma confirmação de pagamento, que é aplicada sem consulta quando a transição é a esperada. A conciliação detecta a divergência depois. | média | Consultar a cobrança no Asaas antes de aplicar eventos que mudam a situação financeira (como já é feito nos saltos e nas operações retomadas, ADR-058); avaliar a lista de IPs de origem do Asaas no proxy. |
| A API confia em `X-Real-IP` vindo de qualquer conexão para os limites por IP. Na produção só o nginx alcança a API e ele sempre reescreve o cabeçalho; uma implantação que exponha a API diretamente permitiria trocar de IP a cada requisição. | baixa | Não publicar a porta da API; se for preciso, aceitar `X-Real-IP` só de endereços configurados. |
| Não há limite de tentativas na borda para o domínio do Keycloak; a proteção é a do próprio Keycloak (bloqueio por usuário), que não freia tentativas espalhadas por muitos usuários. | baixa | Limite por IP no proxy (módulo de rate limit do Caddy ou serviço à frente) quando houver volume de usuários; segundo fator para quem administra organizações. |
| A mensagem de erro de envio de e-mail com todos os destinatários recusados inclui os endereços e vai ao log do worker e ao histórico da notificação. | baixa | Registrar no log só a contagem de destinatários recusados; manter o detalhe só no histórico da organização. |
| O link do portal é consumido por um `GET`: um verificador de links do provedor de e-mail que abra o endereço gasta o uso único. Quem receber um link de outra pessoa entra no portal dela (é o desenho do link). | baixa | Página intermediária com confirmação por `POST` antes de consumir o token. |
| `style-src 'unsafe-inline'` na CSP da interface, exigido pelo Mantine. | baixa | Rever quando a biblioteca suportar nonce. |
| A lista de exceções do gitleaks ignora todos os arquivos `_test.go`. | baixa | Restringir a exceção aos valores fictícios conhecidos. |

## Como repetir

```bash
make vulncheck
make secrets-scan
(cd web && npm audit --omit=dev)
go test -race ./internal/identity/... ./internal/infra/config/ ./internal/infra/safehttp/ ./internal/httpapi/
go test -race -tags integration -run 'TestCrossTenant|TestLogin|TestSessionCookieSecure' ./internal/httpapi/
```

`make prod-check` confere a borda de produção (HTTPS, HSTS, endereço do cliente, lista do domínio do
Keycloak e realms).
