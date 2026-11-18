package payments

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
)

// receivables é o estado falso de reembolsos, disputas e conciliação.
type receivables struct {
	refunds       map[uuid.UUID]Refund
	refundOrder   []uuid.UUID
	disputes      map[uuid.UUID]Dispute
	disputeOrder  []uuid.UUID
	disputeEvents []DisputeEvent
	runs          map[uuid.UUID]ReconciliationRun
	runOrder      []uuid.UUID
	discrepancies map[uuid.UUID]Discrepancy
	discOrder     []uuid.UUID
	reversals     []string
	overpaid      map[uuid.UUID]money.Money
	creditBalance map[uuid.UUID]money.Money
	dueTenants    []uuid.UUID
	dueErr        error
	// refundClaimLocks registra as cobranças com reivindicação de reembolso
	// serializada.
	refundClaimLocks []uuid.UUID
}

func newReceivables() receivables {
	return receivables{refunds: map[uuid.UUID]Refund{}, disputes: map[uuid.UUID]Dispute{}, runs: map[uuid.UUID]ReconciliationRun{},
		discrepancies: map[uuid.UUID]Discrepancy{}, overpaid: map[uuid.UUID]money.Money{}, creditBalance: map[uuid.UUID]money.Money{}}
}

func (r receivables) clone() receivables {
	c := r
	c.refunds = maps.Clone(r.refunds)
	c.refundOrder = slices.Clone(r.refundOrder)
	c.disputes = maps.Clone(r.disputes)
	c.disputeOrder = slices.Clone(r.disputeOrder)
	c.disputeEvents = slices.Clone(r.disputeEvents)
	c.runs = maps.Clone(r.runs)
	c.runOrder = slices.Clone(r.runOrder)
	c.discrepancies = maps.Clone(r.discrepancies)
	c.discOrder = slices.Clone(r.discOrder)
	c.reversals = slices.Clone(r.reversals)
	c.overpaid = maps.Clone(r.overpaid)
	c.creditBalance = maps.Clone(r.creditBalance)
	c.refundClaimLocks = slices.Clone(r.refundClaimLocks)
	return c
}

func (s *payStore) TenantsDueReconciliation(context.Context, time.Time, time.Duration) ([]uuid.UUID, error) {
	return s.dueTenants, s.dueErr
}

func (s *payStore) InsertRefund(_ context.Context, r Refund) error {
	if err := s.fail("InsertRefund"); err != nil {
		return err
	}
	for _, x := range s.refunds {
		if r.RequestKey != "" && x.RequestKey == r.RequestKey {
			return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outro reembolso.")
		}
	}
	s.refunds[r.ID] = r
	s.refundOrder = append(s.refundOrder, r.ID)
	return nil
}

func (s *payStore) Refund(_ context.Context, id uuid.UUID, _ bool) (Refund, error) {
	if err := s.fail("Refund"); err != nil {
		return Refund{}, err
	}
	r, ok := s.refunds[id]
	if !ok {
		return Refund{}, apperr.NotFound("refund_not_found", "Reembolso não encontrado.")
	}
	return r, nil
}

func (s *payStore) RefundByRequestKey(_ context.Context, key string) (Refund, error) {
	if err := s.fail("RefundByRequestKey"); err != nil {
		return Refund{}, err
	}
	for _, id := range s.refundOrder {
		if r := s.refunds[id]; r.RequestKey == key {
			return r, nil
		}
	}
	return Refund{}, apperr.NotFound("refund_not_found", "Reembolso não encontrado.")
}

func (s *payStore) UpdateRefund(_ context.Context, r *Refund) error {
	if err := s.fail("UpdateRefund"); err != nil {
		return err
	}
	r.Version++
	s.refunds[r.ID] = *r
	return nil
}

