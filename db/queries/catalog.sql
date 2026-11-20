-- Produtos, métricas, planos, versões de preço e cupons.

-- name: InsertProduct :exec
INSERT INTO products (id, tenant_id, name, description, status, created_at, updated_at, version)
VALUES (@id, @tenant_id, @name, @description, @status, @created_at, @created_at, 1);

-- name: GetProduct :one
SELECT * FROM products WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetProductForUpdate :one
SELECT * FROM products WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdateProduct :execrows
UPDATE products SET name = @name, description = @description, status = @status, version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListProducts :many
SELECT * FROM products WHERE tenant_id = @tenant_id AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
 ORDER BY lower(name), id LIMIT 500;

-- name: InsertMetric :exec
INSERT INTO billable_metrics (id, tenant_id, code, name, unit, aggregation, max_quantity_per_event, late_grace_hours, status, created_at)
VALUES (@id, @tenant_id, @code, @name, @unit, @aggregation, @max_quantity_per_event, @late_grace_hours, @status, @created_at);

-- name: GetMetric :one
SELECT * FROM billable_metrics WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetMetricByCode :one
SELECT * FROM billable_metrics WHERE tenant_id = @tenant_id AND code = @code;

-- name: ListMetrics :many
SELECT * FROM billable_metrics WHERE tenant_id = @tenant_id ORDER BY code LIMIT 500;

-- name: InsertPlan :exec
INSERT INTO plans (id, tenant_id, product_id, code, name, description, billing_interval, currency, trial_days, status,
                   current_price_version_id, created_at, updated_at, version)
VALUES (@id, @tenant_id, @product_id, @code, @name, @description, @billing_interval, @currency, @trial_days, @status,
        sqlc.narg(current_price_version_id), @created_at, @created_at, 1);

-- name: GetPlan :one
SELECT * FROM plans WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetPlanForUpdate :one
SELECT * FROM plans WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;

-- name: UpdatePlan :execrows
UPDATE plans SET name = @name, description = @description, trial_days = @trial_days, status = @status,
       current_price_version_id = sqlc.narg(current_price_version_id), version = version + 1
 WHERE tenant_id = @tenant_id AND id = @id AND version = @version;

-- name: ListPlans :many
SELECT * FROM plans
 WHERE tenant_id = @tenant_id
   AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(product_id)::uuid IS NULL OR product_id = sqlc.narg(product_id))
 ORDER BY lower(name), id LIMIT 500;

-- name: InsertPriceVersion :exec
INSERT INTO plan_price_versions (id, tenant_id, plan_id, version, pricing_model, flat_amount, unit_amount, metric_id, included_quantity,
                                 currency, created_by, created_at)
VALUES (@id, @tenant_id, @plan_id, @version, @pricing_model, sqlc.narg(flat_amount), sqlc.narg(unit_amount), sqlc.narg(metric_id),
        sqlc.narg(included_quantity), @currency, sqlc.narg(created_by), @created_at);

-- name: GetPriceVersion :one
SELECT * FROM plan_price_versions WHERE tenant_id = @tenant_id AND id = @id;

-- name: ListPriceVersions :many
SELECT * FROM plan_price_versions WHERE tenant_id = @tenant_id AND plan_id = @plan_id ORDER BY version DESC;

-- name: MaxPriceVersion :one
SELECT COALESCE(max(version), 0)::int FROM plan_price_versions WHERE tenant_id = @tenant_id AND plan_id = @plan_id;

-- name: InsertCoupon :exec
INSERT INTO coupons (id, tenant_id, code, name, discount_type, percent_off_bp, amount_off, currency, duration, duration_cycles,
                     max_redemptions, valid_from, valid_until, applies_to_plan_ids, status, created_at, updated_at, version)
VALUES (@id, @tenant_id, @code, @name, @discount_type, sqlc.narg(percent_off_bp), sqlc.narg(amount_off), @currency, @duration,
        sqlc.narg(duration_cycles), sqlc.narg(max_redemptions), sqlc.narg(valid_from), sqlc.narg(valid_until), @applies_to_plan_ids,
        'active', @created_at, @created_at, 1);

-- name: GetCoupon :one
SELECT * FROM coupons WHERE tenant_id = @tenant_id AND id = @id;

-- name: GetCouponByCodeForUpdate :one
SELECT * FROM coupons WHERE tenant_id = @tenant_id AND code = @code FOR UPDATE;

-- name: ListCoupons :many
SELECT * FROM coupons WHERE tenant_id = @tenant_id ORDER BY created_at DESC, id DESC LIMIT 500;

-- name: ArchiveCoupon :execrows
UPDATE coupons SET status = 'archived', version = version + 1 WHERE tenant_id = @tenant_id AND id = @id;

-- name: RedeemCoupon :execrows
-- O limite de resgates é garantido pela condição e pela constraint do banco.
UPDATE coupons SET redemptions_count = redemptions_count + 1
 WHERE tenant_id = @tenant_id AND id = @id AND (max_redemptions IS NULL OR redemptions_count < max_redemptions);

-- name: CountPlansByIDs :one
SELECT count(*)::int FROM plans WHERE tenant_id = @tenant_id AND id = ANY (@ids::uuid[]);

-- name: GetCouponForUpdate :one
SELECT * FROM coupons WHERE tenant_id = @tenant_id AND id = @id FOR UPDATE;
