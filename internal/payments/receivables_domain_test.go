package payments

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

func TestValidateRefundInput(t *testing.T) {
	ok := RefundInput{ChargeID: uuid.New(), Amount: brl(100), Reason: "  Cliente desistiu  "}
	got, err := validateRefundInput(ok)
	if err != nil || got.Reason != "Cliente desistiu" {
		t.Fatalf("pedido válido: %v %q", err, got.Reason)
	}
	cases := map[string]RefundInput{
		"valor zero":        {Amount: brl(0), Reason: "motivo"},
		"valor negativo":    {Amount: brl(-1), Reason: "motivo"},
		"acima do limite":   {Amount: brl(money.MaxAmount + 1), Reason: "motivo"},
		"motivo curto":      {Amount: brl(1), Reason: "ab"},
		"motivo longo":      {Amount: brl(1), Reason: strings.Repeat("a", 501)},
		"motivo com NUL":    {Amount: brl(1), Reason: "motivo\x00x"},
		"chave muito longa": {Amount: brl(1), Reason: "motivo", RequestKey: strings.Repeat("k", 301)},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := validateRefundInput(in)
			wantAppErr(t, err, apperr.KindInvalid, "invalid_refund")
		})
	}
}

func TestChargeRefundReservation(t *testing.T) {
	t.Run("reserva, confirma e libera", func(t *testing.T) {
		c := paidCharge(t, 10000)
		if err := c.ReserveRefund(brl(6000), chargeNow); err != nil {
			t.Fatal(err)
		}
		if r, _ := c.Refundable(); r.Amount() != 4000 || c.AmountRefundPending.Amount() != 6000 {
			t.Fatalf("saldo após reserva: %v pendente %v", r, c.AmountRefundPending)
		}
		wantAppErr(t, c.ReserveRefund(brl(4001), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
		if err := c.ReserveRefund(brl(4000), chargeNow); err != nil {
			t.Fatal(err)
		}
		if err := c.ConfirmRefund(brl(6000), true, chargeNow); err != nil || c.Status != ChargePartiallyRefunded {
			t.Fatalf("confirmação parcial: %v %s", err, c.Status)
		}
		if err := c.ReleaseRefund(brl(4000), chargeNow); err != nil || !c.AmountRefundPending.IsZero() {
			t.Fatalf("liberação: %v %v", err, c.AmountRefundPending)
		}
		wantAppErr(t, c.ReleaseRefund(brl(1), chargeNow), apperr.KindConflict, "refund_reservation_mismatch")
		if err := c.ConfirmRefund(brl(4000), false, chargeNow); err != nil || c.Status != ChargeRefunded || c.AmountRefunded.Amount() != 10000 {
			t.Fatalf("reembolso integral: %v %s %v", err, c.Status, c.AmountRefunded)
		}
		wantAppErr(t, c.ReserveRefund(brl(1), chargeNow), apperr.KindConflict, "charge_not_refundable")
	})
	t.Run("cobrança não paga não aceita pedido", func(t *testing.T) {
		c := newTestCharge(t, 10000)
		wantAppErr(t, c.ReserveRefund(brl(1), chargeNow), apperr.KindConflict, "charge_not_refundable")
		wantAppErr(t, c.ConfirmRefund(brl(1), false, chargeNow), apperr.KindConflict, "refund_exceeds_balance")
	})
	t.Run("valor inválido", func(t *testing.T) {
		c := paidCharge(t, 10000)
		wantAppErr(t, c.ReserveRefund(brl(0), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
		wantAppErr(t, c.ReserveRefund(money.New(1, money.Currency{}), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
		if err := c.ReleaseRefund(money.New(1, money.Currency{}), chargeNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("moedas diferentes: %v", err)
		}
		c.AmountRefunded = money.New(1, money.Currency{})
		if err := c.ReserveRefund(brl(1), chargeNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("saldo inválido: %v", err)
		}
		if err := c.ConfirmRefund(brl(1), false, chargeNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("saldo inválido na confirmação: %v", err)
		}
	})
	t.Run("confirmação sem reserva suficiente", func(t *testing.T) {
		c := paidCharge(t, 10000)
		wantAppErr(t, c.ConfirmRefund(brl(1), true, chargeNow), apperr.KindConflict, "refund_reservation_mismatch")
		_ = c.ReserveRefund(brl(9000), chargeNow)
		wantAppErr(t, c.ConfirmRefund(brl(2000), false, chargeNow), apperr.KindConflict, "refund_exceeds_balance")
	})
	t.Run("em disputa mantém a situação", func(t *testing.T) {
		c := paidCharge(t, 10000)
		_ = c.ReserveRefund(brl(3000), chargeNow)
		_ = c.OpenDispute(chargeNow)
		if err := c.ConfirmRefund(brl(3000), true, chargeNow); err != nil || c.Status != ChargeDisputed || c.AmountRefunded.Amount() != 3000 {
			t.Fatalf("reembolso durante a disputa: %v %s %v", err, c.Status, c.AmountRefunded)
		}
	})
	t.Run("reserva em cobrança sem moeda definida", func(t *testing.T) {
		c := paidCharge(t, 10000)
		c.AmountRefundPending = money.Money{}
		if r, err := c.Refundable(); err != nil || r.Amount() != 10000 {
			t.Fatalf("reserva vazia: %v %v", r, err)
		}
		if err := c.ReserveRefund(brl(10), chargeNow); err != nil || c.AmountRefundPending.Amount() != 10 {
			t.Fatalf("primeira reserva: %v %v", err, c.AmountRefundPending)
		}
	})
	t.Run("estorno limitado ao saldo não reembolsado", func(t *testing.T) {
		c := paidCharge(t, 10000)
		_ = c.ApplyRefund(brl(4000), chargeNow)
		_ = c.OpenDispute(chargeNow)
		if err := c.CloseDispute(false, brl(10000), chargeNow); err != nil || c.AmountChargedBack.Amount() != 6000 {
			t.Fatalf("estorno: %v %v", err, c.AmountChargedBack)
		}
	})
}

func TestRecordProviderStatusKeepsLatestInstant(t *testing.T) {
	c := newTestCharge(t, 100)
	late, early := chargeNow, chargeNow.Add(-time.Hour)
	c.RecordProviderStatus("RECEIVED", &late)
	c.RecordProviderStatus("PENDING", &early)
	if c.ProviderStatus != "PENDING" || !c.ProviderStatusAt.Equal(late) {
		t.Fatalf("o instante mais recente é preservado: %s %v", c.ProviderStatus, c.ProviderStatusAt)
	}
}

// FuzzRefundReservationsNeverExceedPaid: qualquer sequência de reservas,
// confirmações, liberações e chargebacks mantém reembolsado + reservado <=
// pago e reembolsado + estornado <= pago (as constraints do banco).
func FuzzRefundReservationsNeverExceedPaid(f *testing.F) {
	f.Add(int64(10000), []byte{0, 50, 0, 60, 1, 50, 2, 10, 3, 0, 4, 0})
	f.Add(int64(7), []byte{0, 255, 1, 255, 0, 1, 3, 0, 5, 0})
	f.Fuzz(func(t *testing.T, rawPaid int64, script []byte) {
		paid := 1 + int64(uint64(rawPaid)%uint64(money.MaxAmount))
		c := paidCharge(t, paid)
		for i := 0; i+1 < len(script) && i < 80; i += 2 {
			amount := brl(int64(script[i+1]) * paid / 100)
			switch script[i] % 6 {
			case 0:
				_ = c.ReserveRefund(amount, chargeNow)
			case 1:
				_ = c.ConfirmRefund(amount, true, chargeNow)
			case 2:
				_ = c.ReleaseRefund(amount, chargeNow)
			case 3:
				_ = c.OpenDispute(chargeNow)
			case 4:
				_ = c.CloseDispute(false, amount, chargeNow)
			case 5:
				_ = c.ConfirmRefund(amount, false, chargeNow)
			}
			p, r, pend, cb := c.AmountPaid.Amount(), c.AmountRefunded.Amount(), c.AmountRefundPending.Amount(), c.AmountChargedBack.Amount()
			if pend < 0 || r+pend > p || r+cb > p {
				t.Fatalf("pago=%d reembolsado=%d reservado=%d estornado=%d", p, r, pend, cb)
			}
		}
	})
}

func TestDisputeTransitions(t *testing.T) {
	opened := chargeNow
	d := Dispute{ID: uuid.New(), Status: DisputeOpened, OpenedAt: opened}
	if !d.Open() {
		t.Fatal("aberta")
	}
	ev, err := d.transition(DisputeUnderReview, "CHARGEBACK_DISPUTE", opened.Add(time.Hour), chargeNow)
	if err != nil || *ev.From != DisputeOpened || ev.To != DisputeUnderReview || d.ClosedAt != nil {
		t.Fatalf("análise: %+v %v", ev, err)
	}
	if _, err := d.transition(DisputeOpened, "", opened, chargeNow); err == nil {
		t.Fatal("não volta a aberta")
	}
	if _, err := d.transition(DisputeWon, "won", opened.Add(-time.Hour), chargeNow); err != nil || !d.ClosedAt.Equal(opened) || d.Open() {
		t.Fatalf("encerramento nunca antes da abertura: %v %v", err, d.ClosedAt)
	}
	wantAppErr(t, func() error { _, err := d.transition(DisputeLost, "", chargeNow, chargeNow); return err }(), apperr.KindConflict,
		"invalid_dispute_transition")
	for ps, want := range map[ProviderStatus]DisputeStatus{ProviderChargebackRequested: DisputeOpened, ProviderChargebackDispute: DisputeUnderReview,
		ProviderChargebackReversed: DisputeWon, ProviderChargebackLost: DisputeLost} {
		if got, ok := disputeTarget(ps); !ok || got != want {
			t.Fatalf("%s: %s", ps, got)
		}
	}
	if _, ok := disputeTarget(ProviderReceived); ok {
		t.Fatal("recebida não é chargeback")
	}
}

func localRefund(amount int64, status RefundStatus, created time.Time) Refund {
	return Refund{ID: uuid.New(), Status: status, Amount: brl(amount), Origin: RefundOriginRecorvia, CreatedAt: created}
}

func matchIDs(ms []refundMatch) []uuid.UUID {
	out := make([]uuid.UUID, len(ms))
	for i, m := range ms {
		out[i] = m.refundID
	}
	return out
}

func TestPlanRefundSync(t *testing.T) {
	t0 := chargeNow
	t.Run("associação exata por referência", func(t *testing.T) {
		done, failed, pending := localRefund(100, RefundRequested, t0), localRefund(200, RefundUnknown, t0), localRefund(300, RefundRequested, t0)
		finished := localRefund(50, RefundSucceeded, t0)
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{
			{ProviderRefundID: "rf_1", ExternalReference: done.ID.String(), State: RefundStateDone, Amount: brl(100)},
			{ProviderRefundID: "rf_2", ExternalReference: failed.ID.String(), State: RefundStateFailed, Amount: brl(200)},
			{ProviderRefundID: "rf_3", ExternalReference: pending.ID.String(), State: RefundStatePending, Amount: brl(300)},
			{ProviderRefundID: "rf_4", ExternalReference: finished.ID.String(), State: RefundStateDone, Amount: brl(50)},
			{ProviderRefundID: "rf_5", State: RefundStateDone, Amount: brl(0)},
		}}
		plan := planRefundSync(pc, []Refund{done, failed, pending, finished}, nil)
		if len(plan.confirm) != 1 || plan.confirm[0].refundID != done.ID || plan.confirm[0].providerRefundID != "rf_1" {
			t.Fatalf("confirmação: %+v", plan.confirm)
		}
		if !slices.Equal(matchIDs(plan.fail), []uuid.UUID{failed.ID}) || !plan.pending[pending.ID] || len(plan.external) != 0 {
			t.Fatalf("plano: %+v", plan)
		}
		if !plan.covers(done.ID) || !plan.covers(failed.ID) || !plan.covers(pending.ID) || plan.covers(finished.ID) || plan.empty() {
			t.Fatal("cobertura")
		}
	})
	t.Run("reembolso do provedor já registrado e novo identificado", func(t *testing.T) {
		known := localRefund(100, RefundSucceeded, t0)
		known.Origin, known.ProviderRefundID = RefundOriginProvider, "rf_9"
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{
			{ProviderRefundID: "rf_9", State: RefundStateDone, Amount: brl(100)},
			{ProviderRefundID: "rf_10", State: RefundStateDone, Amount: brl(40)},
			{ProviderRefundID: "rf_11", State: RefundStatePending, Amount: brl(40)},
			{ExternalReference: "outro-sistema", State: RefundStateDone, Amount: brl(5)},
		}}
		plan := planRefundSync(pc, []Refund{known}, nil)
		if len(plan.confirm) != 0 || len(plan.external) != 2 || plan.external[0].ProviderRefundID != "rf_10" || plan.external[1].Amount.Amount() != 5 {
			t.Fatalf("plano: %+v", plan)
		}
	})
	t.Run("atribuição por valor sem identificação", func(t *testing.T) {
		old := localRefund(100, RefundSucceeded, t0)
		a, b := localRefund(200, RefundRequested, t0.Add(time.Minute)), localRefund(300, RefundUnknown, t0.Add(2*time.Minute))
		notSent := localRefund(50, RefundRequested, t0.Add(3*time.Minute))
		dispatched := map[uuid.UUID]bool{a.ID: true, b.ID: true}
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{{State: RefundStateDone, Amount: brl(100)},
			{State: RefundStateDone, Amount: brl(200)}, {State: RefundStatePending, Amount: brl(300)}, {State: RefundStateFailed, Amount: brl(70)}}}
		plan := planRefundSync(pc, []Refund{b, old, a, notSent}, dispatched)
		if !slices.Equal(matchIDs(plan.confirm), []uuid.UUID{a.ID}) || !plan.pending[b.ID] || plan.pending[notSent.ID] || len(plan.external) != 0 {
			t.Fatalf("plano: %+v", plan)
		}
	})
	t.Run("saldo sem reembolso local aberto é do provedor", func(t *testing.T) {
		a := localRefund(200, RefundRequested, t0)
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{{State: RefundStateDone, Amount: brl(450)}}}
		plan := planRefundSync(pc, []Refund{a}, map[uuid.UUID]bool{a.ID: true})
		if !slices.Equal(matchIDs(plan.confirm), []uuid.UUID{a.ID}) || len(plan.external) != 1 || plan.external[0].Amount.Amount() != 250 {
			t.Fatalf("plano: %+v", plan)
		}
	})
	t.Run("valor que não cobre o reembolso aberto não é atribuído", func(t *testing.T) {
		a := localRefund(200, RefundRequested, t0)
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{{State: RefundStateDone, Amount: brl(150)}}}
		plan := planRefundSync(pc, []Refund{a}, map[uuid.UUID]bool{a.ID: true})
		if !plan.empty() || plan.covers(a.ID) {
			t.Fatalf("plano: %+v", plan)
		}
	})
	t.Run("total sem lista e cobrança reembolsada sem valor", func(t *testing.T) {
		a := localRefund(1000, RefundRequested, t0)
		d := map[uuid.UUID]bool{a.ID: true}
		if p := planRefundSync(ProviderCharge{Amount: brl(1000), RefundedAmount: brl(1000)}, []Refund{a}, d); !p.covers(a.ID) {
			t.Fatal("total reembolsado")
		}
		if p := planRefundSync(ProviderCharge{Amount: brl(1000), Status: ProviderRefunded}, []Refund{a}, d); !p.covers(a.ID) {
			t.Fatal("reembolsada sem valor informado é integral")
		}
		if p := planRefundSync(ProviderCharge{Amount: brl(1000), Status: ProviderReceived}, []Refund{a}, d); !p.empty() {
			t.Fatal("sem reembolso no provedor")
		}
	})
	t.Run("moedas inconsistentes não geram efeito", func(t *testing.T) {
		a := localRefund(100, RefundRequested, t0)
		d := map[uuid.UUID]bool{a.ID: true}
		pc := ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{{State: RefundStateDone, Amount: money.New(100, money.Currency{})}}}
		if p := planRefundSync(pc, []Refund{a}, d); !p.empty() {
			t.Fatalf("plano: %+v", p)
		}
		bad := localRefund(100, RefundSucceeded, t0)
		bad.Amount = money.New(100, money.Currency{})
		pc = ProviderCharge{Amount: brl(1000), Refunds: []ProviderRefund{{State: RefundStateDone, Amount: brl(100)}}}
		if p := planRefundSync(pc, []Refund{bad, a}, d); !p.empty() {
			t.Fatalf("plano com reembolso inválido: %+v", p)
		}
	})
}

