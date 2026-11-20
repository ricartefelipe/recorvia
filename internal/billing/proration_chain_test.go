package billing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Uma prorrata sem fatura (redução convertida em crédito do cliente) parte
// da fatura de ciclo; cancelada essa fatura, a prorrata deixa de valer como
// base e a mudança seguinte não credita nada.
func TestChangeImmediateCreditlessProrationInvalidatedByVoidedCycleInvoice(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.pro, 1, true, nil)

	first := f.change(t, sub.ID, immediate(f.basic.plan.ID))
	if first.CreditGranted == nil || first.InvoiceID != nil || len(f.store.prorations) != 1 || f.store.prorations[0].InvoiceID != nil {
		t.Fatalf("a redução vira crédito do cliente sem fatura: %+v", first)
	}

	key := cycleFeeKey(sub.ID, sub.CurrentPeriod.Start)
	fee, ok := f.store.cycleFees[key]
	if !ok {
		t.Fatal("a assinatura deveria ter fatura de ciclo do período")
	}
	fee.InvoiceStatus = Void
	f.store.cycleFees[key] = fee

	f.clock.Set(local(2026, time.November, 26, 9, 0))
	res := f.change(t, sub.ID, immediate(f.pro.plan.ID))
	if len(f.store.prorations) != 2 {
		t.Fatalf("a segunda prorrata é registrada: %+v", f.store.prorations)
	}
	rec := f.store.prorations[1]
	if rec.Credit.Amount() != 0 {
		t.Fatalf("sem base válida não há crédito: %+v", rec)
	}
	if res.InvoiceID == nil || res.Proration == nil || res.Proration.Net.Amount() != rec.Charge.Amount() {
		t.Fatalf("a cobrança proporcional do novo plano é faturada integralmente: %+v", res)
	}
	audit := f.store.audits[len(f.store.audits)-1]
	if audit.Details["proration_credit_basis"] != creditBasisNone || audit.Details["proration_voided_invoice_id"] != fee.InvoiceID {
		t.Fatalf("a auditoria registra a ausência de base e a fatura cancelada: %+v", audit.Details)
	}
}

// A prorrata sem fatura continua valendo enquanto a fatura de ciclo de que
// partiu vale: a segunda mudança credita sobre o componente da primeira.
func TestChangeImmediateCreditlessProrationKeepsValidBase(t *testing.T) {
	f := newChangeFixture(t)
	sub := f.addSub(t, f.pro, 1, true, nil)
	f.change(t, sub.ID, immediate(f.basic.plan.ID))

	f.clock.Set(local(2026, time.November, 26, 9, 0))
	f.change(t, sub.ID, immediate(f.pro.plan.ID))
	audit := f.store.audits[len(f.store.audits)-1]
	if audit.Details["proration_credit_basis"] != creditBasisProration {
		t.Fatalf("a base é a prorrata anterior: %+v", audit.Details)
	}
	if rec := f.store.prorations[1]; rec.Credit.Amount() == 0 || rec.CreditBase.Cmp(f.store.prorations[0].ChargeBase) != 0 {
		t.Fatalf("o crédito parte do componente cobrado na primeira mudança: %+v", rec)
	}
}

// Uma mudança agendada cuja data efetiva já passou está em vigor e aguarda o
// motor; uma nova mudança é recusada até o processamento do ciclo.
func TestChangeRejectsReplacingScheduledChangeInEffect(t *testing.T) {
	cases := []struct {
		name        string
		effectiveIn time.Duration
		wantErr     bool
	}{
		{"data efetiva no passado", -time.Hour, true},
		{"data efetiva agora", 0, true},
		{"data efetiva futura é substituída", time.Hour, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newChangeFixture(t)
			sub := f.addSub(t, f.basic, 1, true, nil)
			pending := &subscriptions.ScheduledChange{ID: uuid.New(), TenantID: f.tenantID, SubscriptionID: sub.ID, Kind: subscriptions.PlanChange,
				NewPlanID: &f.pro.plan.ID, NewPriceVersionID: &f.pro.price.ID, EffectiveAt: f.clock.Now().Add(c.effectiveIn),
				Status: subscriptions.ChangePending, CreatedAt: f.clock.Now()}
			f.store.changes[sub.ID] = pending

			_, err := f.svc.Change(context.Background(), f.tenantID, f.actor, &f.userID, sub.ID, nextCycle(f.free.plan.ID), 0)
			if !c.wantErr {
				if err != nil {
					t.Fatalf("mudança futura deveria ser substituída: %v", err)
				}
				if f.store.changes[sub.ID].Status != subscriptions.ChangePending || f.store.changes[sub.ID].ID == pending.ID {
					t.Fatalf("a nova mudança fica pendente no lugar da anterior: %+v", f.store.changes[sub.ID])
				}
				return
			}
			wantAppErr(t, err, apperr.KindConflict, "scheduled_change_in_effect")
			if pending.Status != subscriptions.ChangePending || len(f.store.canceledChanges) != 0 {
				t.Fatalf("a mudança em vigor não pode ser cancelada: %+v", pending)
			}
		})
	}
}
