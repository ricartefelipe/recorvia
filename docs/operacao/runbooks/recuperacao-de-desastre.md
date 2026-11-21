# Runbook: recuperação de desastre e chaves de cifragem

Cobre a perda do banco de dados (restauração do backup lógico num servidor novo, ADR-055), a
conferência das chaves de cifragem contra o banco (ADR-063) e a retomada da aplicação com a
janela perdida conciliada. O procedimento completo, com as justificativas, está em
[Backup e restauração](../backup-e-restauracao.md); este runbook é a sequência de comandos e de
verificações. A sequência é exercitada de ponta a ponta por `make dr-check` (job
`disaster-recovery` da CI, ADR-064).

Regras que valem em todos os passos:
- **o worker só é religado depois da conciliação**: ele publica a outbox, emite as faturas vencidas
  e cria cobranças no provedor;
- **a aplicação só sobe com as chaves certas**: com chave errada ou ausente, a API e o worker se
  recusam a iniciar; não contorne a recusa sem ler [Chave perdida](#chave-perdida);
- **a conciliação é feita com a credencial de cada organização**: a equipe da plataforma não tem
  acesso aos dados financeiros dos tenants (`docs/arquitetura/permissoes.md`); veja o passo 5;
- dinheiro recebido nunca é descartado (ADR-030) e nenhuma cobrança é recriada às cegas.

<a id="banco-perdido"></a>

## Banco de dados perdido

### Sintomas
- PostgreSQL indisponível sem recuperação (volume perdido ou corrompido); `GET /readyz` da API e
  do worker com `postgres: unavailable`.

### Métricas e alertas
- `RecorviaDependencyUnavailable` (`recorvia_dependency_up{dependency="postgres"} == 0`) e
  `RecorviaTargetDown`.
- Sem alerta específico de perda de dados: a decisão de restaurar é de quem conduz o incidente.

### Diagnóstico
- Confirme que o servidor não volta com os dados (volume, armazenamento, réplica). Restaurar um
  backup descarta tudo o que aconteceu depois dele.
- Separe o backup mais recente que passou pela verificação (`make restore-check`) e as chaves de
  cifragem do cofre (todas as versões em uso no instante do backup).
- Registre o instante do backup: ele delimita a [janela perdida](../backup-e-restauracao.md#janela-perdida).

### Ações
1. Pare o worker e tire a API do tráfego.
2. Servidor novo, bootstrap e restauração (passos 1 a 6 do guia):
   ```bash
   docker compose up -d --wait postgres
   docker compose run --rm --no-deps migrate-bootstrap
   (cd backups && sha256sum -c recorvia-<instante>.sha256)
   docker exec -i "$(docker compose ps -q postgres)" pg_restore -U postgres -d recorvia \
     --exit-on-error --single-transaction < backups/recorvia-<instante>.dump
   docker compose run --rm --no-deps migrate
   make restore-check DUMP=backups/recorvia-<instante>.dump
   ```
3. Confira as chaves de cifragem contra o banco restaurado, antes de subir qualquer processo
   ([seção seguinte](#chaves-de-cifragem)).
4. Suba somente a API, fora do tráfego de usuários e de provedores, e confira `GET /readyz`.
5. Concilie a janela perdida pela API, **uma organização por vez, com uma credencial da própria
   organização**. As rotas são de tenant (`reconciliation.run`, `reconciliation.read` e
   `reconciliation.resolve`): não há rota de plataforma nem acesso da equipe da plataforma aos
   dados de um tenant. Quem executa, por ordem de preferência:
   - **credencial de API da organização** (`rcv_...`) com as três permissões, emitida antes do
     incidente por um proprietário ou administrador da organização e guardada conforme o acordo de
     suporte com o tenant. Credenciais emitidas antes do backup continuam válidas no banco
     restaurado (só o hash fica no banco); as emitidas na janela perdida não existem mais;
   - **um membro da organização** com papel de proprietário, administrador ou financeiro (inclusive
     alguém da equipe de suporte convidado pelo tenant antes do incidente), com sessão de usuário:
     o login depende do provedor de identidade e a pessoa precisa alcançar o endereço restrito da
     API;
   - **sem credencial da organização**: não é possível conciliar pela API. Faça o inventário por
     SQL e a conferência no provedor conforme a
     [conciliação manual do guia](../backup-e-restauracao.md#inventário-local), com o tenant, e
     registre no incidente as organizações que ficaram sem conciliar antes de religar o worker.

   A API continua fora do tráfego geral: exponha-a num endereço restrito (rede de operação, VPN
   ou lista de endereços permitidos) a quem vai conciliar. Liberar a API a todos os usuários da
   organização para que eles mesmos conciliem também libera as demais operações (inclusive as que
   chamam o provedor); só faça isso por decisão registrada no incidente.
   ```bash
   API=https://api-restrita.example            # endereço restrito da API, fora do balanceador público
   TOKEN=rcv_credencial_da_organizacao         # credencial de API da organização a conciliar
   # Pedido da conciliação: registra a execução e responde 202 (o worker continua parado).
   curl -X POST -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: $(uuidgen)" \
     "$API/api/v1/reconciliation/runs"
   ```
   Depois de registrar os pedidos de todas as organizações, execute-os uma vez, sem religar o
   worker, com o subcomando `reconcile-pending` e a mesma configuração do worker (o agendador, os
   consumidores e o publicador da outbox não iniciam):
   ```bash
   docker compose run --rm --no-deps worker reconcile-pending   # ou: /app reconcile-pending no container do worker
   curl -H "Authorization: Bearer $TOKEN" "$API/api/v1/reconciliation/discrepancies?status=open"
   ```
   O subcomando termina com erro se alguma conciliação falhar ou ficar sem terminar: consulte a causa
   em `GET /api/v1/reconciliation/runs/{id}` e repita antes de seguir.
   - `payment_not_reflected` (pago no provedor na janela): resolver com `accept_provider`;
   - `missing_local` (cobrança criada na janela, sem registro local): se não foi paga, cancelar no
     provedor e resolver com `dismiss`; se foi paga, não cancelar e seguir a tabela do guia;
   - demais tipos: [runbook de conciliação](conciliacao.md#divergencias-abertas).

   A conciliação exige o provedor padrão configurado na organização; repita, com a credencial de
   cada uma, para todas as organizações com cobranças no período. Para saber quais são
   (superusuário, somente leitura):
   ```sql
   -- Organizações com cobranças vinculadas ao provedor nos 30 dias da janela padrão da conciliação.
   SELECT t.slug, t.id AS tenant_id, count(*) AS cobrancas FROM charges c JOIN tenants t ON t.id = c.tenant_id
    WHERE c.provider_charge_id IS NOT NULL AND c.created_at > now() - interval '30 days' GROUP BY 1, 2 ORDER BY 1;
   ```
   Ainda não existe um meio de plataforma para conciliar todas as organizações de uma vez antes
   de religar o worker (limitação registrada na ADR-064, em `docs/DECISOES.md`, e em
   [Não exercitado](../backup-e-restauracao.md#não-exercitado), no guia).
6. Com as divergências resolvidas, ou com as organizações não conciliadas registradas no incidente
   e avisadas, religue o worker e devolva o tráfego à API.

### Verificação
```sql
-- Eventos ainda não publicados (zeram alguns segundos depois de religar o worker; superusuário).
SELECT event_type, count(*) FROM outbox_events WHERE published_at IS NULL GROUP BY 1 ORDER BY 1;
-- Mais de uma cobrança em aberto para a mesma fatura (duplicidade a tratar com o provedor).
SELECT tenant_id, invoice_id, count(*) FROM charges
 WHERE status IN ('pending', 'awaiting_payment') GROUP BY 1, 2 HAVING count(*) > 1;
-- Lançamentos desbalanceados no ledger. Esperado: nenhuma linha.
SELECT tenant_id, transaction_id, currency FROM ledger_entries GROUP BY 1, 2, 3
HAVING sum(amount) FILTER (WHERE direction = 'debit') IS DISTINCT FROM sum(amount) FILTER (WHERE direction = 'credit');
```
- `GET /readyz` da API (PostgreSQL e Redis) e do worker (PostgreSQL e RabbitMQ) respondem 200.
- `RecorviaPaymentOperationsUnknown` e `RecorviaPaymentOperationsExhausted` não disparam; não há
  dead letters novas (`GET /api/v1/dead-letters`).
- No provedor, cada fatura em aberto tem uma única cobrança ativa.

<a id="chaves-de-cifragem"></a>

## Chaves de cifragem: conferência e recusa na inicialização

### Sintomas
- A API ou o worker terminam logo ao iniciar com `keyring incompatível com os dados cifrados` e
  `A aplicação não foi iniciada` no log.
- `keyring-check` termina com erro.
- Com a conferência relaxada (`RECORVIA_ENCRYPTION_STARTUP_CHECK=warn` ou `off`), com uma chave
  declarada perdida (`RECORVIA_ENCRYPTION_LOST_KEYS`) ou com um registro corrompido: respostas 503
  `provider_credentials_unreadable`, notificações `failed` com `content_unavailable` e entregas de
  webhook `exhausted` com o erro "nenhum segredo de assinatura válido pôde ser decifrado".

### Métricas e alertas
- `RecorviaTargetDown` (o processo não fica no ar) e, com a conferência relaxada,
  `RecorviaNotificationsFailed` e `RecorviaWebhookDeliveriesFailing`.

### Diagnóstico
```bash
# Confere as chaves da configuração atual do serviço contra o banco, num container novo, sem
# iniciar nada e sem alterar dados (uso: antes de subir a aplicação).
docker compose run --rm --no-deps worker keyring-check
# Chaves de um processo que já está em execução (rotação, conferência relaxada): a mesma
# conferência dentro do próprio container, que usa o ambiente do processo.
docker compose exec -T worker /app keyring-check
docker compose logs --since 15m api worker | grep -E 'keyring|A aplicação não foi iniciada'
```
```sql
-- Identificadores de chave referenciados pelos dados cifrados (superusuário; sem conteúdo).
SELECT 'credencial de provedor' AS finalidade, api_key_key_id AS chave, count(*) AS registros
  FROM payment_provider_accounts WHERE api_key_key_id IS NOT NULL GROUP BY 2
UNION ALL
SELECT 'segredo de endpoint de webhook', secret_key_id, count(*) FROM webhook_endpoint_secrets GROUP BY 2
UNION ALL
SELECT 'conteúdo de notificação', content_key_id, count(*) FROM notifications WHERE content_key_id IS NOT NULL GROUP BY 2
ORDER BY 1, 2;
```

| Resultado do `keyring-check` | Causa | Bloqueia a inicialização? | Ação |
|---|---|---|---|
| `chave ausente` | o identificador gravado nos dados não está em `RECORVIA_ENCRYPTION_KEYS` | sim | acrescentar a chave do cofre com o mesmo identificador (a lista precisa ter todas as versões em uso, não só a ativa) |
| `chave com outro valor` | o identificador existe, mas **nenhum** registro cifrado com ele decifra, em nenhuma finalidade | sim | corrigir o valor: a chave do cofre é de outro ambiente ou foi trocada. Se a chave só tem um registro, o mesmo resultado aparece para esse registro corrompido: confira o valor no cofre antes de concluir |
| `registros ilegíveis` | a chave decifra outros registros (de qualquer finalidade), mas estes não: dado adulterado, corrompido ou copiado de outra organização ou registro | não (fica no log em nível de erro e no relatório, com `atenção:`) | investigar como incidente de integridade; não é problema do keyring. O registro falha item a item até ser refeito (conta configurada de novo, segredo rotacionado) |
| `chave declarada perdida` | o identificador não está no keyring e está em `RECORVIA_ENCRYPTION_LOST_KEYS` | não | refazer as integrações afetadas ([Chave perdida](#chave-perdida)) |
| qualquer um seguido de `(fora de uso)` | o que não decifra não é mais lido: segredo de webhook expirado, endpoint removido ou conta de provedor desativada | não | nenhuma ação obrigatória |

O diagnóstico é por chave: basta um registro decifrado, de qualquer finalidade, para o valor da
chave estar comprovado. O `keyring-check` termina com erro somente nas linhas que bloqueiam.

### Ações
1. Corrigir `RECORVIA_ENCRYPTION_KEYS` e `RECORVIA_ENCRYPTION_ACTIVE_KEY` com as chaves do cofre e
   repetir o `keyring-check` até `keyring compatível com o banco`.
2. Só então iniciar a API e o worker.
3. Se a chave não existe mais em lugar nenhum: [Chave perdida](#chave-perdida).

### Verificação
`keyring-check` termina com `keyring compatível com o banco`; a API e o worker registram
`keyring conferido com o banco` ao iniciar.

<a id="chave-perdida"></a>

## Chave perdida

Sem a chave, os dados cifrados com ela não são recuperáveis. O que pode ser feito é declarar a
perda, religar a aplicação e refazer as integrações afetadas. A declaração é por chave
(`RECORVIA_ENCRYPTION_LOST_KEYS`): a conferência continua valendo, no modo `enforce`, para todas
as outras chaves.

### Sintomas
- `keyring-check` com `chave ausente` (ou `chave com outro valor`) e a chave não está no cofre nem
  em nenhuma cópia.

### Métricas e alertas
- Os mesmos da [seção anterior](#chaves-de-cifragem); depois de religar com a chave declarada
  perdida, `RecorviaNotificationsFailed` e `RecorviaWebhookDeliveriesFailing` são esperados até as
  integrações serem refeitas.

### Diagnóstico
- A saída do `keyring-check` lista o que depende da chave perdida: finalidade, quantidade de
  registros e quantos estão em uso.
- Confirme a perda com quem administra o cofre antes de seguir: os passos abaixo encerram
  notificações e entregas pendentes que a chave ainda poderia salvar. Procure a chave também no
  ambiente de um processo que ainda esteja em execução e na configuração do deploy anterior: se
  ela estiver lá, guarde-a no cofre e volte à [seção anterior](#chaves-de-cifragem).
- Verifique se a chave perdida é a ativa (`RECORVIA_ENCRYPTION_ACTIVE_KEY`): nesse caso é preciso
  uma chave nova, e ela entra no mesmo passo em que a perda é declarada (passo 3).

### Ações
1. Registre a saída do `keyring-check` no incidente (não contém segredos).
2. Pare a API e o worker, **todas as réplicas**, se algum ainda estiver em execução (no modo
   `enforce` eles já não iniciam sem a chave; no modo `warn` ou `off` podem estar no ar). Com
   todos parados, a configuração abaixo é trocada **num único passo**.
3. Grave a configuração nova no ambiente da API e do worker, de uma vez e sem iniciar nada:
   - `RECORVIA_ENCRYPTION_KEYS`: as chaves que continuam no cofre, sem a perdida (retire-a se
     ainda estiver na lista com um valor que não serve). Se a chave perdida era a ativa, ou a
     única, acrescente uma chave nova (`openssl rand -base64 32`), com um **identificador novo**
     e guardada no cofre **antes** de entrar na configuração. Não reutilize o identificador da
     chave perdida: os dados antigos apareceriam como `chave com outro valor`, que bloqueia, e
     uma chave configurada não pode ser declarada perdida;
   - `RECORVIA_ENCRYPTION_ACTIVE_KEY`: uma chave da lista; a nova, se a perdida era a ativa;
   - `RECORVIA_ENCRYPTION_LOST_KEYS`: o identificador da chave perdida (só o identificador);
   - `RECORVIA_ENCRYPTION_STARTUP_CHECK=enforce` (o padrão).

   Exemplo com as chaves `dr1` e `dr2`, a `dr2` ativa e perdida: `RECORVIA_ENCRYPTION_KEYS` passa
   a ter `dr1` e a nova `dr3`, `RECORVIA_ENCRYPTION_ACTIVE_KEY=dr3` e
   `RECORVIA_ENCRYPTION_LOST_KEYS=dr2`.

   A [rotação em duas fases](../backup-e-restauracao.md#rotação) **não se aplica** quando a chave
   perdida é a ativa: a fase 1 mantém a chave ativa, que é justamente a que não existe mais, e a
   API, o worker e o `keyring-check` recusam a configuração com
   `RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada`. As duas fases existem
   para processos que convivem com keyrings diferentes; com todos parados (passo 2), nenhum
   processo fica para trás e a troca num único passo é segura. Se a chave ativa não muda (a
   perdida era uma chave anterior), a mudança é só retirar a chave e declarar a perda.
4. Confira antes de iniciar, com a configuração do passo 3 já gravada: o `keyring-check` precisa
   terminar com `keyring compatível com o banco`, com a primeira linha mostrando as chaves
   restantes, a ativa e `declaradas perdidas: <identificador>`, as linhas `chave declarada
   perdida` e um aviso `atenção:` para cada finalidade em uso que depende dela. Qualquer outra
   chave ausente ou errada continua recusada.
   ```bash
   # Container novo, com a configuração atual do compose: é o que os serviços vão usar ao iniciar.
   docker compose run --rm --no-deps worker keyring-check
   ```
   Se o comando terminar com erro de configuração (chave ativa fora da lista, chave configurada
   declarada como perdida), corrija o passo 3; nada foi iniciado nem alterado.
5. Inicie a API e o worker. Consequências, por finalidade, até cada integração ser refeita:
   - **contas de provedor**: criação de cobranças, reembolsos, conciliação e webhooks do provedor
     respondem 503 `provider_credentials_unreadable` até a conta ser configurada de novo
     (`PUT /api/v1/payment-providers/{provider}`, `integrations.write`, com a chave de API, um
     novo segredo de webhook e a `version` da conta no `If-Match`; a operação não depende das
     credenciais anteriores);
   - **endpoints de webhook**: as entregas se encerram como `exhausted` ("nenhum segredo de assinatura
     válido pôde ser decifrado") até o tenant gerar um segredo novo
     (`POST /api/v1/webhook-endpoints/{id}/rotate-secret`, com a `version` do endpoint no `If-Match` e
     `previous_secret_valid_hours` igual a 0: o
     segredo anterior não assina mais nada) e reenviar as entregas
     (`POST /api/v1/webhook-deliveries/{id}/retry`);
   - **notificações pendentes**: encerram-se como `failed` (`content_unavailable`) na tentativa
     seguinte; as de fatura e de lembrete não são recriadas; convites podem ser refeitos.
6. Acompanhe com o `keyring-check`: as linhas da chave perdida passam a `(fora de uso)` à medida que
   as contas são configuradas de novo e os segredos, rotacionados. Uma organização que não refaça
   as integrações continua com as próprias operações falhando, sem afetar as demais nem impedir a
   inicialização. A declaração pode ser retirada quando nenhuma linha em uso citar a chave.

Não use `RECORVIA_ENCRYPTION_STARTUP_CHECK=warn` como estado permanente: ele inicia a aplicação
mesmo com **qualquer** chave errada ou ausente (e mesmo sem conseguir concluir a conferência), o
que devolve o risco de um engano de keyring encerrar notificações e entregas pendentes. O modo
`warn` serve a uma emergência em que a conferência em si impede a inicialização.

### Verificação
`keyring-check` termina com `keyring compatível com o banco`; a API e o worker, em `enforce`,
registram `keyring conferido com o banco` ao iniciar e, enquanto houver dado em uso que dependa da
chave perdida, um aviso por finalidade. Os alertas de notificações e de entregas de webhook
normalizam depois que as integrações são refeitas.
