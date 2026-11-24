# Changelog

Todas as mudanças relevantes são registradas aqui. Formato baseado em
[Keep a Changelog](https://keepachangelog.com/pt-BR/1.1.0/) e
[versionamento semântico](https://semver.org/lang/pt-BR/).

## [Não lançado]

## [0.4.1] - 2026-11-24

Revisão de UI/UX e de acessibilidade da interface, feita a partir de capturas de todas as telas em
computador e celular (ADR-090).

### Adicionado
- Roteiro E2E de acessibilidade (`web/e2e/10-acessibilidade.spec.ts`) com axe-core: 29 telas em
  computador e celular, falhando em violações graves ou críticas das regras WCAG 2.1 AA, e navegação
  por teclado com foco visível.

### Alterado
- Contraste WCAG AA nos textos secundários, badges, botões preenchidos, placeholders e no logo, com as
  cores calculadas no tema; campos com contorno visível no foco.
- No celular, as tabelas viram cartões com rótulo e valor; quando a rolagem horizontal continua, ela é
  acessível pelo teclado.
- Códigos internos traduzidos: ações e recursos da auditoria (com filtros em lista), eventos da
  assinatura, motivos da fatura, lançamentos e contas do ledger, recursos do plano Recorvia, eventos de
  webhook, consumidores, situação no provedor e campos da conciliação.
- Dados do cliente na emissão e preço do item exibidos como lista, sem JSON, com CPF/CNPJ e CEP
  formatados; fuso pelo nome ("horário de Brasília"); versão de preço pela data; "Fim do período de
  teste" e "Data-base do ciclo".
- Seletor de organização com o nome completo no computador e no menu lateral no celular; ações da
  fatura agrupadas no celular; legenda curta no topo do painel.

### Corrigido
- As abas do catálogo apontavam para painéis inexistentes; os nomes acessíveis do menu do usuário e do
  seletor de mês não continham o texto visível.
- O teste da baixa do valor a recuperar falhava entre 21h e meia-noite de Brasília, porque formatava a
  data efetiva em UTC.

## [0.4.0] - 2026-11-23

Preparação para produção: pacote de implantação num servidor, backup contínuo com recuperação para um
ponto no tempo, revisão de segurança e teste de carga.

### Adicionado
- Pacote de produção em `deploy/producao/` (ADR-086):
  - compose de um servidor com Caddy: HTTPS automático, interface e API na mesma origem e Keycloak
    em subdomínio;
  - Keycloak em modo de produção com banco próprio e realm sem usuários (verificação de e-mail,
    política de senha, proteção contra força bruta, também no realm `master`);
  - healthchecks, limites, reinício automático, sistema de arquivos somente leitura e somente as
    portas 80 e 443;
  - `.env.example` com cada variável explicada e `gerar-segredos.sh`;
  - guia `docs/operacao/implantacao.md`.
- `make prod-check` e job `production-stack`: sobem o pacote de produção com `localhost` e conferem
  HTTPS, login até o Keycloak, recusas do proxy, endereço real do cliente por IPv4 e IPv6, portas
  internas fechadas, arquivamento do backup e o backup do Keycloak.
- Backup contínuo do PostgreSQL com WAL-G (ADR-087):
  - arquivamento de WAL e backups base cifrados no cliente num armazenamento compatível com S3;
  - agente `postgres-backup` com backup base agendado, retenção e métricas;
  - imagem `recorvia-postgres` sem root.
- Recuperação para um ponto no tempo num servidor novo (`make pitr-restore TARGET=... [TIMELINE=...]`).
  Segue a linha do tempo pedida, recusa a chave do backup errada ou ausente e confere o resultado.
- Exercício de recuperação para um ponto no tempo com MinIO (`make pitr-check` e job `pitr-check`),
  com RPO e RTO medidos.
- Exportação cifrada do realm do Keycloak (`make keycloak-export`).
- Alertas do arquivamento de WAL, do backup base e do agente de backup, com runbook.
- `docs/operacao/seguranca.md` com o modelo de ameaças, o que foi verificado e os riscos abertos
  (ADR-089).
- Testes de acesso cruzado entre organizações por identificador, corpo, cursor e credencial de API em
  cerca de 40 rotas e nos recebíveis.
- Teste de carga manual (`make load-test`, ADR-088), num projeto isolado do compose, com os limites de
  produção: fechamento, leitura, escrita, webhooks e consumo, sem deixar a máquina suspender.
  Números, limites recomendados e como repetir em `docs/operacao/carga.md`.

### Alterado
- O proxy de produção só repassa ao Keycloak o realm `recorvia` e os recursos estáticos. Console, API
  de administração e realm `master` ficam restritos a `KEYCLOAK_ADMIN_ALLOWED_CIDRS`, fechado por
  padrão.
- O nginx da interface só aceita `X-Forwarded-For` dos proxies listados em
  `RECORVIA_TRUSTED_PROXIES`. A origem pública expõe `/livez`; `/readyz` fica só na rede interna.
- `RECORVIA_ALLOWED_ORIGINS` aceita só origens bem formadas, recusa `null`, `*`, porta padrão
  explícita e porta vazia, e exige https em produção.
- As respostas da API trazem `Content-Security-Policy` restritiva também sem o nginx à frente. O
  nginx deixou de duplicar cabeçalhos de segurança.
- O motor de recorrência fatura até 4 organizações ao mesmo tempo (`RECORVIA_BILLING_CONCURRENCY`,
  limitado à metade do pool de conexões) e retoma na hora uma organização parada no limite de 500
  assinaturas, com 5 s entre as repetições e no máximo 20 seguidas. No teste de carga, a primeira
  fatura do fechamento sai em 6 s, e não em 20 a 88 s.
- O limite de webhooks de provedores por IP passa a ser configurável
  (`RECORVIA_WEBHOOK_RATE_LIMIT_PER_MINUTE`, padrão 1200).

### Corrigido
- **Login CSRF:** cada login fica vinculado ao navegador que o iniciou por um cookie próprio do fluxo
  (`__Host-…_login_<hash>`). Logins em várias abas continuam funcionando. Logins em andamento
  durante a atualização precisam ser refeitos (ADR-089).
- A proteção contra SSRF dos webhooks enviados recusa formas IPv6 com IPv4 embutido (`::/96`,
  `::ffff:0:0:0/96`) e as faixas `3fff::/20` e `5f00::/16`.
- A lista de assinaturas não faz mais uma consulta por item. Com 64 usuários, a mediana caiu de 2,4 a
  3,6 s para 0,3 a 0,5 s.
- Lotes de consumo simultâneos com `external_id` em comum não entram mais em deadlock.
- A consulta de reembolsos e chargebacks do painel caiu de cerca de 200 ms para cerca de 20 ms.
- Sem limite de contratos no plano da plataforma, a criação de assinatura não conta mais as
  assinaturas ativas.

### Segurança
- A imagem do proxy compila o Caddy com `golang.org/x/net` 0.62.0, `golang.org/x/crypto` 0.59.0 e
  OpenTelemetry 1.47.0 e roda como usuário sem privilégios. As imagens do Keycloak de produção e do
  PostgreSQL também rodam sem root.
- O código-fonte do WAL-G é baixado só por HTTPS e conferido pela soma SHA-256.

## [0.3.0] - 2026-11-21

Auditoria de todos os requisitos (RF-01 a RF-17 e RNF-01 a RNF-12) e correção do que ela encontrou.

### Adicionado
- Demonstrativo e documento da fatura com emitente, endereço de cobrança, períodos, condições e o
  aviso de que a fatura comercial não é documento fiscal; exportação CSV com descontos, créditos,
  notas de crédito, baixa e período, em partes quando passa de 50 mil linhas.
- `PATCH /api/v1/customers/{id}/contacts/{contact_id}`; histórico do cliente paginado por cursor.
- Lista, agendamento, conclusão e cancelamento dos serviços adicionais pela equipe da plataforma;
  usuário de demonstração `equipe@recorvia.test` e runbook da equipe da plataforma.
- Histórico de notificações com o motivo de cada situação e linha do tempo
  (`GET /api/v1/notifications/{id}/events`); tentativas das entregas de webhooks
  (`GET /api/v1/webhook-deliveries/{id}/attempts`).
- Painel: indicador "A receber do provedor"; fórmula (`definitions`) e base temporal (`bases`:
  mês, fim do mês ou atual) de cada indicador, e `position_at`; filtros `open_balance=true` nas
  faturas e `in_force=true` nas assinaturas; fórmulas em `docs/arquitetura/indicadores.md`.
- Simulador de pagamentos: ações `confirm` e `receive`; replay em ordem determinística.
- Rota inexistente e método não permitido em `application/problem+json` (`route_not_found`,
  `method_not_allowed` com `Allow`); `HEAD` responde como `GET`, sem corpo.
- Logs com `trace_id` e `span_id`; log de acesso com `tenant_id` e `user_id`; tarefas agendadas
  com `job`; consumidores com o `request_id` da requisição de origem, que chega à auditoria.
- `/readyz` do worker responde 503 durante a drenagem; um segundo sinal força a saída.
- `make pin-check` e passo da CI: falham com action sem SHA, dependência npm sem versão exata,
  imagem sem tag fixa ou com tag flutuante e `go.mod` sem toolchain (ADR-082).
- Testes de integração HTTP validam cada resposta contra o contrato OpenAPI.
- Concorrência otimista nas alterações de recursos versionados que ainda não a exigiam: papel e
  remoção de membro, arquivamento de cliente e de cupom, endereço de cobrança (versão do cliente),
  nova versão de preço (versão do plano), provedor de pagamento, rotação e revogação de credencial,
  resolução de divergência, templates de e-mail, remoção e rotação do segredo de endpoint de
  webhook, troca do plano da plataforma e atendimento dos serviços adicionais pela equipe. Sem
  `If-Match`, 428; com versão divergente, 412 sem nenhum efeito; o sucesso devolve `ETag`. Template
  padrão e provedor não configurado usam `"0"` (ADR-083 e ADR-085).
- Paginação por cursor (`limit`, `cursor`, `next_cursor`; 25 por padrão) em disputas, divergências e
  execuções de conciliação, operações de pagamento, mensagens não processadas, convites, movimentos
  de crédito, credenciais de API, produtos, planos, cupons, métricas, pedidos de serviços adicionais,
  demonstrativos da plataforma e reajustes de assinatura; antes, essas listas cortavam entre 36 e
  500 itens sem avisar. Os campos de seleção da interface carregam todas as páginas (ADR-085).
- Endereço da organização no perfil (`PATCH /api/v1/organization`, campo `address`, auditado como
  `organization.address_updated`), copiado para o emitente na emissão e exibido no documento da
  fatura (ADR-084).
- Aviso de que a fatura comercial não é documento fiscal na página da fatura do portal e no e-mail
  padrão de fatura emitida.
- Cards do painel com link para a lista filtrada correspondente, só com a permissão de leitura da
  lista; filtros "Somente com saldo em aberto" (faturas) e "Somente em vigor" (assinaturas).
- `requested_tenant_id` no log de acesso, inclusive em requisições recusadas antes da validação do
  tenant.
- Roteiros E2E de onboarding, política de cobrança, endpoint de webhook e convite com aceite;
  usuária de demonstração `gabi.convidada@exemplo.test`.
- Migrations `00019` a `00024` (a numeração `00020` não foi usada).

### Alterado
- **Contrato:** as quatro rotas de criação financeira acima respondem 422
  `idempotency_key_required` sem a chave; `subscription_in_progress` e `credit_note_in_progress`
  são conflitos transitórios, com `Retry-After`.
- **Contrato:** a fatura cancelada tem `amount_due` igual a zero; o histórico do cliente devolve 25
  eventos por página, com `next_cursor`; um mês futuro no painel responde 422.
- No logout, sem o ID token, o Keycloak pede confirmação ao usuário.
- `stop_grace_period` do worker passa a 2m30s.
- Detalhes ausentes no histórico e na auditoria passam a ser `{}` em vez de `null`.
- **Contrato:** as alterações listadas acima exigem `If-Match`; as listas acima são paginadas e as
  operações de pagamento passam a vir pela data de criação; `sort` fora da allowlist, ou numa lista
  que não ordena por parâmetro, responde 422 `invalid_sort` em vez de ser ignorado. Membros,
  credenciais, divergências, contas de provedor, templates e a assinatura da plataforma expõem
  `version`. Produtos e planos passam a vir do mais recente para o mais antigo.
- Tornar uma conta de provedor a padrão dá versão nova à conta que perde o padrão.
- O validador de contrato dos testes recusa 5xx não declarado; o 503 do provedor está declarado nas
  operações que o devolvem.

### Corrigido
- **Cobrança em dobro na reentrega:** os consumidores `payments.invoice_issued` e
  `payments.invoice_credited` gravavam o inbox depois do efeito, em transação separada; uma
  mensagem reentregue depois de uma cobrança recusada criava outra cobrança no provedor. A marcação
  do inbox e a cobrança ficam na mesma transação, e a chamada ao provedor acontece depois do commit
  (ADR-076).
- **Idempotency-Key:** `POST /api/v1/subscriptions`, `/platform/addon-orders`,
  `/subscriptions/{id}/price-adjustments` e `/subscriptions/{id}/discounts` aceitavam pedidos sem a
  chave. Só a execução que detém a chave (`lease_token`) registra ou libera o resultado (ADR-075).
- **Assinatura duplicada:** a criação grava a chave de negócio e a impressão do pedido; a repetição
  devolve a mesma assinatura, também com pedidos concorrentes e depois da validade do registro HTTP.
- **Limites do plano da plataforma:** podiam ser ultrapassados descendo de plano e aceitando
  convites pendentes, suspendendo e reativando assinaturas ou com pedidos simultâneos. Convites
  pendentes ocupam vagas e as checagens são serializadas pela assinatura da plataforma (ADR-077).
- **Implantação assistida:** o pedido nunca era atendido nem cobrado. A equipe da plataforma lista,
  agenda, conclui e cancela; o serviço concluído entra uma única vez no demonstrativo.
- **Logout:** o ID token não vai mais ao navegador; o servidor revoga o refresh token (ADR-078).
- **Revogação de sessões:** "Encerrar todas as sessões"; o membro removido perde as sessões
  daquela organização e o usuário desativado perde todas; a revogação usa gerações no Redis com
  gravação condicional, para que uma requisição em andamento não recrie a sessão revogada.
- **Fatura:** o quadro de totais do documento fecha com o saldo; o emitente é gravado na emissão;
  a observação da fatura emitida fica congelada; a fatura cancelada fica sem saldo (ADR-079).
- **Edição de contato:** omitir um campo restaurava o valor padrão e podia reativar o e-mail de
  cobrança desligado pelo cliente.
- **Ledger:** confirmação, recebimento e liquidação do provedor em contas distintas
  (`provider_confirmed`, `provider_clearing`, `cash`); os valores em liquidação passam ao disponível,
  inclusive em reembolsos, chargebacks, disputas e ajustes de conciliação (ADR-080). A conciliação
  detecta o recebimento perdido.
- **Aviso de suspensão** fora da janela de horário da régua (ADR-081).
- **Painel financeiro:** divergências abertas sempre zero; MRR e ARR sem reajuste e sem cupons
  recorrentes; churn contando trial cancelado; indicadores de saldo mostrando a posição de hoje em
  meses passados; fórmulas com chaves diferentes entre a API e a tela (ADR-074).
- **Senhas em mensagens de erro:** URLs do Redis, do RabbitMQ e do PostgreSQL, inclusive com
  caracteres reservados sem codificação na senha, e credenciais SMTP saem mascaradas dos erros e dos
  logs, sem alterar URLs sem credenciais.
- **Encerramento do worker:** a conexão com o RabbitMQ fecha depois das mensagens em andamento,
  então o ack não se perde; entregas não processadas voltam à fila na hora.
- **Simulador de pagamentos:** reembolso e chargeback de cobrança confirmada e ainda não recebida
  passam a ser aceitos, como no provedor real; o pagador é idempotente por comerciante.
- **Política de cobrança:** a mensagem de lembretes repetidos nunca aparecia no formulário.

### Removido
- `identity.Service.RevokeAll`, sem uso.

## [0.2.0] - 2026-11-21

### Adicionado
- Expurgo automático por idade (tarefa `retention.purge` do worker, horária, com lease, por
  tenant e em lotes): inbox, dead letters tratadas, eventos, entregas e segredos vencidos de
  webhooks enviados, corpo dos webhooks recebidos do provedor, conteúdo e histórico de
  notificações, links do portal, convites não aceitos e consumo já faturado. Prazos em
  `RECORVIA_RETENTION_*_DAYS`, com mínimos validados na partida. Registros financeiros e de
  auditoria ficam retidos de propósito (ADR-060).
- Anonimização de cliente (`POST /api/v1/customers/{id}/anonymize`, permissão
  `customers.anonymize`, só por sessão de usuário), irreversível e idempotente, com a ação na
  ficha do cliente; faturas emitidas, pagamentos, ledger e auditoria permanecem (ADR-061).
- Exercício automatizado de recuperação de desastre (`make dr-check` e job `disaster-recovery`):
  backup com as chaves guardadas à parte, destruição, restauração num servidor novo, recusa de
  chave errada ou ausente, conciliação da janela perdida antes de religar o worker e conferência
  de que nada se duplicou, com RPO e RTO observados.
- Conferência das chaves de cifragem contra o banco na partida da API e do worker e no subcomando
  `keyring-check` (`RECORVIA_ENCRYPTION_STARTUP_CHECK`: `enforce`, `warn` ou `off`), e declaração
  de chave perdida (`RECORVIA_ENCRYPTION_LOST_KEYS`) (ADR-063 e ADR-064).
- Subcomando `reconcile-pending` do worker: executa as conciliações registradas e termina, sem
  agendador, consumidores nem publicador da outbox.
- Alertmanager no perfil `observability`: roteamento por severidade, agrupamento, inibição e
  repetição, envio por e-mail e por webhook configurado por variáveis de ambiente, link do
  runbook em cada notificação (ADR-065).
- Métricas de fila sem rótulos de tenant: dead letters abertas por consumidor, fila de
  notificações e de entregas de webhooks, com a idade do item mais antigo; alertas, painéis e
  runbooks correspondentes (ADR-066 e ADR-067).
- Links do portal: listagem e revogação explícita
  (`GET` e `DELETE /api/v1/customers/{id}/portal-links`), que encerra as sessões abertas pelo link;
  aba Portal na ficha do cliente (ADR-068).
- Paginação por cursor das faturas do portal do cliente (ADR-069).
- Chargeback de pagamento com excedente: a disputa perdida consome o crédito do cliente gerado
  pelo excedente; o crédito já usado vira valor a recuperar, na conta `customer_recoverables`
  (ADR-070).
- `GET /api/v1/reconciliation/runs/{id}` para acompanhar uma execução de conciliação, e executor
  de conciliações no worker, com lease e retomada sem duplicar divergências (ADR-071).
- Métricas e alertas das execuções de conciliação à espera do worker e do expurgo por idade.
- Baixa do valor a recuperar das disputas perdidas, total ou parcial, como recebido (`cash`) ou
  perdido (`bad_debt`), com justificativa, data efetiva, auditoria e evento
  `dispute.recovery_settled`; o limite é garantido pelo banco, inclusive com pedidos simultâneos
  (`GET` e `POST /api/v1/disputes/{id}/recovery-settlements`, `GET /api/v1/disputes/{id}`,
  permissão `payments.settle_recovery`). A tela de disputas mostra o saldo a recuperar e o painel
  ganha o indicador "Valores a recuperar de clientes" (ADR-073).
- Migrations `00014` a `00018` (a numeração `00016` não foi usada).

### Alterado
- **Contrato:** `POST /api/v1/reconciliation/runs` passou a ser assíncrono: registra a execução e
  responde `202` com `Location`; antes respondia `200` com a execução encerrada (ADR-072).
  Respostas `202` não ficam registradas na `Idempotency-Key`.
- **Contrato:** `GET /api/portal/v1/invoices` devolve 25 faturas por página por padrão (antes, até
  100 sem paginação); `POST /api/v1/customers/{id}/portal-links` devolve também o `id` do link.
- A API e o worker não iniciam quando as chaves de cifragem não decifram os dados em uso; antes
  iniciavam, e o worker encerrava notificações e entregas de webhook sem volta.
- `customers.anonymize` não pode ser concedida a credenciais de API; cliente anonimizado recusa
  novos contatos, endereço e assinaturas.
- O indicador de chargebacks do painel financeiro deixa de incluir a parte que era pagamento
  excedente.
- O guia de desenvolvimento passa a ser `docs/DESENVOLVIMENTO.md`; arquivos de ambiente local
  ficam fora do repositório.

### Corrigido
- O chargeback de um pagamento com excedente deixava o crédito com o cliente e registrava como
  receita estornada um valor que nunca foi receita.
- Credenciais de provedor que não decifram respondem 503 `provider_credentials_unreadable` em vez
  de 500; a reconfiguração do provedor não depende mais das credenciais anteriores.
- Teste de cifragem das notificações que acusava falso alarme por coincidência nos bytes
  cifrados.

### Removido
- `portal.Service.RevokeLinks` (revogação em massa não exposta por nenhuma rota), substituída pela
  revogação por identificador.

## [0.1.0] - 2026-11-20

### Adicionado
- Núcleo de dinheiro em unidades mínimas com aritmética protegida contra
  overflow, cálculo racional exato, arredondamento half-even e alocação por
  maior resto.
- Calendário de ciclos com âncora preservada, períodos `[início, fim)` e
  prorrata por dias civis no fuso do tenant.
- Configuração validada na inicialização, logs estruturados com redação e
  transações PostgreSQL com contexto de tenant local.
- Bootstrap de papéis do banco (`recorvia_owner`, `recorvia_app`) e migration
  de fundação com RLS.
- CI com formatação, vet, lint, testes com detector de corridas, cobertura,
  fuzz, integração, govulncheck e detecção de segredos.
- Organizações, membros, convites com expiração e uso único, papéis e
  permissões explícitas; plano da plataforma com limites e demonstrativo mensal.
- Login OIDC (Authorization Code + PKCE) com sessão no servidor, cookie
  `HttpOnly`/`Secure`/`SameSite`, CSRF e troca de organização com rotação.
- Clientes PF/PJ, catálogo com versões de preço imutáveis e quatro modelos de
  preço, assinaturas com máquina de estados e motor de recorrência com retomada.
- Faturas com itens, snapshot de preço e numeração única por tenant; ledger de
  partidas dobradas; créditos de clientes.
- Cobrança por provedor com simulador determinístico e adaptador do Asaas
  (não exercitado no sandbox), operações externas com estado `unknown`,
  webhooks autenticados e processamento assíncrono idempotente.
- Outbox com lease, RabbitMQ com confirmação, ack manual, retentativas e DLQ;
  agendador com lease; `Idempotency-Key`; `If-Match`; erros RFC 9457.
- Ambiente local com Docker Compose (PostgreSQL, Redis, RabbitMQ, Keycloak,
  Mailpit, simulador, API, worker e seed de dois tenants).
- Interface web em português (React, TypeScript, TanStack Query, Mantine) com
  tipos gerados do contrato OpenAPI, testes de componente e roteiros E2E com
  Playwright; servida por nginx sem root na mesma origem da API.

- Cupons de desconto, aplicação e remoção na assinatura.
- Mudança de plano e quantidade imediata (prorrata com fatura ou crédito do
  cliente) ou no próximo ciclo; reajustes programados com histórico.
- Ingestão de consumo medido com deduplicação, política de atraso por métrica e
  correção posterior sem alterar a fatura emitida.
- Notas de crédito limitadas ao saldo em aberto, com ajuste da cobrança.
- Reembolsos totais e parciais como operação externa persistida: o valor é
  reservado sob bloqueio da cobrança (nunca acima do pago, mesmo com pedidos
  concorrentes), o provedor é consultado antes de repetir depois de resposta
  ambígua e reembolsos feitos direto no provedor são registrados.
- Disputas e chargebacks com retenção no ledger, estorno na disputa ganha e
  chargeback limitado ao saldo elegível na disputa perdida.
- Cancelamento da cobrança no provedor quando ela é cancelada localmente
  (fatura cancelada, nota de crédito, substituição); uma criação obsoleta
  nunca cria a cobrança no provedor.
- Conciliação manual e agendada com oito tipos de divergência, deduplicação
  entre execuções, resolução com responsável, justificativa, auditoria e
  ajuste no ledger, e rodízio de cobranças fora da janela para detectar
  chargebacks e reembolsos tardios.
- Régua de cobrança por política do tenant: passos relativos ao vencimento,
  janela de horário no fuso do tenant, limite de frequência, parada após a
  resolução da fatura, suspensão por atraso e reativação.
- Templates de notificação com lista fechada de variáveis e escape de HTML;
  notificações por e-mail com identidade de negócio deduplicada, retentativas
  e respeito às preferências de contato.
- Webhooks enviados no formato Standard Webhooks (HMAC-SHA256 com timestamp,
  rotação de segredo com dupla assinatura, retentativas com backoff e jitter,
  reenvio manual) com proteção contra SSRF validada também no momento da
  conexão.
- Portal do cliente por link de uso único (somente o hash do token é
  guardado) trocado por sessão restrita, com faturas, documentos e
  preferências de contato.
- Credenciais de API com escopos limitados às permissões de quem as emite,
  segredo de 256 bits guardado só como hash, expiração, rotação, revogação e
  autenticação `Authorization: Bearer`.
- Todas as 127 operações do contrato OpenAPI implementadas.
- Perfil opcional de observabilidade (OpenTelemetry Collector, Jaeger,
  Prometheus e Grafana) com dashboards provisionados e 34 regras de alerta
  testadas com `promtool test rules`; liveness e readiness distintos na API e
  no worker; métricas operacionais sem rótulos de alta cardinalidade.
- Backup lógico do PostgreSQL com manifesto e restauração verificada num banco
  novo (`make backup`, `make restore-check`), exercitados na CI.
- Análise das imagens com Trivy na CI, falhando em vulnerabilidades HIGH ou
  CRITICAL com correção disponível.
- Runbooks de faturamento, filas e DLQ, conciliação, notificações e webhooks
  e isolamento de dados; política de retenção e anonimização documentada.

### Corrigido
- Ciclo mensal da plataforma sobrepunha períodos e perdia a âncora dos dias 29
  a 31; a avaliação parcial cobrava o mês inteiro.
- Caminho de retorno do login aceitava caracteres de controle (redirecionamento
  para outro domínio após remoção de TAB pelo navegador).
- Recuperação de fatura incobrável não estornava a perda e quitava a fatura
  com valor ainda baixado; taxa do provedor ignorada em pagamento de fatura já
  quitada; repetição de pagamento manual após quitação era recusada.
- Falha de uma assinatura interrompia o faturamento de todo o tenant.
- Confirmação do provedor para cobrança recusada ou cancelada localmente era
  ignorada; o valor recebido passa a ser aplicado.
- Chave de negócio derivada do `Idempotency-Key` passa a ter tamanho fixo
  (chaves longas provocavam erro interno).
- Credenciais de API recebiam erro interno ao criar políticas de cobrança,
  personalizar templates e criar endpoints de webhook (autoria `uuid.Nil`
  violava a FK para `users`); a autoria passa a ser nula, com o ator
  `api_credential` na auditoria. Gestão de membros, convites, plano da
  plataforma e credenciais recusa credenciais com 403
  `user_session_required`, e o tratamento de dead letters grava a auditoria,
  com a justificativa, na mesma transação da resolução (ADR-059).

[Não lançado]: https://github.com/ricartefelipe/recorvia/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/ricartefelipe/recorvia/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/ricartefelipe/recorvia/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/ricartefelipe/recorvia/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ricartefelipe/recorvia/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ricartefelipe/recorvia/releases/tag/v0.1.0