func paidLocal(t *testing.T, amount, fee int64) LocalCharge {
	t.Helper()
	c := paidCharge(t, amount)
	c.Fee = brl(fee)
	return LocalCharge{Charge: c}
}

func provPaid(c Charge, amount, fee int64) ProviderCharge {
	f := brl(fee)
	return ProviderCharge{ProviderChargeID: c.ProviderChargeID, ExternalReference: c.ID.String(), Status: ProviderReceived, RawStatus: "paid",
		Amount: brl(amount), Fee: &f, RefundedAmount: brl(0)}
}

func kinds(fs []Finding) []DiscrepancyKind {
	out := make([]DiscrepancyKind, len(fs))
	for i, f := range fs {
		out[i] = f.Kind
	}
	return out
}

func TestCompare(t *testing.T) {
	now := chargeNow.Add(3 * time.Hour)
	t.Run("cobranças consistentes", func(t *testing.T) {
		l := paidLocal(t, 10000, 199)
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{provPaid(l.Charge, 10000, 199)}, Local: []LocalCharge{l}, Now: now})
		if len(res.Findings) != 0 || res.Checked() != 1 || !slices.Equal(res.CheckedCharges, []uuid.UUID{l.Charge.ID}) {
			t.Fatalf("resultado: %+v", res)
		}
	})
	t.Run("valor e taxa divergentes", func(t *testing.T) {
		l := paidLocal(t, 10000, 199)
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{provPaid(l.Charge, 9900, 250)}, Local: []LocalCharge{l}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyAmountMismatch, DiscrepancyFeeMismatch}) {
			t.Fatalf("divergências: %v", kinds(res.Findings))
		}
		a, f := res.Findings[0], res.Findings[1]
		if a.Local["basis"] != "paid" || a.LocalAmount.Amount() != 10000 || a.ProviderAmount.Amount() != 9900 || f.ProviderAmount.Amount() != 250 {
			t.Fatalf("valores: %+v %+v", a, f)
		}
		if !strings.Contains(a.DedupeKey, ":10000:9900") || f.DedupeKey != "fee_mismatch:"+l.Charge.ID.String()+":199:250" {
			t.Fatalf("chaves: %s %s", a.DedupeKey, f.DedupeKey)
		}
	})
	t.Run("taxa não informada não é comparada", func(t *testing.T) {
		l := paidLocal(t, 10000, 199)
		p := provPaid(l.Charge, 10000, 0)
		p.Fee = nil
		if res := Compare(CompareInput{ProviderCharges: []ProviderCharge{p}, Local: []LocalCharge{l}, Now: now}); len(res.Findings) != 0 {
			t.Fatalf("divergências: %v", kinds(res.Findings))
		}
	})
	t.Run("pagamento não refletido nos dois sentidos", func(t *testing.T) {
		open := LocalCharge{Charge: newTestCharge(t, 10000)}
		_ = open.Charge.MarkCreated("pay_open", "", chargeNow)
		paid := paidLocal(t, 5000, 0)
		paid.Charge.ProviderChargeID = "pay_paid"
		pending := ProviderCharge{ProviderChargeID: "pay_paid", Status: ProviderPending, RawStatus: "pending", Amount: brl(5000)}
		unknown := ProviderCharge{ProviderChargeID: "pay_x", Status: ProviderUnknownStatus, Amount: brl(5000)}
		weird := paidLocal(t, 5000, 0)
		weird.Charge.ProviderChargeID = "pay_x"
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{provPaid(open.Charge, 10000, 199), pending, unknown},
			Local: []LocalCharge{open, paid, weird}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyPaymentNotReflected, DiscrepancyPaymentNotReflected}) {
			t.Fatalf("divergências: %v", kinds(res.Findings))
		}
		dirs := []any{res.Findings[0].Local["direction"], res.Findings[1].Local["direction"]}
		if !slices.Contains(dirs, "provider_paid") || !slices.Contains(dirs, "local_paid") {
			t.Fatalf("sentidos: %v", dirs)
		}
	})
	t.Run("cobranças em aberto com valor divergente", func(t *testing.T) {
		l := LocalCharge{Charge: newTestCharge(t, 10000)}
		_ = l.Charge.MarkCreated("pay_9", "", chargeNow)
		p := ProviderCharge{ProviderChargeID: "pay_9", Status: ProviderOverdue, Amount: brl(9000)}
		failed := LocalCharge{Charge: newTestCharge(t, 10000)}
		_ = failed.Charge.MarkCreated("pay_10", "", chargeNow)
		_ = failed.Charge.MarkFailed("recusa", chargeNow)
		pf := ProviderCharge{ProviderChargeID: "pay_10", Status: ProviderPending, Amount: brl(1)}
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{p, pf}, Local: []LocalCharge{l, failed}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyAmountMismatch}) || res.Findings[0].Local["basis"] != "charge" {
			t.Fatalf("divergências: %+v", res.Findings)
		}
	})
	t.Run("cobrança sem correspondência nos dois lados", func(t *testing.T) {
		gone := paidLocal(t, 1000, 0)
		gone.Charge.ProviderChargeID = "pay_gone"
		notLooked := paidLocal(t, 1000, 0)
		notLooked.Charge.ProviderChargeID = "pay_skip"
		foreign := ProviderCharge{ProviderChargeID: "pay_foreign", ExternalReference: "pedido-123", Status: ProviderReceived, Amount: brl(700)}
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{foreign, foreign, {ProviderChargeID: ""}}, Local: []LocalCharge{gone, notLooked},
			MissingAtProvider: map[string]bool{"pay_gone": true}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyMissingLocal, DiscrepancyMissingProvider}) {
			t.Fatalf("divergências: %v", kinds(res.Findings))
		}
		if res.Checked() != 2 || !slices.Equal(res.CheckedProviderIDs, []string{"pay_foreign"}) {
			t.Fatalf("verificadas: %d %v", res.Checked(), res.CheckedProviderIDs)
		}
	})
	t.Run("correspondência pela referência externa", func(t *testing.T) {
		l := LocalCharge{Charge: newTestCharge(t, 10000)}
		p := provPaid(l.Charge, 10000, 0)
		p.ProviderChargeID = "pay_late"
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{p, {ProviderChargeID: "pay_dup", ExternalReference: l.Charge.ID.String(),
			Status: ProviderPending, Amount: brl(1)}}, Local: []LocalCharge{l}, Now: now})
		// A primeira cobrança do provedor com a referência é associada; a
		// segunda (duplicada) é apontada como sem correspondência local.
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyMissingLocal, DiscrepancyPaymentNotReflected}) ||
			res.Findings[0].ProviderChargeID != "pay_dup" || res.Findings[0].Local["referenced_charge_id"] != l.Charge.ID.String() ||
			res.Findings[1].ProviderChargeID != "pay_late" {
			t.Fatalf("divergências: %+v", res.Findings)
		}
	})
	t.Run("reembolsos", func(t *testing.T) {
		l := paidLocal(t, 10000, 0)
		_ = l.Charge.ApplyRefund(brl(1000), chargeNow)
		stale := localRefund(2000, RefundRequested, now.Add(-2*time.Hour))
		fresh := localRefund(500, RefundUnknown, now.Add(-time.Minute))
		l.Refunds = []Refund{stale, fresh, localRefund(1000, RefundSucceeded, chargeNow)}
		p := provPaid(l.Charge, 10000, 0)
		p.Status, p.Refunds = ProviderPartiallyRefunded, []ProviderRefund{{State: RefundStateDone, Amount: brl(3500)}}
		res := Compare(CompareInput{ProviderCharges: []ProviderCharge{p}, Local: []LocalCharge{l}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyRefundPending}) || *res.Findings[0].RefundID != stale.ID {
			t.Fatalf("diferença explicada por reembolsos em andamento: %+v", res.Findings)
		}
		p.Refunds = []ProviderRefund{{State: RefundStateDone, Amount: brl(9000)}}
		res = Compare(CompareInput{ProviderCharges: []ProviderCharge{p}, Local: []LocalCharge{l}, Now: now})
		if len(res.Findings) != 2 || res.Findings[0].DedupeKey != "refund_pending:charge:"+l.Charge.ID.String()+":1000:9000" {
			t.Fatalf("diferença não explicada: %+v", res.Findings)
		}
		l.Refunds = nil
		p.Refunds = nil
		res = Compare(CompareInput{ProviderCharges: []ProviderCharge{p}, Local: []LocalCharge{l}, Now: now})
		if !slices.Equal(kinds(res.Findings), []DiscrepancyKind{DiscrepancyRefundPending}) {
			t.Fatalf("reembolso local sem registro no provedor: %+v", res.Findings)
		}
	})
	t.Run("chargebacks", func(t *testing.T) {
		paid := paidLocal(t, 10000, 0)
		disputed := paidLocal(t, 10000, 0)
		_ = disputed.Charge.OpenDispute(chargeNow)
		chargedBack := paidLocal(t, 10000, 0)
		_ = chargedBack.Charge.OpenDispute(chargeNow)
		_ = chargedBack.Charge.CloseDispute(false, brl(10000), chargeNow)
		cases := []struct {
			name   string
			local  LocalCharge
			status ProviderStatus
			want   bool
		}{
			{"aberto no provedor", paid, ProviderChargebackRequested, true},
			{"em análise já registrado", disputed, ProviderChargebackDispute, false},
			{"perdido não registrado", disputed, ProviderChargebackLost, true},
			{"perdido registrado", chargedBack, ProviderChargebackLost, false},
			{"ganho com disputa local aberta", disputed, ProviderChargebackReversed, true},
			{"ganho já registrado", paid, ProviderChargebackReversed, false},
			{"provedor sem chargeback", disputed, ProviderReceived, true},
			{"estornada sem chargeback no provedor", chargedBack, ProviderReceived, true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				p := provPaid(c.local.Charge, 10000, 0)
				p.Status = c.status
				res := Compare(CompareInput{ProviderCharges: []ProviderCharge{p}, Local: []LocalCharge{c.local}, Now: now})
				got := slices.Contains(kinds(res.Findings), DiscrepancyChargebackPending)
				if got != c.want {
					t.Fatalf("divergência de chargeback: %v, esperado %v (%v)", got, c.want, kinds(res.Findings))
				}
			})
		}
	})
	t.Run("operações com resultado desconhecido", func(t *testing.T) {
		rid := uuid.New()
		ops := []Operation{{ID: uuid.New(), ChargeID: uuid.New(), RefundID: &rid, Kind: OpRefund, Status: OpUnknown, Attempts: 6},
			{ID: uuid.New(), Status: OpPending}}
		res := Compare(CompareInput{UnknownOps: ops, Now: now})
		if len(res.Findings) != 1 || *res.Findings[0].RefundID != rid || res.Findings[0].Local["kind"] != "refund" || res.Checked() != 0 {
			t.Fatalf("divergências: %+v", res.Findings)
		}
	})
}

