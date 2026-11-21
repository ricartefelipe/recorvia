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
	settlements   []RecoverySettlement
	// disputeLocks registra as disputas bloqueadas (Dispute com forUpdate).
	disputeLocks  []uuid.UUID
	runs          map[uuid.UUID]ReconciliationRun
	runOrder      []uuid.UUID
	discrepancies map[uuid.UUID]Discrepancy
	discOrder     []uuid.UUID
	reversals     []string
	overpaid      map[uuid.UUID]money.Money
	creditBalance map[uuid.UUID]money.Money
	dueTenants    []uuid.UUID
	dueErr        error
	claimableErr  error
	// refundClaimLocks registra as cobranças com reivindicação de reembolso
	// serializada.
	refundClaimLocks []uuid.UUID
	// creditSteps registra, em ordem, o bloqueio do crédito do cliente
	// ("lock") e as leituras do saldo ("balance").
	creditSteps []string
	// checkedAt guarda a última verificação de cada cobrança pela conciliação.
	checkedAt      map[uuid.UUID]time.Time
	rotationScopes []RotationScope
}

func newReceivables() receivables {
	return receivables{refunds: map[uuid.UUID]Refund{}, disputes: map[uuid.UUID]Dispute{}, runs: map[uuid.UUID]ReconciliationRun{},
		discrepancies: map[uuid.UUID]Discrepancy{}, overpaid: map[uuid.UUID]money.Money{}, creditBalance: map[uuid.UUID]money.Money{},
		checkedAt: map[uuid.UUID]time.Time{}}
}

