-- Organizações, membros, convites e plano da plataforma.

-- name: InsertTenant :exec
INSERT INTO tenants (id, slug, name, legal_name, document_number, timezone, currency, invoice_prefix, payment_terms_days, status, created_at, updated_at, version)
VALUES (@id, @slug, @name, sqlc.narg(legal_name), sqlc.narg(document_number), @timezone, @currency, @invoice_prefix, @payment_terms_days, @status, @created_at, @created_at, 1);

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = app_current_tenant();

-- name: UpdateTenant :execrows
UPDATE tenants
   SET name = @name, legal_name = sqlc.narg(legal_name), document_number = sqlc.narg(document_number), timezone = @timezone,
       invoice_prefix = @invoice_prefix, payment_terms_days = @payment_terms_days, version = version + 1
 WHERE id = @id AND version = @version;

-- name: GetTenantAddress :one
SELECT * FROM tenant_addresses WHERE tenant_id = @tenant_id;

-- name: UpsertTenantAddress :exec
INSERT INTO tenant_addresses (tenant_id, street, number, complement, district, city, state, postal_code, country, updated_at)
VALUES (@tenant_id, @street, @number, @complement, @district, @city, @state, @postal_code, @country, @updated_at)
ON CONFLICT (tenant_id) DO UPDATE
   SET street = EXCLUDED.street, number = EXCLUDED.number, complement = EXCLUDED.complement, district = EXCLUDED.district,
       city = EXCLUDED.city, state = EXCLUDED.state, postal_code = EXCLUDED.postal_code, country = EXCLUDED.country,
       updated_at = EXCLUDED.updated_at;

-- name: TenantSlugAvailable :one
SELECT app_tenant_slug_available(@slug)::boolean AS available;

-- name: UserMemberships :many
SELECT tenant_id::uuid AS tenant_id, tenant_name::text AS tenant_name, tenant_slug::text AS tenant_slug,
       role::text AS role, tenant_status::text AS tenant_status
  FROM app_user_memberships(@user_id);

-- name: InsertMembership :exec
INSERT INTO memberships (tenant_id, user_id, role, status, invited_by, created_at, updated_at, version)
VALUES (@tenant_id, @user_id, @role, @status, sqlc.narg(invited_by), @created_at, @created_at, 1);

-- name: GetMembership :one
SELECT * FROM memberships WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: GetMembershipForUpdate :one
SELECT * FROM memberships WHERE tenant_id = @tenant_id AND user_id = @user_id FOR UPDATE;

-- name: UpdateMembership :execrows
UPDATE memberships SET role = @role, status = @status, version = version + 1
 WHERE tenant_id = @tenant_id AND user_id = @user_id AND version = @version;

-- name: ListMembers :many
SELECT m.tenant_id, m.user_id, m.role, m.status, m.invited_by, m.created_at, m.version, u.email, u.display_name
  FROM memberships m JOIN users u ON u.id = m.user_id
 WHERE m.tenant_id = @tenant_id AND m.status = 'active'
 ORDER BY lower(u.display_name), m.user_id;

-- name: LockActiveOwners :many
-- Bloqueia as associações de proprietários para serializar rebaixamentos e
-- remoções concorrentes (a organização mantém ao menos um proprietário).
SELECT user_id FROM memberships
 WHERE tenant_id = @tenant_id AND role = 'owner' AND status = 'active'
 ORDER BY user_id FOR UPDATE;

-- name: CountActiveMembers :one
SELECT count(*)::int FROM memberships WHERE tenant_id = @tenant_id AND status = 'active';

-- name: InsertInvitation :exec
INSERT INTO invitations (id, tenant_id, email, role, token_hash, invited_by, expires_at, created_at)
VALUES (@id, @tenant_id, @email, @role, @token_hash, @invited_by, @expires_at, @created_at);

-- name: GetInvitation :one
SELECT * FROM invitations WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetInvitationForUpdate :one
SELECT * FROM invitations WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdateInvitation :exec
UPDATE invitations SET accepted_at = sqlc.narg(accepted_at), accepted_by = sqlc.narg(accepted_by),
       revoked_at = sqlc.narg(revoked_at), revoked_by = sqlc.narg(revoked_by)
 WHERE tenant_id = @tenant_id AND id = @id;

-- name: ListInvitations :many
SELECT * FROM invitations
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: CountPendingInvitations :one
SELECT count(*)::int FROM invitations
 WHERE tenant_id = @tenant_id AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > @now;

-- name: FindInvitation :one
SELECT invitation_id::uuid AS invitation_id, tenant_id::uuid AS tenant_id FROM app_find_invitation(@token_hash);

-- name: GetDefaultPlatformPlan :one
SELECT * FROM platform_plans WHERE is_default AND active;

-- name: GetPlatformPlan :one
SELECT * FROM platform_plans WHERE code = @code;

-- name: ListPlatformPlans :many
SELECT * FROM platform_plans WHERE active ORDER BY sort_order, code;

-- name: ListPlatformAddons :many
SELECT * FROM platform_addons WHERE active ORDER BY code;

-- name: GetPlatformAddon :one
SELECT * FROM platform_addons WHERE code = @code;

