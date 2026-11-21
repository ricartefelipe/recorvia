-- Plataforma: atendimento dos pedidos de serviço adicional (implantação
-- assistida) pela equipe da plataforma e cobrança no demonstrativo mensal
-- (ADR-077). O tenant registra o pedido; a equipe agenda, conclui ou
-- cancela. O pedido concluído entra no demonstrativo do período em que a
-- conclusão ocorreu (fim do período posterior à conclusão) ou, se esse já
-- tiver sido fechado quando a conclusão foi confirmada, no seguinte; fica
-- vinculado ao demonstrativo que o cobrou, o que garante cobrança única.

-- +goose Up

ALTER TABLE platform_addon_orders
  ADD COLUMN scheduled_for timestamptz,
  ADD COLUMN completed_at  timestamptz,
  ADD COLUMN canceled_at   timestamptz,
  ADD COLUMN cancel_reason text,
  ADD COLUMN handled_by    uuid REFERENCES users (id),
  ADD COLUMN statement_id  uuid,
  ADD COLUMN version       integer NOT NULL DEFAULT 1;

-- Pedidos alterados antes desta migration só podiam mudar por SQL direto: a
-- última alteração vira o instante da transição.
UPDATE platform_addon_orders SET scheduled_for = updated_at WHERE status = 'scheduled';
UPDATE platform_addon_orders SET completed_at = updated_at WHERE status = 'completed';
-- Pedidos concluídos já cobrados pelo fechamento anterior a esta migration
-- (que somava os concluídos com updated_at dentro do período do
-- demonstrativo) ficam vinculados ao demonstrativo que os cobrou: o
-- fechamento atual soma os concluídos sem demonstrativo e, sem o vínculo,
-- cobraria esses pedidos de novo.
UPDATE platform_addon_orders o SET statement_id = s.id
  FROM platform_statements s
 WHERE o.status = 'completed' AND o.statement_id IS NULL AND s.tenant_id = o.tenant_id
   AND o.updated_at >= s.period_start AND o.updated_at < s.period_end;
UPDATE platform_addon_orders SET canceled_at = updated_at, cancel_reason = 'Cancelado antes do registro de justificativas.'
 WHERE status = 'canceled';

ALTER TABLE platform_addon_orders
  ADD CONSTRAINT platform_addon_orders_statement_fk FOREIGN KEY (tenant_id, statement_id) REFERENCES platform_statements (tenant_id, id),
  ADD CONSTRAINT platform_addon_orders_scheduled CHECK (status <> 'scheduled' OR scheduled_for IS NOT NULL),
  ADD CONSTRAINT platform_addon_orders_completion CHECK ((status = 'completed') = (completed_at IS NOT NULL)),
  ADD CONSTRAINT platform_addon_orders_cancellation CHECK ((status = 'canceled') = (canceled_at IS NOT NULL)
    AND (canceled_at IS NULL) = (cancel_reason IS NULL)
    AND (cancel_reason IS NULL OR length(cancel_reason) BETWEEN 3 AND 500)),
  ADD CONSTRAINT platform_addon_orders_billed_when_completed CHECK (statement_id IS NULL OR status = 'completed');

-- Pedidos concluídos ainda não cobrados, lidos no fechamento do demonstrativo.
CREATE INDEX platform_addon_orders_unbilled_idx ON platform_addon_orders (tenant_id, completed_at)
  WHERE status = 'completed' AND statement_id IS NULL;
CREATE INDEX platform_addon_orders_status_idx ON platform_addon_orders (status, created_at);

-- +goose StatementBegin
-- Pedido encerrado (concluído ou cancelado) não muda de situação, e o
-- vínculo com o demonstrativo, uma vez gravado, é definitivo: a cobrança não
-- se repete nem é desfeita por alteração direta. Organização, serviço, preço
-- e autor do pedido nunca mudam.
CREATE FUNCTION app_platform_addon_orders_guard()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
BEGIN
  IF NEW.tenant_id <> OLD.tenant_id OR NEW.addon_code <> OLD.addon_code OR NEW.price_amount <> OLD.price_amount
     OR NEW.requested_by <> OLD.requested_by OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'dados do pedido de serviço adicional não podem mudar' USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.status IN ('completed', 'canceled') AND (NEW.status <> OLD.status
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.canceled_at IS DISTINCT FROM OLD.canceled_at) THEN
    RAISE EXCEPTION 'pedido de serviço adicional encerrado não muda de situação' USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.statement_id IS NOT NULL AND NEW.statement_id IS DISTINCT FROM OLD.statement_id THEN
    RAISE EXCEPTION 'pedido de serviço adicional já cobrado em demonstrativo' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER platform_addon_orders_guard BEFORE UPDATE ON platform_addon_orders
  FOR EACH ROW EXECUTE FUNCTION app_platform_addon_orders_guard();

