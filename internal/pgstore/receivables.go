package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// Reembolsos, disputas, cancelamento no provedor e conciliação.

// TenantsDueReconciliation descobre tenants elegíveis à conciliação agendada.
func (p *Payments) TenantsDueReconciliation(ctx context.Context, now time.Time, every time.Duration) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsDueReconciliation(ctx, dbgen.TenantsDueReconciliationParams{Now: now, IntervalSeconds: int32(every.Seconds())})
		return err
	})
	return out, err
}

func refundFromRow(r dbgen.Refund) (payments.Refund, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return payments.Refund{}, err
	}
	return payments.Refund{ID: r.ID, TenantID: r.TenantID, ChargeID: r.ChargeID, Provider: r.Provider, Origin: payments.RefundOrigin(r.Origin),
		Status: payments.RefundStatus(r.Status), Amount: money.New(r.Amount, cur), CreditAmount: money.New(r.CreditAmount, cur), Reason: r.Reason,
		ProviderRefundID: deref(r.ProviderRefundID), FailureReason: deref(r.FailureReason), RequestedBy: r.RequestedBy,
		RequestKey: deref(r.RequestKey), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CompletedAt: r.CompletedAt, Version: int(r.Version)}, nil
}

func refundsFromRows(rows []dbgen.Refund) ([]payments.Refund, error) {
	out := make([]payments.Refund, 0, len(rows))
	for _, r := range rows {
		x, err := refundFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func (tx paymentsTx) InsertRefund(ctx context.Context, r payments.Refund) error {
	err := tx.q.InsertRefund(ctx, dbgen.InsertRefundParams{ID: r.ID, TenantID: tx.tenantID, ChargeID: r.ChargeID, Provider: r.Provider,
		Origin: string(r.Origin), Status: string(r.Status), Amount: r.Amount.Amount(), CreditAmount: r.CreditAmount.Amount(),
		Currency: r.Amount.Currency().Code(), Reason: r.Reason, ProviderRefundID: strPtr(r.ProviderRefundID), FailureReason: strPtr(r.FailureReason),
		RequestedBy: r.RequestedBy, RequestKey: strPtr(r.RequestKey), CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt})
	switch {
	case postgres.IsUniqueViolation(err, "refunds_request_key"):
		return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outro reembolso.")
	case postgres.IsUniqueViolation(err, "refunds_provider_refund_key"):
		return apperr.Conflict("provider_refund_linked", "O reembolso do provedor já está registrado.")
	}
	return err
}

func (tx paymentsTx) Refund(ctx context.Context, id uuid.UUID, forUpdate bool) (payments.Refund, error) {
	var r dbgen.Refund
	var err error
	if forUpdate {
		r, err = tx.q.GetRefundForUpdate(ctx, dbgen.GetRefundForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetRefund(ctx, dbgen.GetRefundParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return payments.Refund{}, notFound(err, "refund_not_found", "Reembolso não encontrado.")
	}
	return refundFromRow(r)
}

func (tx paymentsTx) RefundByRequestKey(ctx context.Context, key string) (payments.Refund, error) {
	r, err := tx.q.GetRefundByRequestKey(ctx, dbgen.GetRefundByRequestKeyParams{TenantID: tx.tenantID, RequestKey: &key})
	if err != nil {
		return payments.Refund{}, notFound(err, "refund_not_found", "Reembolso não encontrado.")
	}
	return refundFromRow(r)
}

func (tx paymentsTx) UpdateRefund(ctx context.Context, r *payments.Refund) error {
	n, err := tx.q.UpdateRefund(ctx, dbgen.UpdateRefundParams{TenantID: tx.tenantID, ID: r.ID, Status: string(r.Status),
		CreditAmount: r.CreditAmount.Amount(), ProviderRefundID: strPtr(r.ProviderRefundID), FailureReason: strPtr(r.FailureReason),
		CompletedAt: r.CompletedAt, Version: int32(r.Version)})
	if err != nil {
		if postgres.IsUniqueViolation(err, "refunds_provider_refund_key") {
			return apperr.Conflict("provider_refund_linked", "O reembolso do provedor já está registrado.")
		}
		return err
	}
	if err := versionConflict(n, "O reembolso foi alterado concorrentemente."); err != nil {
		return err
	}
	r.Version++
	return nil
}

func (tx paymentsTx) ChargeRefunds(ctx context.Context, chargeID uuid.UUID) ([]payments.Refund, error) {
	rows, err := tx.q.ListChargeRefunds(ctx, dbgen.ListChargeRefundsParams{TenantID: tx.tenantID, ChargeID: chargeID})
	if err != nil {
		return nil, err
	}
	return refundsFromRows(rows)
}

func (tx paymentsTx) ChargeOperations(ctx context.Context, chargeID uuid.UUID, kind payments.OperationKind) ([]payments.Operation, error) {
	rows, err := tx.q.ListChargeOperations(ctx, dbgen.ListChargeOperationsParams{TenantID: tx.tenantID, ChargeID: chargeID, Kind: string(kind)})
	if err != nil {
		return nil, err
	}
	return operationsFromRows(rows)
}

func (tx paymentsTx) LockChargeRefundClaims(ctx context.Context, chargeID uuid.UUID) error {
	return tx.q.LockChargeRefundClaims(ctx, dbgen.LockChargeRefundClaimsParams{TenantID: tx.tenantID, ChargeID: chargeID})
}

func operationsFromRows(rows []dbgen.PaymentOperation) ([]payments.Operation, error) {
	out := make([]payments.Operation, 0, len(rows))
	for _, r := range rows {
		op, err := operationFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func (tx paymentsTx) EnsureOperation(ctx context.Context, op payments.Operation) (bool, error) {
	n, err := tx.q.InsertOperationIfAbsent(ctx, dbgen.InsertOperationIfAbsentParams{ID: op.ID, TenantID: tx.tenantID, ChargeID: op.ChargeID,
		RefundID: op.RefundID, Kind: string(op.Kind), Status: string(op.Status), ExternalReference: op.ExternalReference,
		NextAttemptAt: op.NextAttemptAt, CreatedAt: op.CreatedAt})
	return n == 1, err
}

func (tx paymentsTx) RefundCreditBase(ctx context.Context, chargeID uuid.UUID) (money.Money, money.Money, error) {
	over, err := tx.q.ChargeOverpaymentAmount(ctx, dbgen.ChargeOverpaymentAmountParams{TenantID: tx.tenantID, ChargeID: &chargeID})
	if err != nil {
		return money.Money{}, money.Money{}, err
	}
	used, err := tx.q.ChargeRefundedCreditAmount(ctx, dbgen.ChargeRefundedCreditAmountParams{TenantID: tx.tenantID, ChargeID: chargeID})
	if err != nil {
		return money.Money{}, money.Money{}, err
	}
	return brl(over), brl(used), nil
}

func (tx paymentsTx) LockCustomerCredits(ctx context.Context, customerID uuid.UUID) error {
	_, err := tx.q.LockCustomerCredits(ctx, dbgen.LockCustomerCreditsParams{TenantID: tx.tenantID, CustomerID: customerID})
	return notFound(err, "customer_not_found", "Cliente não encontrado.")
}

func (tx paymentsTx) CustomerCreditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error) {
	return tx.creditBalance(ctx, customerID)
}

func disputeFromRow(r dbgen.Dispute) (payments.Dispute, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return payments.Dispute{}, err
	}
	return payments.Dispute{ID: r.ID, TenantID: r.TenantID, ChargeID: r.ChargeID, Provider: r.Provider, Status: payments.DisputeStatus(r.Status),
		Amount: money.New(r.Amount, cur), ProviderStatus: deref(r.ProviderStatus), OpenedAt: r.OpenedAt, ClosedAt: r.ClosedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}, nil
}

func (tx paymentsTx) LatestDispute(ctx context.Context, chargeID uuid.UUID) (*payments.Dispute, error) {
	r, err := tx.q.GetLatestDisputeForCharge(ctx, dbgen.GetLatestDisputeForChargeParams{TenantID: tx.tenantID, ChargeID: chargeID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := disputeFromRow(r)
	return &d, err
}

func (tx paymentsTx) InsertDispute(ctx context.Context, d payments.Dispute) error {
	err := tx.q.InsertDispute(ctx, dbgen.InsertDisputeParams{ID: d.ID, TenantID: tx.tenantID, ChargeID: d.ChargeID, Provider: d.Provider,
		Status: string(d.Status), Amount: d.Amount.Amount(), Currency: d.Amount.Currency().Code(), ProviderStatus: strPtr(d.ProviderStatus),
		OpenedAt: d.OpenedAt, ClosedAt: d.ClosedAt, CreatedAt: d.CreatedAt})
	if postgres.IsUniqueViolation(err, "disputes_open_key") {
		return apperr.Conflict("dispute_already_open", "A cobrança já tem disputa aberta.")
	}
	return err
}

func (tx paymentsTx) UpdateDispute(ctx context.Context, d *payments.Dispute) error {
	n, err := tx.q.UpdateDispute(ctx, dbgen.UpdateDisputeParams{TenantID: tx.tenantID, ID: d.ID, Status: string(d.Status),
		ProviderStatus: strPtr(d.ProviderStatus), ClosedAt: d.ClosedAt, Version: int32(d.Version)})
	if err != nil {
		return err
	}
	if err := versionConflict(n, "A disputa foi alterada concorrentemente."); err != nil {
		return err
	}
	d.Version++
	return nil
}

func (tx paymentsTx) InsertDisputeEvent(ctx context.Context, e payments.DisputeEvent) error {
	details, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	if e.Details == nil {
		details = []byte("{}")
	}
	var from *string
	if e.From != nil {
		f := string(*e.From)
		from = &f
	}
	return tx.q.InsertDisputeEvent(ctx, dbgen.InsertDisputeEventParams{ID: e.ID, TenantID: tx.tenantID, DisputeID: e.DisputeID, FromStatus: from,
		ToStatus: string(e.To), ProviderStatus: strPtr(e.ProviderStatus), OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Details: details})
}

func runFromRow(r dbgen.ReconciliationRun) payments.ReconciliationRun {
	return payments.ReconciliationRun{ID: r.ID, TenantID: r.TenantID, ProviderAccountID: r.ProviderAccountID, Provider: r.Provider,
		Trigger: payments.RunTrigger(r.TriggerType), Status: payments.RunStatus(r.Status), WindowStart: r.WindowStart, WindowEnd: r.WindowEnd,
		CheckedCount: int(r.CheckedCount), DiscrepancyCount: int(r.DiscrepancyCount), Error: deref(r.Error), RequestedBy: r.RequestedBy,
		RequestKey: deref(r.RequestKey), LeaseUntil: r.LeaseUntil, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
}

func (tx paymentsTx) ReconciliationRun(ctx context.Context, id uuid.UUID, forUpdate bool) (payments.ReconciliationRun, error) {
	var r dbgen.ReconciliationRun
	var err error
	if forUpdate {
		r, err = tx.q.GetReconciliationRunForUpdate(ctx, dbgen.GetReconciliationRunForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetReconciliationRun(ctx, dbgen.GetReconciliationRunParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return payments.ReconciliationRun{}, notFound(err, "reconciliation_run_not_found", "Execução de conciliação não encontrada.")
	}
	return runFromRow(r), nil
}

func (tx paymentsTx) ReconciliationRunByRequestKey(ctx context.Context, key string) (payments.ReconciliationRun, error) {
	r, err := tx.q.GetReconciliationRunByRequestKey(ctx, dbgen.GetReconciliationRunByRequestKeyParams{TenantID: tx.tenantID, RequestKey: &key})
	if err != nil {
		return payments.ReconciliationRun{}, notFound(err, "reconciliation_run_not_found", "Execução de conciliação não encontrada.")
	}
	return runFromRow(r), nil
}

func (tx paymentsTx) FailStaleReconciliationRuns(ctx context.Context, accountID uuid.UUID, now time.Time) (int, error) {
	msg := "Execução interrompida antes da conclusão (lease expirado)."
	n, err := tx.q.FailStaleReconciliationRuns(ctx, dbgen.FailStaleReconciliationRunsParams{TenantID: tx.tenantID, ProviderAccountID: accountID,
		Now: &now, Error: &msg})
	return int(n), err
}

func (tx paymentsTx) InsertReconciliationRun(ctx context.Context, r payments.ReconciliationRun) error {
	err := tx.q.InsertReconciliationRun(ctx, dbgen.InsertReconciliationRunParams{ID: r.ID, TenantID: tx.tenantID, ProviderAccountID: r.ProviderAccountID,
		Provider: r.Provider, TriggerType: string(r.Trigger), Status: string(r.Status), WindowStart: r.WindowStart, WindowEnd: r.WindowEnd,
		RequestedBy: r.RequestedBy, RequestKey: strPtr(r.RequestKey), LeaseUntil: r.LeaseUntil, StartedAt: r.StartedAt})
	switch {
	case postgres.IsUniqueViolation(err, "reconciliation_runs_running_key"):
		return apperr.Conflict("reconciliation_running", "Já há uma conciliação em andamento para este provedor.")
	case postgres.IsUniqueViolation(err, "reconciliation_runs_request_key"):
		return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outra execução.")
	}
	return err
}

func (tx paymentsTx) FinishReconciliationRun(ctx context.Context, r payments.ReconciliationRun) error {
	return tx.q.FinishReconciliationRun(ctx, dbgen.FinishReconciliationRunParams{TenantID: tx.tenantID, ID: r.ID, Status: string(r.Status),
		CheckedCount: int32(r.CheckedCount), DiscrepancyCount: int32(r.DiscrepancyCount), Error: strPtr(r.Error), FinishedAt: r.FinishedAt})
}

func (tx paymentsTx) ChargesForReconciliation(ctx context.Context, sc payments.ReconciliationScope) ([]payments.Charge, error) {
	pids, refs := sc.ProviderChargeIDs, sc.ReferenceIDs
	if pids == nil {
		pids = []string{}
	}
	if refs == nil {
		refs = []uuid.UUID{}
	}
	rows, err := tx.q.ListChargesForReconciliation(ctx, dbgen.ListChargesForReconciliationParams{TenantID: tx.tenantID, ProviderAccountID: sc.AccountID,
		WindowStart: sc.WindowStart, WindowEnd: sc.WindowEnd, ProviderChargeIds: pids, ReferenceIds: refs, RowLimit: int32(sc.Limit)})
	if err != nil {
		return nil, err
	}
	out := make([]payments.Charge, 0, len(rows))
	for _, r := range rows {
		c, err := chargeFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (tx paymentsTx) ChargesForRotation(ctx context.Context, sc payments.RotationScope) ([]payments.Charge, error) {
	rows, err := tx.q.ListChargesForRotation(ctx, dbgen.ListChargesForRotationParams{TenantID: tx.tenantID, ProviderAccountID: sc.AccountID,
		Since: sc.Since, WindowStart: sc.WindowStart, WindowEnd: sc.WindowEnd, RowLimit: int32(sc.Limit)})
	if err != nil {
		return nil, err
	}
	out := make([]payments.Charge, 0, len(rows))
	for _, r := range rows {
		c, err := chargeFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (tx paymentsTx) MarkChargesReconciled(ctx context.Context, chargeIDs []uuid.UUID, at time.Time) error {
	if len(chargeIDs) == 0 {
		return nil
	}
	return tx.q.MarkChargesReconciled(ctx, dbgen.MarkChargesReconciledParams{TenantID: tx.tenantID, CheckedAt: at, ChargeIds: chargeIDs})
}

func (tx paymentsTx) RefundsForCharges(ctx context.Context, chargeIDs []uuid.UUID) ([]payments.Refund, error) {
	if len(chargeIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.q.ListRefundsForCharges(ctx, dbgen.ListRefundsForChargesParams{TenantID: tx.tenantID, ChargeIds: chargeIDs})
	if err != nil {
		return nil, err
	}
	return refundsFromRows(rows)
}

func (tx paymentsTx) UnknownOperations(ctx context.Context, now time.Time, limit int) ([]payments.Operation, error) {
	rows, err := tx.q.ListUnknownOperations(ctx, dbgen.ListUnknownOperationsParams{TenantID: tx.tenantID, Now: &now, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	return operationsFromRows(rows)
}

func amountPtr(m *money.Money) *int64 {
	if m == nil {
		return nil
	}
	v := m.Amount()
	return &v
}

func moneyFromPtr(v *int64) *money.Money {
	if v == nil {
		return nil
	}
	m := brl(*v)
	return &m
}

func discrepancyFromRow(r dbgen.ReconciliationDiscrepancy) (payments.Discrepancy, error) {
	local, err := jsonMap(r.LocalValue)
	if err != nil {
		return payments.Discrepancy{}, err
	}
	prov, err := jsonMap(r.ProviderValue)
	if err != nil {
		return payments.Discrepancy{}, err
	}
	return payments.Discrepancy{ID: r.ID, TenantID: r.TenantID, RunID: r.RunID, LastRunID: r.LastRunID, Kind: payments.DiscrepancyKind(r.Kind),
		Status: payments.DiscrepancyStatus(r.Status), DedupeKey: r.DedupeKey, Provider: r.Provider, ChargeID: r.ChargeID, RefundID: r.RefundID,
		DisputeID: r.DisputeID, OperationID: r.OperationID, ProviderChargeID: deref(r.ProviderChargeID), LocalAmount: moneyFromPtr(r.LocalAmount),
		ProviderAmount: moneyFromPtr(r.ProviderAmount), LocalValue: local, ProviderValue: prov, Occurrences: int(r.Occurrences),
		FirstDetectedAt: r.FirstDetectedAt, LastDetectedAt: r.LastDetectedAt, Resolution: payments.Resolution(deref(r.Resolution)),
		Justification: deref(r.Justification), ResolvedBy: r.ResolvedBy, ResolvedAt: r.ResolvedAt, LedgerTransactionID: r.LedgerTransactionID,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}, nil
}

func jsonObject(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

func (tx paymentsTx) DiscrepancyByKey(ctx context.Context, key string) (*payments.Discrepancy, error) {
	r, err := tx.q.GetDiscrepancyByKeyForUpdate(ctx, dbgen.GetDiscrepancyByKeyForUpdateParams{TenantID: tx.tenantID, DedupeKey: key})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := discrepancyFromRow(r)
	return &d, err
}

func (tx paymentsTx) InsertDiscrepancy(ctx context.Context, d payments.Discrepancy) error {
	local, err := jsonObject(d.LocalValue)
	if err != nil {
		return err
	}
	prov, err := jsonObject(d.ProviderValue)
	if err != nil {
		return err
	}
	err = tx.q.InsertDiscrepancy(ctx, dbgen.InsertDiscrepancyParams{ID: d.ID, TenantID: tx.tenantID, RunID: d.RunID, Kind: string(d.Kind),
		DedupeKey: d.DedupeKey, Provider: d.Provider, ChargeID: d.ChargeID, RefundID: d.RefundID, DisputeID: d.DisputeID, OperationID: d.OperationID,
		ProviderChargeID: strPtr(d.ProviderChargeID), LocalAmount: amountPtr(d.LocalAmount), ProviderAmount: amountPtr(d.ProviderAmount),
		LocalValue: local, ProviderValue: prov, DetectedAt: d.FirstDetectedAt})
	if postgres.IsUniqueViolation(err, "reconciliation_discrepancies_dedupe_key") {
		return apperr.Conflict("discrepancy_exists", "A divergência foi registrada concorrentemente.")
	}
	return err
}

func (tx paymentsTx) UpdateDiscrepancy(ctx context.Context, d *payments.Discrepancy) error {
	local, err := jsonObject(d.LocalValue)
	if err != nil {
		return err
	}
	prov, err := jsonObject(d.ProviderValue)
	if err != nil {
		return err
	}
	n, err := tx.q.UpdateDiscrepancy(ctx, dbgen.UpdateDiscrepancyParams{TenantID: tx.tenantID, ID: d.ID, LastRunID: d.LastRunID, Status: string(d.Status),
		LocalAmount: amountPtr(d.LocalAmount), ProviderAmount: amountPtr(d.ProviderAmount), LocalValue: local, ProviderValue: prov,
		Occurrences: int32(d.Occurrences), LastDetectedAt: d.LastDetectedAt, Resolution: strPtr(string(d.Resolution)),
		Justification: strPtr(d.Justification), ResolvedBy: d.ResolvedBy, ResolvedAt: d.ResolvedAt, LedgerTransactionID: d.LedgerTransactionID,
		Version: int32(d.Version)})
	if err != nil {
		return err
	}
	if err := versionConflict(n, "A divergência foi alterada concorrentemente."); err != nil {
		return err
	}
	d.Version++
	return nil
}

func (tx paymentsTx) Discrepancy(ctx context.Context, id uuid.UUID, forUpdate bool) (payments.Discrepancy, error) {
	var r dbgen.ReconciliationDiscrepancy
	var err error
	if forUpdate {
		r, err = tx.q.GetDiscrepancyForUpdate(ctx, dbgen.GetDiscrepancyForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetDiscrepancy(ctx, dbgen.GetDiscrepancyParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return payments.Discrepancy{}, notFound(err, "discrepancy_not_found", "Divergência não encontrada.")
	}
	return discrepancyFromRow(r)
}

func (tx paymentsTx) OpenDiscrepanciesForSubjects(ctx context.Context, provider string, chargeIDs []uuid.UUID, providerChargeIDs []string) ([]payments.Discrepancy, error) {
	if chargeIDs == nil {
		chargeIDs = []uuid.UUID{}
	}
	if providerChargeIDs == nil {
		providerChargeIDs = []string{}
	}
	rows, err := tx.q.ListOpenDiscrepanciesForSubjects(ctx, dbgen.ListOpenDiscrepanciesForSubjectsParams{TenantID: tx.tenantID, Provider: provider,
		ChargeIds: chargeIDs, ProviderChargeIds: providerChargeIDs})
	if err != nil {
		return nil, err
	}
	out := make([]payments.Discrepancy, 0, len(rows))
	for _, r := range rows {
		d, err := discrepancyFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// --- consultas de leitura para a API ---

// ListRefunds lista reembolsos do tenant (mais recentes primeiro).
func (s *Store) ListRefunds(ctx context.Context, tenantID uuid.UUID, chargeID *uuid.UUID, limit int, cursor string) ([]payments.Refund, string, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []payments.Refund
	next := ""
	err = s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListRefunds(ctx, dbgen.ListRefundsParams{TenantID: tenantID, ChargeID: chargeID, CursorTs: cts, CursorID: cid,
			RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		if len(rows) > limit {
			next = timeCursor(rows[limit-1].CreatedAt, rows[limit-1].ID)
			rows = rows[:limit]
		}
		out, err = refundsFromRows(rows)
		return err
	})
	return out, next, err
}

// DisputeWithHistory é uma disputa com o histórico de transições.
type DisputeWithHistory struct {
	payments.Dispute
	History []payments.DisputeEvent
}

// ListDisputes lista as disputas mais recentes do tenant com histórico.
func (s *Store) ListDisputes(ctx context.Context, tenantID uuid.UUID, chargeID *uuid.UUID, limit int) ([]DisputeWithHistory, error) {
	limit = pageLimit(limit)
	var out []DisputeWithHistory
	err := s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListDisputes(ctx, dbgen.ListDisputesParams{TenantID: tenantID, ChargeID: chargeID, RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(rows))
		idx := map[uuid.UUID]int{}
		for _, r := range rows {
			d, err := disputeFromRow(r)
			if err != nil {
				return err
			}
			idx[d.ID] = len(out)
			ids = append(ids, d.ID)
			out = append(out, DisputeWithHistory{Dispute: d})
		}
		if len(ids) == 0 {
			return nil
		}
		evs, err := b.q.ListDisputeEvents(ctx, dbgen.ListDisputeEventsParams{TenantID: tenantID, DisputeIds: ids})
		if err != nil {
			return err
		}
		for _, e := range evs {
			details, err := jsonMap(e.Details)
			if err != nil {
				return err
			}
			ev := payments.DisputeEvent{ID: e.ID, TenantID: e.TenantID, DisputeID: e.DisputeID, To: payments.DisputeStatus(e.ToStatus),
				ProviderStatus: deref(e.ProviderStatus), OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, Details: details}
			if e.FromStatus != nil {
				from := payments.DisputeStatus(*e.FromStatus)
				ev.From = &from
			}
			i := idx[e.DisputeID]
			out[i].History = append(out[i].History, ev)
		}
		return nil
	})
	return out, err
}

// ListReconciliationRuns lista as execuções mais recentes.
func (s *Store) ListReconciliationRuns(ctx context.Context, tenantID uuid.UUID, limit int) ([]payments.ReconciliationRun, error) {
	limit = pageLimit(limit)
	var out []payments.ReconciliationRun
	err := s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListReconciliationRuns(ctx, dbgen.ListReconciliationRunsParams{TenantID: tenantID, RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, runFromRow(r))
		}
		return nil
	})
	return out, err
}

// ListDiscrepancies lista divergências com filtros de situação e tipo.
func (s *Store) ListDiscrepancies(ctx context.Context, tenantID uuid.UUID, status, kind string, limit int) ([]payments.Discrepancy, error) {
	limit = pageLimit(limit)
	var out []payments.Discrepancy
	err := s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListDiscrepancies(ctx, dbgen.ListDiscrepanciesParams{TenantID: tenantID, Status: strPtr(status), Kind: strPtr(kind),
			RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			d, err := discrepancyFromRow(r)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

// OpenDiscrepancyStats conta divergências abertas por tipo (todos os
// tenants, sem identificá-los), para métricas.
func (s *Store) OpenDiscrepancyStats(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.OpenDiscrepancyStats(ctx)
		for _, r := range rows {
			out[r.Kind] = r.Total
		}
		return err
	})
	return out, err
}

// RefundStats conta reembolsos por situação (todos os tenants, sem
// identificá-los), para métricas.
func (s *Store) RefundStats(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.RefundStats(ctx)
		for _, r := range rows {
			out[r.Status] = r.Total
		}
		return err
	})
	return out, err
}