-- name: InsertTenantPlatformSubscription :exec
INSERT INTO tenant_platform_subscriptions (tenant_id, plan_code, status, trial_ends_at, anchor_date, current_period_start, current_period_end, created_at, updated_at, version)
VALUES (@tenant_id, @plan_code, @status, sqlc.narg(trial_ends_at), @anchor_date, @current_period_start, @current_period_end, @created_at, @created_at, 1);

-- name: GetTenantPlatformSubscription :one
SELECT * FROM tenant_platform_subscriptions WHERE tenant_id = @tenant_id;

-- name: GetTenantPlatformSubscriptionForUpdate :one
SELECT * FROM tenant_platform_subscriptions WHERE tenant_id = @tenant_id FOR UPDATE;

-- name: UpdateTenantPlatformSubscription :execrows
UPDATE tenant_platform_subscriptions
   SET plan_code = @plan_code, status = @status, trial_ends_at = sqlc.narg(trial_ends_at),
       current_period_start = @current_period_start, current_period_end = @current_period_end, version = version + 1
 WHERE tenant_id = @tenant_id AND version = @version;

-- name: CountActiveSubscriptions :one
SELECT count(*)::int FROM subscriptions WHERE tenant_id = @tenant_id AND status IN ('trialing', 'active', 'past_due');

-- name: InsertAddonOrder :exec
INSERT INTO platform_addon_orders (id, tenant_id, addon_code, status, notes, price_amount, requested_by, created_at, updated_at)
VALUES (@id, @tenant_id, @addon_code, @status, @notes, @price_amount, @requested_by, @created_at, @created_at);

-- name: ListAddonOrders :many
-- Paginação por keyset em (created_at, id), que não mudam depois do pedido.
SELECT * FROM platform_addon_orders
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @row_limit;

-- name: GetAddonOrderForUpdate :one
SELECT * FROM platform_addon_orders WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdateAddonOrder :execrows
UPDATE platform_addon_orders
   SET status = @status, scheduled_for = sqlc.narg(scheduled_for), completed_at = sqlc.narg(completed_at),
       canceled_at = sqlc.narg(canceled_at), cancel_reason = sqlc.narg(cancel_reason), handled_by = sqlc.narg(handled_by),
       updated_at = @updated_at, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: SumUnbilledCompletedAddons :one
-- Pedidos concluídos antes do fim do período que ainda não entraram em
-- demonstrativo (ADR-077).
SELECT COALESCE(sum(price_amount), 0)::bigint FROM platform_addon_orders
 WHERE tenant_id = @tenant_id AND status = 'completed' AND statement_id IS NULL AND completed_at < @before;

-- name: MarkAddonsBilled :exec
UPDATE platform_addon_orders SET statement_id = @statement_id
 WHERE tenant_id = @tenant_id AND status = 'completed' AND statement_id IS NULL AND completed_at < @before;

-- name: StaffAddonOrders :many
SELECT * FROM app_platform_addon_orders(sqlc.narg(status)::text, sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid,
  @row_limit::int);

-- name: PlatformTenantNames :many
SELECT tenant_id::uuid AS tenant_id, name::text AS name, slug::text AS slug FROM app_platform_tenant_names(@ids::uuid[]);

-- name: ListAllPlatformAddons :many
SELECT * FROM platform_addons ORDER BY code;

-- name: InsertPlatformStatement :execrows
INSERT INTO platform_statements (id, tenant_id, period_start, period_end, plan_code, subscription_amount, addons_amount, total_amount, currency, status, created_at)
VALUES (@id, @tenant_id, @period_start, @period_end, @plan_code, @subscription_amount, @addons_amount, @total_amount, @currency, @status, @created_at)
ON CONFLICT (tenant_id, period_start) DO NOTHING;

-- name: ListPlatformStatements :many
-- Paginação por keyset em (period_start, id), que não mudam depois do
-- fechamento.
SELECT * FROM platform_statements
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(cursor_ts)::timestamptz IS NULL OR (period_start, id) < (sqlc.narg(cursor_ts)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY period_start DESC, id DESC
 LIMIT @row_limit;

-- name: TenantsDuePlatformStatement :many
SELECT t::uuid AS tenant_id FROM app_tenants_due_platform_statement(@now) AS t;

-- name: PlatformTenantCatalog :many
SELECT tenant_id::uuid AS tenant_id, name::text AS name, slug::text AS slug, status::text AS status,
       COALESCE(plan_code, '')::text AS plan_code, COALESCE(platform_status, '')::text AS platform_status, member_count::bigint AS member_count,
       created_at::timestamptz AS created_at
  FROM app_platform_tenant_catalog();

-- name: InitInvoiceSequence :exec
INSERT INTO invoice_number_sequences (tenant_id, next_number) VALUES (@tenant_id, 1) ON CONFLICT (tenant_id) DO NOTHING;

-- name: EnsureLedgerAccount :exec
INSERT INTO ledger_accounts (id, tenant_id, code, name, kind, currency)
VALUES (@id, @tenant_id, @code, @name, @kind, @currency)
ON CONFLICT (tenant_id, code, currency) DO NOTHING;
