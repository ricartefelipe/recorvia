//go:build integration

package pgstore_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// countingGateway conta as criações de cobrança pedidas ao provedor.
type countingGateway struct {
	payments.Gateway
	creates *atomic.Int32
}

func (g countingGateway) CreateCharge(ctx context.Context, req payments.CreateChargeRequest) (payments.ProviderCharge, error) {
	g.creates.Add(1)
	return g.Gateway.CreateCharge(ctx, req)
}

func newCountingPayHarness(t *testing.T) (*payHarness, *atomic.Int32) {
	t.Helper()
	creates := &atomic.Int32{}
	h := newPayHarnessWith(t, 2*time.Second, func(gw payments.Gateway) payments.Gateway { return countingGateway{Gateway: gw, creates: creates} })
	return h, creates
}

// outboxEventID devolve o id do evento da outbox (a mensagem entregue aos
// consumidores) do tipo informado para a fatura.
func (h *payHarness) outboxEventID(t *testing.T, eventType string, invoiceID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := h.env.DB.InTenantTx(context.Background(), h.tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM outbox_events WHERE tenant_id = $1 AND event_type = $2 AND aggregate_id = $3
			ORDER BY occurred_at DESC, id DESC LIMIT 1`, h.tn.ID, eventType, invoiceID).Scan(&id)
	})
	if err != nil {
		t.Fatalf("evento %s da fatura: %v", eventType, err)
	}
	return id
}

func (h *payHarness) chargesOf(t *testing.T, invoiceID uuid.UUID) []payments.Charge {
	t.Helper()
	cs, err := h.env.Store.InvoiceCharges(context.Background(), h.tn.ID, invoiceID)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func (h *payHarness) inboxCount(t *testing.T, consumer string, msg uuid.UUID) int {
	t.Helper()
	return countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM inbox_messages WHERE tenant_id = $1 AND consumer = $2 AND message_id = $3`,
		h.tn.ID, consumer, msg)
}

// Regressão: o consumidor payments.invoice_issued gravava o inbox depois da
// cobrança, numa transação própria. Uma reentrega da mesma mensagem (ack
// perdido, republicação após o lease, reprocessamento da DLQ) depois de uma
// recusa chamava RequestCharge, que criava a tentativa 2 no provedor. Com
// inbox e cobrança na mesma transação, a reentrega não faz nada, depois de
// recusa ou de sucesso.
func TestInvoiceIssuedRedeliveryCreatesNoNewCharge(t *testing.T) {
	cases := []struct {
		name     string
		scenario string
		want     payments.ChargeStatus
	}{
		{"depois de recusa", "decline", payments.ChargeFailed},
		{"depois de sucesso", "", payments.ChargeAwaitingPayment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, creates := newCountingPayHarness(t)
			if tc.scenario != "" {
				if err := h.sim.EnqueueScenario(h.apiKey, tc.scenario); err != nil {
					t.Fatal(err)
				}
			}
			inv := h.issueInvoice(t, 4000)
			msg := h.outboxEventID(t, outbox.EventInvoiceIssued, inv.ID)
			if err := h.pay.HandleInvoiceIssued(ctx, h.tn.ID, inv.ID, msg); err != nil {
				t.Fatal(err)
			}
			first := h.chargesOf(t, inv.ID)
			if len(first) != 1 || first[0].Status != tc.want || creates.Load() != 1 || h.inboxCount(t, payments.ConsumerInvoiceIssued, msg) != 1 {
				t.Fatalf("primeira entrega: %+v, %d criações, inbox %d", first, creates.Load(), h.inboxCount(t, payments.ConsumerInvoiceIssued, msg))
			}
			for range 3 {
				if err := h.pay.HandleInvoiceIssued(ctx, h.tn.ID, inv.ID, msg); err != nil {
					t.Fatalf("reentrega: %v", err)
				}
			}
			got := h.chargesOf(t, inv.ID)
			if len(got) != 1 || got[0].ID != first[0].ID || got[0].Status != tc.want || creates.Load() != 1 {
				t.Fatalf("reentrega criou cobrança: %+v, %d criações no provedor", got, creates.Load())
			}
			if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1`, h.tn.ID); n != 1 {
				t.Fatalf("operações = %d após reentregas", n)
			}
		})
	}
}

// Regressão: o consumidor payments.invoice_credited cancelava a cobrança
// divergente, solicitava a nova pelo saldo e só então gravava o inbox, em
// transações separadas. Recusada a cobrança nova, a reentrega da mensagem
// solicitava outra (tentativa 3). Agora inbox, cancelamento local e a nova
// cobrança com sua operação são gravados juntos.
func TestInvoiceCreditedRedeliveryCreatesNoNewCharge(t *testing.T) {
	cases := []struct {
		name     string
		scenario string
		want     payments.ChargeStatus
	}{
		{"depois de recusa", "decline", payments.ChargeFailed},
		{"depois de sucesso", "", payments.ChargeAwaitingPayment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, creates := newCountingPayHarness(t)
			inv := h.issueInvoice(t, 10000)
			old, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
			if err != nil || old == nil || old.Status != payments.ChargeAwaitingPayment {
				t.Fatalf("cobrança original: %+v %v", old, err)
			}
			notes := billing.NewCreditNoteService(h.env.Store.Billing(), h.env.Clock)
			if _, err := notes.Issue(ctx, h.tn.ID, h.tn.Actor, &h.tn.Owner.ID, inv.ID, brlCents(3000), "Desconto comercial", ""); err != nil {
				t.Fatal(err)
			}
			if tc.scenario != "" {
				if err := h.sim.EnqueueScenario(h.apiKey, tc.scenario); err != nil {
					t.Fatal(err)
				}
			}
			msg := h.outboxEventID(t, outbox.EventInvoiceCredited, inv.ID)
			if err := h.pay.HandleInvoiceCredited(ctx, h.tn.ID, inv.ID, msg); err != nil {
				t.Fatal(err)
			}
			first := h.chargesOf(t, inv.ID)
			if len(first) != 2 || first[0].Status != payments.ChargeCanceled || first[1].Status != tc.want || first[1].Amount.Amount() != 7000 ||
				creates.Load() != 2 || h.inboxCount(t, payments.ConsumerInvoiceCredited, msg) != 1 {
				t.Fatalf("primeira entrega: %+v, %d criações", first, creates.Load())
			}
			ops := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1`, h.tn.ID)
			for range 3 {
				if err := h.pay.HandleInvoiceCredited(ctx, h.tn.ID, inv.ID, msg); err != nil {
					t.Fatalf("reentrega: %v", err)
				}
			}
			got := h.chargesOf(t, inv.ID)
			if len(got) != 2 || got[1].ID != first[1].ID || got[1].Status != tc.want || creates.Load() != 2 {
				t.Fatalf("reentrega criou cobrança: %+v, %d criações no provedor", got, creates.Load())
			}
			if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1`, h.tn.ID); n != ops {
				t.Fatalf("operações %d -> %d após reentregas", ops, n)
			}
		})
	}
}