func TestResolutionRules(t *testing.T) {
	for _, k := range DiscrepancyKinds {
		if !resolutionApplies(k, ResolutionDismiss) || !resolutionApplies(k, ResolutionKeepLocal) || resolutionApplies(k, "other") {
			t.Fatalf("%s: dismiss e keep_local valem sempre", k)
		}
		wantAdjust := k == DiscrepancyAmountMismatch || k == DiscrepancyFeeMismatch
		if resolutionApplies(k, ResolutionAdjustLedger) != wantAdjust || resolutionApplies(k, ResolutionAcceptProvider) == (k == DiscrepancyMissingLocal) {
			t.Fatalf("%s: regras de ajuste e de aceite", k)
		}
	}
	in, err := validateResolve(ResolveInput{Resolution: ResolutionKeepLocal, Justification: "  conferido no extrato  "})
	if err != nil || in.Justification != "conferido no extrato" {
		t.Fatalf("válida: %v %q", err, in.Justification)
	}
	for name, in := range map[string]ResolveInput{
		"ação desconhecida":     {Resolution: ResolutionAutoResolved, Justification: "conferido no extrato"},
		"justificativa curta":   {Resolution: ResolutionDismiss, Justification: "curta"},
		"justificativa longa":   {Resolution: ResolutionDismiss, Justification: strings.Repeat("a", 2001)},
		"justificativa com NUL": {Resolution: ResolutionDismiss, Justification: "conferido\x00extrato"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateResolve(in)
			wantAppErr(t, err, apperr.KindInvalid, "invalid_resolution")
		})
	}
}

