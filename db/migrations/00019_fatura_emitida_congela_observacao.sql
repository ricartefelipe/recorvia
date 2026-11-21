-- Faturamento: fatura emitida mais completa e congelada (ADR-079).
-- - A observação (memo) da fatura emitida passa a ficar congelada junto com
--   os demais campos de emissão; o gatilho anterior (00006) deixava a
--   observação ser reescrita depois da emissão, de modo que o documento
--   entregue ao cliente podia mudar sem histórico. A identidade da fatura
--   (tenant e criação) também fica congelada.
-- - Os dados do emitente (nome, razão social e CNPJ da organização) passam a
--   ser gravados na emissão (issuer_snapshot) e ficam congelados. Faturas
--   emitidas antes desta migration ficam com issuer_snapshot nulo e o
--   documento delas usa os dados atuais da organização.
-- - Fatura cancelada (void) não tem saldo devedor: o crédito aplicado volta
--   ao cliente no cancelamento, e o saldo passa a ser zero. As faturas já
--   canceladas são corrigidas aqui.
-- - Índice da ordenação da listagem e da exportação de faturas (criação e
--   identificador decrescentes).

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_invoice_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.status <> 'draft' THEN
      RAISE EXCEPTION 'fatura emitida % não pode ser excluída', OLD.id USING ERRCODE = 'P0001';
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.status <> 'draft' AND (
       NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR
       NEW.number IS DISTINCT FROM OLD.number OR
       NEW.issued_at IS DISTINCT FROM OLD.issued_at OR
       NEW.due_at IS DISTINCT FROM OLD.due_at OR
       NEW.customer_id IS DISTINCT FROM OLD.customer_id OR
       NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR
       NEW.kind IS DISTINCT FROM OLD.kind OR
       NEW.currency IS DISTINCT FROM OLD.currency OR
       NEW.billing_period_start IS DISTINCT FROM OLD.billing_period_start OR
       NEW.billing_period_end IS DISTINCT FROM OLD.billing_period_end OR
       NEW.subtotal IS DISTINCT FROM OLD.subtotal OR
       NEW.discount_total IS DISTINCT FROM OLD.discount_total OR
       NEW.total IS DISTINCT FROM OLD.total OR
       NEW.credit_applied IS DISTINCT FROM OLD.credit_applied OR
       NEW.customer_snapshot IS DISTINCT FROM OLD.customer_snapshot OR
       NEW.memo IS DISTINCT FROM OLD.memo OR
       NEW.issuer_snapshot IS DISTINCT FROM OLD.issuer_snapshot OR
       NEW.created_at IS DISTINCT FROM OLD.created_at) THEN
    RAISE EXCEPTION 'fatura emitida % não pode ser reescrita', OLD.id USING ERRCODE = 'P0001';
  END IF;
  IF OLD.status IN ('void') AND NEW.status <> OLD.status THEN
    RAISE EXCEPTION 'fatura cancelada % não pode mudar de situação', OLD.id USING ERRCODE = 'P0001';
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Dados do emitente copiados da organização na emissão.
ALTER TABLE invoices ADD COLUMN issuer_snapshot jsonb;

-- Fatura cancelada não tem saldo devedor; as demais seguem a conta da 00006.
ALTER TABLE invoices DROP CONSTRAINT invoices_due_consistency;
UPDATE invoices SET amount_due = 0 WHERE status = 'void' AND amount_due <> 0;
ALTER TABLE invoices ADD CONSTRAINT invoices_due_consistency CHECK (
  CASE WHEN status = 'void' THEN amount_due = 0
       ELSE amount_due = total - credit_applied - amount_paid - amount_credited - amount_written_off END);

-- Ordenação da listagem e da exportação de faturas (ListInvoices e
-- InvoiceListPosition).
CREATE INDEX invoices_tenant_created_idx ON invoices (tenant_id, created_at DESC, id DESC);

-- +goose Down
-- Volta à versão da 00006: sem congelar a observação, o tenant e a criação,
-- sem os dados do emitente gravados na emissão e sem o índice. O saldo das
-- faturas canceladas volta a seguir a conta da 00006 (total menos créditos
-- aplicados, sem pagamentos nem notas de crédito, que impedem o
-- cancelamento).
DROP INDEX IF EXISTS invoices_tenant_created_idx;
ALTER TABLE invoices DROP CONSTRAINT invoices_due_consistency;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app_invoice_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.status <> 'draft' THEN
      RAISE EXCEPTION 'fatura emitida % não pode ser excluída', OLD.id USING ERRCODE = 'P0001';
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.status <> 'draft' AND (
       NEW.number IS DISTINCT FROM OLD.number OR
       NEW.issued_at IS DISTINCT FROM OLD.issued_at OR
       NEW.due_at IS DISTINCT FROM OLD.due_at OR
       NEW.customer_id IS DISTINCT FROM OLD.customer_id OR
       NEW.subscription_id IS DISTINCT FROM OLD.subscription_id OR
       NEW.kind IS DISTINCT FROM OLD.kind OR
       NEW.currency IS DISTINCT FROM OLD.currency OR
       NEW.billing_period_start IS DISTINCT FROM OLD.billing_period_start OR
       NEW.billing_period_end IS DISTINCT FROM OLD.billing_period_end OR
       NEW.subtotal IS DISTINCT FROM OLD.subtotal OR
       NEW.discount_total IS DISTINCT FROM OLD.discount_total OR
       NEW.total IS DISTINCT FROM OLD.total OR
       NEW.credit_applied IS DISTINCT FROM OLD.credit_applied OR
       NEW.customer_snapshot IS DISTINCT FROM OLD.customer_snapshot) THEN
    RAISE EXCEPTION 'fatura emitida % não pode ser reescrita', OLD.id USING ERRCODE = 'P0001';
  END IF;
  IF OLD.status IN ('void') AND NEW.status <> OLD.status THEN
    RAISE EXCEPTION 'fatura cancelada % não pode mudar de situação', OLD.id USING ERRCODE = 'P0001';
  END IF;
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

UPDATE invoices SET amount_due = total - credit_applied - amount_paid - amount_credited - amount_written_off WHERE status = 'void';
ALTER TABLE invoices ADD CONSTRAINT invoices_due_consistency CHECK (amount_due = total - credit_applied - amount_paid - amount_credited - amount_written_off);
ALTER TABLE invoices DROP COLUMN IF EXISTS issuer_snapshot;