func (r receivables) clone() receivables {
	c := r
	c.refunds = maps.Clone(r.refunds)
	c.refundOrder = slices.Clone(r.refundOrder)
	c.disputes = maps.Clone(r.disputes)
	c.disputeOrder = slices.Clone(r.disputeOrder)
	c.disputeEvents = slices.Clone(r.disputeEvents)
	c.settlements = slices.Clone(r.settlements)
	c.disputeLocks = slices.Clone(r.disputeLocks)
	c.runs = maps.Clone(r.runs)
	c.runOrder = slices.Clone(r.runOrder)
	c.discrepancies = maps.Clone(r.discrepancies)
	c.discOrder = slices.Clone(r.discOrder)
	c.reversals = slices.Clone(r.reversals)
	c.overpaid = maps.Clone(r.overpaid)
	c.creditBalance = maps.Clone(r.creditBalance)
	c.refundClaimLocks = slices.Clone(r.refundClaimLocks)
	c.creditSteps = slices.Clone(r.creditSteps)
	c.checkedAt = maps.Clone(r.checkedAt)
	c.rotationScopes = slices.Clone(r.rotationScopes)
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

func (s *payStore) OverpaymentCreditBase(_ context.Context, chargeID uuid.UUID) (money.Money, money.Money, error) {
	if err := s.fail("OverpaymentCreditBase"); err != nil {
		return money.Money{}, money.Money{}, err
	}
	used := brl(0)
	for _, r := range s.refunds {
		if r.ChargeID == chargeID && r.Status == RefundSucceeded {
			used, _ = used.Add(r.CreditAmount)
		}
	}
	for _, d := range s.disputes {
		if d.ChargeID == chargeID && d.Status == DisputeLost {
			used, _ = used.Add(amountOrZero(d.CreditAmount, brl(0)))
			used, _ = used.Add(amountOrZero(d.RecoveryAmount, brl(0)))
		}
	}
	over, ok := s.overpaid[chargeID]
	if !ok {
		over = brl(0)
	}
	// Como na consulta real: o excedente é o lançamento payment.overpayment
	// dos pagamentos da cobrança (s.overpaid acrescenta um valor fixo), em todo
	// o histórico do ledger.
	for _, p := range s.payments {
		if p.ChargeID == nil || *p.ChargeID != chargeID {
			continue
		}
		for _, t := range s.ledger {
			if t.IdempotencyKey != "payment.overpayment:"+p.ID.String() {
				continue
			}
			for _, e := range t.Entries {
				if e.Direction == ledger.Credit {
					over, _ = over.Add(e.Amount)
				}
			}
		}
	}
	return over, used, nil
}

func (s *payStore) LockCustomerCredits(_ context.Context, _ uuid.UUID) error {
	if err := s.fail("LockCustomerCredits"); err != nil {
		return err
	}
	s.creditSteps = append(s.creditSteps, "lock")
	return nil
}

func (s *payStore) CustomerCreditBalance(_ context.Context, customerID uuid.UUID) (money.Money, error) {
	if err := s.fail("CustomerCreditBalance"); err != nil {
		return money.Money{}, err
	}
	s.creditSteps = append(s.creditSteps, "balance")
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

func (s *payStore) Dispute(_ context.Context, id uuid.UUID, forUpdate bool) (Dispute, error) {
	if err := s.fail("Dispute"); err != nil {
		return Dispute{}, err
	}
	d, ok := s.disputes[id]
	if !ok || d.TenantID != s.current {
		return Dispute{}, apperr.NotFound("dispute_not_found", "Disputa não encontrada.")
	}
	if forUpdate {
		s.disputeLocks = append(s.disputeLocks, id)
	}
	return d, nil
}

// InsertRecoverySettlement reproduz o banco: a chave do pedido é única, o
// gatilho acumula a baixa nos totais da disputa e a constraint recusa a soma
// acima do valor a recuperar.
func (s *payStore) InsertRecoverySettlement(_ context.Context, st RecoverySettlement) error {
	if err := s.fail("InsertRecoverySettlement"); err != nil {
		return err
	}
	for _, x := range s.settlements {
		if st.RequestKey != "" && x.RequestKey == st.RequestKey {
			return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outra baixa.")
		}
	}
	d := s.disputes[st.DisputeID]
	zero := money.Zero(st.Amount.Currency())
	var err error
	if st.Outcome == RecoveryReceived {
		d.RecoveryReceived, err = amountOrZero(d.RecoveryReceived, zero).Add(st.Amount)
	} else {
		d.RecoveryWrittenOff, err = amountOrZero(d.RecoveryWrittenOff, zero).Add(st.Amount)
	}
	if err != nil {
		return err
	}
	if left, err := d.RecoveryOutstanding(); err != nil || left.IsNegative() {
		return apperr.Conflict("recovery_exceeds_outstanding", "A baixa excede o saldo a recuperar da disputa.")
	}
	s.disputes[d.ID] = d
	s.settlements = append(s.settlements, st)
	return nil
}

func (s *payStore) RecoverySettlementByRequestKey(_ context.Context, key string) (RecoverySettlement, error) {
	if err := s.fail("RecoverySettlementByRequestKey"); err != nil {
		return RecoverySettlement{}, err
	}
	for _, x := range s.settlements {
		if x.RequestKey == key {
			return x, nil
		}
	}
	return RecoverySettlement{}, apperr.NotFound("recovery_settlement_not_found", "Baixa do valor a recuperar não encontrada.")
}

func (s *payStore) RecoverySettlements(_ context.Context, disputeID uuid.UUID) ([]RecoverySettlement, error) {
	if err := s.fail("RecoverySettlements"); err != nil {
		return nil, err
	}
	var out []RecoverySettlement
	for _, x := range s.settlements {
		if x.DisputeID == disputeID {
			out = append(out, x)
		}
	}
	return out, nil
}

func (s *payStore) ReverseLedger(_ context.Context, key, description string, at time.Time) error {
	if err := s.fail("ReverseLedger"); err != nil {
		return err
	}
	// Como o banco: o lançamento original é procurado em todo o histórico
	// (s.ledger, não só nos lançamentos observados em s.posted), e o estorno é
	// gravado nos dois, com a chave de idempotência descartando a repetição.
	for _, t := range s.ledger {
		if t.IdempotencyKey != key {
			continue
		}
		s.reversals = append(s.reversals, key)
		r := t.Reversal(uuid.New(), at, description)
		if slices.ContainsFunc(s.ledger, func(p ledger.Transaction) bool { return p.IdempotencyKey == r.IdempotencyKey }) {
			return nil
		}
		s.posted = append(s.posted, r)
		s.ledger = append(s.ledger, r)
		return nil
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

func (s *payStore) UpdateReconciliationRunLease(_ context.Context, r ReconciliationRun) error {
	if err := s.fail("UpdateReconciliationRunLease"); err != nil {
		return err
	}
	cur, ok := s.runs[r.ID]
	if !ok || cur.Status != RunRunning {
		return apperr.Conflict("reconciliation_run_interrupted", "A execução foi encerrada por outra instância.")
	}
	cur.LeaseUntil, cur.Attempts = r.LeaseUntil, r.Attempts
	s.runs[r.ID] = cur
	return nil
}

func (s *payStore) ClaimableReconciliationRuns(_ context.Context, now time.Time, limit int) ([]RunRef, error) {
	if s.claimableErr != nil {
		return nil, s.claimableErr
	}
	var out []RunRef
	for _, id := range s.runOrder {
		if r := s.runs[id]; r.Claimable(now) && len(out) < limit {
			out = append(out, RunRef{TenantID: r.TenantID, RunID: r.ID})
		}
	}
	return out, nil
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
	// Como na consulta real: as presentes na listagem do provedor primeiro,
	// depois as da janela, em ordem de criação, até o limite.
	var listed, window []Charge
	for _, id := range s.chargeOrder {
		c := s.charges[id]
		if c.ProviderAccountID != sc.AccountID {
			continue
		}
		switch {
		case (c.ProviderChargeID != "" && slices.Contains(sc.ProviderChargeIDs, c.ProviderChargeID)) || slices.Contains(sc.ReferenceIDs, c.ID):
			listed = append(listed, c)
		case in(c.CreatedAt) || in(c.UpdatedAt):
			window = append(window, c)
		}
	}
	out := append(listed, window...)
	if sc.Limit > 0 && len(out) > sc.Limit {
		out = out[:sc.Limit]
	}
	return out, nil
}

func (s *payStore) ChargesForRotation(_ context.Context, sc RotationScope) ([]Charge, error) {
	if err := s.fail("ChargesForRotation"); err != nil {
		return nil, err
	}
	s.rotationScopes = append(s.rotationScopes, sc)
	in := func(t time.Time) bool { return !t.Before(sc.WindowStart) && t.Before(sc.WindowEnd) }
	var out []Charge
	for _, id := range s.chargeOrder {
		c := s.charges[id]
		switch c.Status {
		case ChargeAwaitingPayment, ChargePaid, ChargePartiallyRefunded, ChargeDisputed:
		default:
			continue
		}
		if c.ProviderAccountID == sc.AccountID && c.ProviderChargeID != "" && !c.CreatedAt.Before(sc.Since) && !in(c.CreatedAt) && !in(c.UpdatedAt) {
			out = append(out, c)
		}
	}
	// Nunca verificadas primeiro, depois as verificadas há mais tempo.
	slices.SortStableFunc(out, func(a, b Charge) int {
		ta, oka := s.checkedAt[a.ID]
		tb, okb := s.checkedAt[b.ID]
		switch {
		case !oka && !okb:
			return 0
		case !oka:
			return -1
		case !okb:
			return 1
		}
		return ta.Compare(tb)
	})
	if sc.Limit > 0 && len(out) > sc.Limit {
		out = out[:sc.Limit]
	}
	return out, nil
}

func (s *payStore) MarkChargesReconciled(_ context.Context, ids []uuid.UUID, at time.Time) error {
	if err := s.fail("MarkChargesReconciled"); err != nil {
		return err
	}
	for _, id := range ids {
		s.checkedAt[id] = at
	}
	return nil
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
