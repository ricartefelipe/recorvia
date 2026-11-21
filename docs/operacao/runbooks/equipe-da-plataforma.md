# Equipe da plataforma

Inclusão e remoção de integrantes da equipe da plataforma (`platform_staff`). A equipe vê o
catálogo de organizações (`GET /api/v1/admin/tenants`) e atende os pedidos de serviços adicionais
(tela *Atendimento de serviços*, `/admin/servicos`; ADR-077). Não tem acesso aos demais dados de um
tenant sem convite da própria organização.

## Sintomas

- Alguém que deveria atender a implantação assistida recebe 403 em `/api/v1/admin/...` ou vê
  "Acesso negado" em `/admin/servicos`.
- Uma pessoa que saiu da equipe ainda vê o atendimento.

Sem alerta automático: o pedido de inclusão ou remoção chega pelo responsável da operação e fica
registrado (quem pediu, quem executou, horário).

## Diagnóstico

O papel da aplicação só lê `platform_staff`; a manutenção é feita pelo dono das tabelas (ou pelo
superusuário no ambiente local). O usuário precisa ter entrado ao menos uma vez no Recorvia (linha
em `users`), exceto o usuário de demonstração do ambiente local, criado pelo `migrate up`.

```bash
docker compose exec postgres psql -U postgres -d recorvia
```

```sql
-- Quem é da equipe hoje.
SELECT u.email, u.display_name, s.role, s.created_at
  FROM platform_staff s JOIN users u ON u.id = s.user_id
 ORDER BY s.created_at;

-- O usuário a incluir (pelo e-mail usado no provedor de identidade).
SELECT id, email, status, last_login_at FROM users WHERE lower(email) = lower('pessoa@recorvia.test');
```

## Ações

Inclusão (papel `support` ou `operations`):

```sql escrita
INSERT INTO platform_staff (user_id, role)
SELECT id, 'operations' FROM users WHERE lower(email) = lower('pessoa@recorvia.test')
ON CONFLICT (user_id) DO NOTHING;
```

Remoção:

```sql escrita
DELETE FROM platform_staff
 WHERE user_id = (SELECT id FROM users WHERE lower(email) = lower('pessoa@recorvia.test'));
```

A situação de equipe é lida a cada requisição: a inclusão e a remoção valem na próxima requisição,
sem encerrar a sessão. Para cortar o acesso de imediato, a pessoa (ou a operação, em nome dela)
usa *Encerrar todas as sessões* no menu do usuário.

No ambiente local, o `migrate up` inclui o usuário de demonstração `equipe@recorvia.test`
(variável `RECORVIA_DEMO_STAFF_SUBJECT` do serviço `migrate` no `compose.yaml`). Em produção essa
variável é recusada; a equipe é mantida somente por este procedimento.

## Verificação

- A consulta de diagnóstico lista a pessoa (ou não a lista, depois da remoção).
- Com a sessão dela, `GET /api/v1/session` traz `platform_staff: true` (ou `false`) e a tela
  `/admin/servicos` abre (ou mostra "Acesso negado").