// Recebimento perdido só é apontado com o provedor recebido e valor ainda em
// pagamentos confirmados.
func TestCompareMissedReceipt(t *testing.T) {
	c := Charge{ID: uuid.New(), ProviderChargeID: "pay_r", Status: ChargePaid, Amount: brl(10000), AmountPaid: brl(10000), Fee: brl(199),
		AmountRefunded: brl(0), AmountChargedBack: brl(0), AmountRefundPending: brl(0)}
	received := provPaid(c, 10000, 199)
	confirmed := received
	confirmed.Status = ProviderConfirmed
	cases := []struct {
		name    string
		pending money.Money
		p       ProviderCharge
		want    bool
	}{
		{"recebido no provedor e pendente aqui", brl(10000), received, true},
		{"recebido nos dois lados", money.Money{}, received, false},
		{"pendente aqui e só confirmado no provedor", brl(10000), confirmed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := compareCharge(LocalCharge{Charge: c, PendingReceipt: tc.pending}, tc.p, time.Now())
			found := slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Kind == DiscrepancyPaymentNotReflected && f.DedupeKey == "payment_not_reflected:"+c.ID.String()+":provider_received" &&
					f.Local["direction"] == DirectionProviderReceived && f.LocalAmount.IsZero() && f.ProviderAmount.Amount() == 10000
			})
			if found != tc.want || len(fs) != map[bool]int{true: 1, false: 0}[tc.want] {
				t.Fatalf("divergências: %+v", kinds(fs))
			}
		})
	}
}