func (s *payStore) ChargeRefunds(_ context.Context, chargeID uuid.UUID) ([]Refund, error) {
	if err := s.fail("ChargeRefunds"); err != nil {
		return nil, err
	}
	var out []Refund
	for _, id := range s.refundOrder {
		if r := s.refunds[id]; r.ChargeID == chargeID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *payStore) ChargeOperations(_ context.Context, chargeID uuid.UUID, kind OperationKind) ([]Operation, error) {
	if err := s.fail("ChargeOperations"); err != nil {
		return nil, err
	}
	var out []Operation
	for _, op := range s.sortedOperations() {
		if op.ChargeID == chargeID && op.Kind == kind {
			out = append(out, op)
		}
	}
	return out, nil
}

func (s *payStore) LockChargeRefundClaims(_ context.Context, chargeID uuid.UUID) error {
	if err := s.fail("LockChargeRefundClaims"); err != nil {
		return err
	}
	s.refundClaimLocks = append(s.refundClaimLocks, chargeID)
	return nil
}

func (s *payStore) EnsureOperation(_ context.Context, op Operation) (bool, error) {
	if err := s.fail("EnsureOperation"); err != nil {
		return false, err
	}
	for _, x := range s.operations {
		if x.ChargeID == op.ChargeID && x.Kind == op.Kind && op.Kind == OpCancelCharge {
			return false, nil
		}
	}
	s.operations[op.ID] = op
	return true, nil
}

func (s *payStore) RefundCreditBase(_ context.Context, chargeID uuid.UUID) (money.Money, money.Money, error) {
	if err := s.fail("RefundCreditBase"); err != nil {
		return money.Money{}, money.Money{}, err
	}
	used := brl(0)
	for _, r := range s.refunds {
		if r.ChargeID == chargeID && r.Status == RefundSucceeded {
			used, _ = used.Add(r.CreditAmount)
		}
	}
	over, ok := s.overpaid[chargeID]
	if !ok {
		over = brl(0)
	}
	return over, used, nil
}

func (s *payStore) CustomerCreditBalance(_ context.Context, customerID uuid.UUID) (money.Money, error) {
	if err := s.fail("CustomerCreditBalance"); err != nil {
		return money.Money{}, err
	}
	b, ok := s.creditBalance[customerID]
	if !ok {
		b = brl(0)
	}
	return b, nil
}

func (s *payStore) LatestDispute(_ context.Context, chargeID uuid.UUID) (*Dispute, error) {
	if err := s.fail("LatestDispute"); err != nil {
		return nil, err
	}
	for i := len(s.disputeOrder) - 1; i >= 0; i-- {
		if d := s.disputes[s.disputeOrder[i]]; d.ChargeID == chargeID {
			return &d, nil
		}
	}
	return nil, nil
}

func (s *payStore) InsertDispute(_ context.Context, d Dispute) error {
	if err := s.fail("InsertDispute"); err != nil {
		return err
	}
	s.disputes[d.ID] = d
	s.disputeOrder = append(s.disputeOrder, d.ID)
	return nil
}

func (s *payStore) UpdateDispute(_ context.Context, d *Dispute) error {
	if err := s.fail("UpdateDispute"); err != nil {
		return err
	}
	d.Version++
	s.disputes[d.ID] = *d
	return nil
}

func (s *payStore) InsertDisputeEvent(_ context.Context, e DisputeEvent) error {
	if err := s.fail("InsertDisputeEvent"); err != nil {
		return err
	}
	s.disputeEvents = append(s.disputeEvents, e)
	return nil
}

func (s *payStore) ReverseLedger(_ context.Context, key, description string, at time.Time) error {
	if err := s.fail("ReverseLedger"); err != nil {
		return err
	}
	for _, t := range s.posted {
		if t.IdempotencyKey == key {
			s.reversals = append(s.reversals, key)
			s.posted = append(s.posted, t.Reversal(uuid.New(), at, description))
			return nil
		}
	}
	return nil
}

func (s *payStore) ReconciliationRun(_ context.Context, id uuid.UUID, _ bool) (ReconciliationRun, error) {
	if err := s.fail("ReconciliationRun"); err != nil {
		return ReconciliationRun{}, err
	}
	r, ok := s.runs[id]
	if !ok {
		return ReconciliationRun{}, apperr.NotFound("reconciliation_run_not_found", "Execução não encontrada.")
	}
	return r, nil
}

func (s *payStore) ReconciliationRunByRequestKey(_ context.Context, key string) (ReconciliationRun, error) {
	if err := s.fail("ReconciliationRunByRequestKey"); err != nil {
		return ReconciliationRun{}, err
	}
	for _, id := range s.runOrder {
		if r := s.runs[id]; r.RequestKey == key {
			return r, nil
		}
	}
	return ReconciliationRun{}, apperr.NotFound("reconciliation_run_not_found", "Execução não encontrada.")
}

func (s *payStore) FailStaleReconciliationRuns(_ context.Context, accountID uuid.UUID, now time.Time) (int, error) {
	if err := s.fail("FailStaleReconciliationRuns"); err != nil {
		return 0, err
	}
	n := 0
	for id, r := range s.runs {
		if r.ProviderAccountID == accountID && r.Status == RunRunning && r.LeaseUntil != nil && r.LeaseUntil.Before(now) {
			r.Status, r.FinishedAt, r.LeaseUntil, r.Error = RunFailed, &now, nil, "interrompida"
			s.runs[id] = r
			n++
		}
	}
	return n, nil
}

func (s *payStore) InsertReconciliationRun(_ context.Context, r ReconciliationRun) error {
	if err := s.fail("InsertReconciliationRun"); err != nil {
		return err
	}
	for _, x := range s.runs {
		if x.ProviderAccountID == r.ProviderAccountID && x.Status == RunRunning {
			return apperr.Conflict("reconciliation_running", "Já há uma conciliação em andamento.")
		}
	}
	s.runs[r.ID] = r
	s.runOrder = append(s.runOrder, r.ID)
	return nil
}

func (s *payStore) FinishReconciliationRun(_ context.Context, r ReconciliationRun) error {
	if err := s.fail("FinishReconciliationRun"); err != nil {
		return err
	}
	s.runs[r.ID] = r
	return nil
}

func (s *payStore) ChargesForReconciliation(_ context.Context, sc ReconciliationScope) ([]Charge, error) {
	if err := s.fail("ChargesForReconciliation"); err != nil {
		return nil, err
	}
	in := func(t time.Time) bool { return !t.Before(sc.WindowStart) && t.Before(sc.WindowEnd) }
	var out []Charge
	for _, id := range s.chargeOrder {
		c := s.charges[id]
		if c.ProviderAccountID != sc.AccountID {
			continue
		}
		if in(c.CreatedAt) || in(c.UpdatedAt) || (c.ProviderChargeID != "" && slices.Contains(sc.ProviderChargeIDs, c.ProviderChargeID)) ||
			slices.Contains(sc.ReferenceIDs, c.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *payStore) RefundsForCharges(_ context.Context, ids []uuid.UUID) ([]Refund, error) {
	if err := s.fail("RefundsForCharges"); err != nil {
		return nil, err
	}
	var out []Refund
	for _, id := range s.refundOrder {
		if r := s.refunds[id]; slices.Contains(ids, r.ChargeID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *payStore) UnknownOperations(_ context.Context, now time.Time, limit int) ([]Operation, error) {
	if err := s.fail("UnknownOperations"); err != nil {
		return nil, err
	}
	var out []Operation
	for _, op := range s.sortedOperations() {
		if (op.Status == OpUnknown || op.Stalled(now)) && len(out) < limit {
			out = append(out, op)
		}
	}
	return out, nil
}

func (s *payStore) DiscrepancyByKey(_ context.Context, key string) (*Discrepancy, error) {
	if err := s.fail("DiscrepancyByKey"); err != nil {
		return nil, err
	}
	for _, id := range s.discOrder {
		if d := s.discrepancies[id]; d.DedupeKey == key {
			return &d, nil
		}
	}
	return nil, nil
}

func (s *payStore) InsertDiscrepancy(_ context.Context, d Discrepancy) error {
	if err := s.fail("InsertDiscrepancy"); err != nil {
		return err
	}
	s.discrepancies[d.ID] = d
	s.discOrder = append(s.discOrder, d.ID)
	return nil
}

func (s *payStore) UpdateDiscrepancy(_ context.Context, d *Discrepancy) error {
	if err := s.fail("UpdateDiscrepancy"); err != nil {
		return err
	}
	d.Version++
	s.discrepancies[d.ID] = *d
	return nil
}

func (s *payStore) Discrepancy(_ context.Context, id uuid.UUID, _ bool) (Discrepancy, error) {
	if err := s.fail("Discrepancy"); err != nil {
		return Discrepancy{}, err
	}
	d, ok := s.discrepancies[id]
	if !ok {
		return Discrepancy{}, apperr.NotFound("discrepancy_not_found", "Divergência não encontrada.")
	}
	return d, nil
}

func (s *payStore) OpenDiscrepanciesForSubjects(_ context.Context, provider string, chargeIDs []uuid.UUID, pids []string) ([]Discrepancy, error) {
	if err := s.fail("OpenDiscrepanciesForSubjects"); err != nil {
		return nil, err
	}
	var out []Discrepancy
	for _, id := range s.discOrder {
		d := s.discrepancies[id]
		if d.Status != DiscrepancyOpen || d.Provider != provider {
			continue
		}
		if (d.ChargeID != nil && slices.Contains(chargeIDs, *d.ChargeID)) || (d.ProviderChargeID != "" && slices.Contains(pids, d.ProviderChargeID)) ||
			d.Kind == DiscrepancyUnknownOperation {
			out = append(out, d)
		}
	}
	return out, nil
}

// --- utilidades dos testes de recebíveis ---

func (s *payStore) discrepancyList() []Discrepancy {
	out := make([]Discrepancy, 0, len(s.discOrder))
	for _, id := range s.discOrder {
		out = append(out, s.discrepancies[id])
	}
	return out
}

func (s *payStore) refundList() []Refund {
	out := make([]Refund, 0, len(s.refundOrder))
	for _, id := range s.refundOrder {
		out = append(out, s.refunds[id])
	}
	return out
}

func postedByKind(txs []ledger.Transaction, kind string) []ledger.Transaction {
	var out []ledger.Transaction
	for _, t := range txs {
		if t.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}
