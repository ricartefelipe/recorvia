-- Painel financeiro (ADR-074): índices para os recebíveis na posição do fim
-- de um mês encerrado sem percorrer o histórico inteiro do ledger. A consulta
-- DashboardReceivablesAt parte das faturas que podiam ter saldo na posição
-- (abertas ou encerradas depois dela) e soma só as partidas delas, achadas
-- pela referência do lançamento: a fatura, os seus pagamentos e as suas notas
-- de crédito.

-- +goose Up

-- Pagamentos de uma fatura (as partidas de contas a receber do recebimento
-- referenciam o pagamento).
CREATE INDEX payments_invoice_idx ON payments (tenant_id, invoice_id);

-- Faturas encerradas pelo instante em que deixaram de ter saldo: paga,
-- cancelada ou baixada como incobrável. Um índice por coluna: sob RLS, só
-- comparações simples de coluna viram condição de índice.
CREATE INDEX invoices_paid_at_idx ON invoices (tenant_id, paid_at) WHERE paid_at IS NOT NULL;
CREATE INDEX invoices_voided_at_idx ON invoices (tenant_id, voided_at) WHERE voided_at IS NOT NULL;
CREATE INDEX invoices_uncollectible_at_idx ON invoices (tenant_id, uncollectible_at) WHERE uncollectible_at IS NOT NULL;

-- Base da inadimplência: faturas emitidas por vencimento, em qualquer situação.
CREATE INDEX invoices_issued_due_idx ON invoices (tenant_id, due_at) WHERE status <> 'draft';

-- +goose Down
DROP INDEX IF EXISTS invoices_issued_due_idx;
DROP INDEX IF EXISTS invoices_uncollectible_at_idx;
DROP INDEX IF EXISTS invoices_voided_at_idx;
DROP INDEX IF EXISTS invoices_paid_at_idx;
DROP INDEX IF EXISTS payments_invoice_idx;
