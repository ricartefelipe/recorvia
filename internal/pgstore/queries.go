package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// Consultas de leitura específicas para a API (sem regra de negócio).

// AppendAudit grava uma entrada de auditoria em transação própria (ações
// somente de leitura auditadas, como exportações).
func (s *Store) AppendAudit(ctx context.Context, tenantID uuid.UUID, e audit.Entry) error {
	return s.inTenant(ctx, tenantID, func(b base) error { return b.Audit(ctx, e) })
}

// InvoiceCharges lista as cobranças de uma fatura.
func (s *Store) InvoiceCharges(ctx context.Context, tenantID, invoiceID uuid.UUID) ([]payments.Charge, error) {
	var out []payments.Charge
	err := s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListInvoiceCharges(ctx, dbgen.ListInvoiceChargesParams{TenantID: tenantID, InvoiceID: invoiceID})
		if err != nil {
			return err
		}
		for _, r := range rows {
			c, err := chargeFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// ListCharges lista cobranças com filtros.
func (s *Store) ListCharges(ctx context.Context, tenantID uuid.UUID, status string, invoiceID *uuid.UUID, limit int, cursor string) ([]payments.Charge, string, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []payments.Charge
	next := ""
	err = s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListCharges(ctx, dbgen.ListChargesParams{TenantID: tenantID, Status: strPtr(status), InvoiceID: invoiceID,
			CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		for i, r := range rows {
			if i == limit {
				next = timeCursor(rows[i-1].CreatedAt, rows[i-1].ID)
				break
			}
			c, err := chargeFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	return out, next, err
}

// ListPayments lista recebimentos.
func (s *Store) ListPayments(ctx context.Context, tenantID uuid.UUID, invoiceID *uuid.UUID, limit int, cursor string) ([]payments.Payment, string, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []payments.Payment
	next := ""
	err = s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListPayments(ctx, dbgen.ListPaymentsParams{TenantID: tenantID, InvoiceID: invoiceID, CursorTs: cts, CursorID: cid,
			RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		for i, r := range rows {
			if i == limit {
				next = timeCursor(rows[i-1].PaidAt, rows[i-1].ID)
				break
			}
			out = append(out, paymentFromRow(r))
		}
		return nil
	})
	return out, next, err
}

// ListOperations lista operações externas, das criadas mais recentemente para
// as mais antigas, em páginas por cursor.
func (s *Store) ListOperations(ctx context.Context, tenantID uuid.UUID, status string, limit int, cursor string) ([]payments.Operation, string, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []payments.Operation
	next := ""
	err = s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListOperations(ctx, dbgen.ListOperationsParams{TenantID: tenantID, Status: strPtr(status), CursorTs: cts, CursorID: cid,
			RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		rows, next = keysetPage(rows, limit, func(r dbgen.PaymentOperation) (time.Time, uuid.UUID) { return r.CreatedAt, r.ID })
		for _, r := range rows {
			op, err := operationFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, op)
		}
		return nil
	})
	return out, next, err
}

// Operation devolve a operação externa do tenant.
func (s *Store) Operation(ctx context.Context, tenantID, id uuid.UUID) (payments.Operation, error) {
	var op payments.Operation
	err := s.inTenant(ctx, tenantID, func(b base) error {
		r, err := b.q.GetOperation(ctx, dbgen.GetOperationParams{TenantID: tenantID, ID: id})
		if err != nil {
			return notFound(err, "operation_not_found", "Operação não encontrada.")
		}
		op, err = operationFromRow(r)
		return err
	})
	return op, err
}

// Clock expõe o relógio do store.
func (s *Store) Now() time.Time { return s.clock.Now() }