-- +goose StatementBegin
-- app_platform_addon_orders alimenta o atendimento pela equipe da
-- plataforma: pedidos de serviço adicional de todas as organizações, sem
-- dados dos clientes dos tenants, paginados por cursor (instante de
-- ordenação e ID do último item). Sem situação, lista os pedidos em aberto
-- (solicitados e agendados) do mais antigo para o mais recente; agendados,
-- pela data do atendimento; concluídos e cancelados, do encerramento mais
-- recente para o mais antigo.
CREATE FUNCTION app_platform_addon_orders(p_status text, p_after_at timestamptz, p_after_id uuid, p_limit integer)
RETURNS SETOF platform_addon_orders
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
  IF p_limit IS NULL OR p_limit < 1 OR p_limit > 201 THEN
    RAISE EXCEPTION 'limite de página inválido' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF (p_after_at IS NULL) <> (p_after_id IS NULL) THEN
    RAISE EXCEPTION 'cursor incompleto' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_status IS NULL OR p_status = 'requested' THEN
    RETURN QUERY SELECT o.* FROM platform_addon_orders o
     WHERE (CASE WHEN p_status IS NULL THEN o.status IN ('requested', 'scheduled') ELSE o.status = p_status END)
       AND (p_after_at IS NULL OR (o.created_at, o.id) > (p_after_at, p_after_id))
     ORDER BY o.created_at, o.id LIMIT p_limit;
  ELSIF p_status = 'scheduled' THEN
    RETURN QUERY SELECT o.* FROM platform_addon_orders o
     WHERE o.status = 'scheduled' AND (p_after_at IS NULL OR (o.scheduled_for, o.id) > (p_after_at, p_after_id))
     ORDER BY o.scheduled_for, o.id LIMIT p_limit;
  ELSIF p_status = 'completed' THEN
    RETURN QUERY SELECT o.* FROM platform_addon_orders o
     WHERE o.status = 'completed' AND (p_after_at IS NULL OR (o.completed_at, o.id) < (p_after_at, p_after_id))
     ORDER BY o.completed_at DESC, o.id DESC LIMIT p_limit;
  ELSIF p_status = 'canceled' THEN
    RETURN QUERY SELECT o.* FROM platform_addon_orders o
     WHERE o.status = 'canceled' AND (p_after_at IS NULL OR (o.canceled_at, o.id) < (p_after_at, p_after_id))
     ORDER BY o.canceled_at DESC, o.id DESC LIMIT p_limit;
  ELSE
    RAISE EXCEPTION 'situação desconhecida: %', p_status USING ERRCODE = 'invalid_parameter_value';
  END IF;
END
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_platform_addon_orders(text, timestamptz, uuid, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_platform_addon_orders(text, timestamptz, uuid, integer) TO recorvia_app;

-- +goose StatementBegin
-- app_platform_tenant_names devolve nome e slug das organizações informadas
-- (identificação dos pedidos no atendimento da equipe da plataforma).
CREATE FUNCTION app_platform_tenant_names(p_ids uuid[])
RETURNS TABLE (tenant_id uuid, name text, slug text)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$ SELECT t.id, t.name, t.slug FROM tenants t WHERE t.id = ANY (p_ids) $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION app_platform_tenant_names(uuid[]) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app_platform_tenant_names(uuid[]) TO recorvia_app;

-- +goose Down
DROP FUNCTION IF EXISTS app_platform_tenant_names(uuid[]);
DROP FUNCTION IF EXISTS app_platform_addon_orders(text, timestamptz, uuid, integer);
DROP TRIGGER IF EXISTS platform_addon_orders_guard ON platform_addon_orders;
-- O fechamento anterior a esta migration cobra os concluídos com updated_at
-- dentro do período: os já cobrados passam a ter updated_at no período do
-- demonstrativo que os cobrou (não são cobrados de novo e são revinculados
-- por um Up seguinte), e os concluídos ainda não cobrados, no período
-- corrente da assinatura (cobrados no próximo fechamento).
UPDATE platform_addon_orders o SET updated_at = s.period_start
  FROM platform_statements s
 WHERE o.statement_id = s.id AND s.tenant_id = o.tenant_id;
UPDATE platform_addon_orders o SET updated_at = GREATEST(o.updated_at, t.current_period_start)
  FROM tenant_platform_subscriptions t
 WHERE o.status = 'completed' AND o.statement_id IS NULL AND t.tenant_id = o.tenant_id;
DROP FUNCTION IF EXISTS app_platform_addon_orders_guard();
DROP INDEX IF EXISTS platform_addon_orders_status_idx;
DROP INDEX IF EXISTS platform_addon_orders_unbilled_idx;
ALTER TABLE platform_addon_orders
  DROP CONSTRAINT IF EXISTS platform_addon_orders_billed_when_completed,
  DROP CONSTRAINT IF EXISTS platform_addon_orders_cancellation,
  DROP CONSTRAINT IF EXISTS platform_addon_orders_completion,
  DROP CONSTRAINT IF EXISTS platform_addon_orders_scheduled,
  DROP CONSTRAINT IF EXISTS platform_addon_orders_statement_fk,
  DROP COLUMN IF EXISTS version,
  DROP COLUMN IF EXISTS statement_id,
  DROP COLUMN IF EXISTS handled_by,
  DROP COLUMN IF EXISTS cancel_reason,
  DROP COLUMN IF EXISTS canceled_at,
  DROP COLUMN IF EXISTS completed_at,
  DROP COLUMN IF EXISTS scheduled_for;
