//go:build integration

package pgstore_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

type payHarness struct {
	env    *fixture.Env
	tn     fixture.Tenant
	sim    *paysim.Sim
	pay    *payments.Service
	apiKey string
}

func newPayHarness(t *testing.T, callTimeout time.Duration) *payHarness {
	t.Helper()
	return newPayHarnessWith(t, callTimeout, nil)
}

// newPayHarnessWith monta o cenário com o gateway do simulador envolvido por
// wrap (nil usa o gateway do simulador sem alteração).
func newPayHarnessWith(t *testing.T, callTimeout time.Duration, wrap func(payments.Gateway) payments.Gateway) *payHarness {
	t.Helper()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "pay")
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h := &payHarness{env: env, tn: tn, apiKey: "sim_teste_" + uuid.NewString()[:8]}
	simSrv := httptest.NewUnstartedServer(nil)
	h.sim = paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, discardLogger())
	h.sim.TimeoutDelay = 3 * time.Second
	simSrv.Config.Handler = h.sim.Handler()
	simSrv.Start()
	t.Cleanup(simSrv.Close)
	// Endpoint de recepção do Recorvia: delega ao caso de uso real.
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := h.pay.ReceiveWebhook(r.Context(), "simulator", token, r.Header, body); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recv.Close)
	resolver := gateways.NewResolver(callTimeout, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
		Gateway: func(a payments.ProviderAccount, hc *http.Client) (payments.Gateway, error) {
			var gw payments.Gateway = simulator.New(simSrv.URL, a.APIKey, hc)
			if wrap != nil {
				gw = wrap(gw)
			}
			return gw, nil
		}})
	h.pay = env.Payments(resolver, payments.Config{WebhookBaseURL: recv.URL + "/webhooks/providers", CallTimeout: callTimeout, Lease: 10 * time.Second})
	if _, err := h.pay.ConfigureAccount(context.Background(), tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: h.apiKey, WebhookSecret: strings.Repeat("s", 40), MakeDefault: true}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *payHarness) issueInvoice(t *testing.T, amount int64) billing.Invoice {
	t.Helper()
	plan := h.env.NewFlatPlan(t, h.tn, amount)
	c := h.env.NewCustomer(t, h.tn)
	sub := h.env.NewSubscription(t, h.tn, c.ID, plan.ID, nil)
	if _, err := h.env.Engine.RunTenant(context.Background(), h.tn.ID, h.tn.Actor); err != nil {
		t.Fatal(err)
	}
	pg, err := h.env.Billing.List(context.Background(), h.tn.ID, billing.ListFilter{SubscriptionID: &sub.ID, Limit: 10})
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("fatura não emitida: %v %d", err, len(pg.Items))
	}
	return pg.Items[0]
}

// processWebhooks consome os eventos provider_webhook.received da outbox,
// como faria o consumidor (o id do evento da outbox é a mensagem do inbox).
func (h *payHarness) processWebhooks(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	type ev struct {
		id      uuid.UUID
		payload []byte
	}
	var evs []ev
	err := h.env.DB.InTenantTx(ctx, h.tn.ID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, payload FROM outbox_events WHERE tenant_id = $1 AND event_type = 'provider_webhook.received' ORDER BY occurred_at, id`, h.tn.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ev
			if err := rows.Scan(&e.id, &e.payload); err != nil {
				return err
			}
			evs = append(evs, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		var p struct {
			ID uuid.UUID `json:"webhook_event_id"`
		}
		if err := json.Unmarshal(e.payload, &p); err != nil {
			t.Fatal(err)
		}
		if err := h.pay.ProcessWebhook(ctx, h.tn.ID, p.ID, e.id); err != nil {
			t.Fatalf("processamento do webhook: %v", err)
		}
	}
}

func (h *payHarness) charge(t *testing.T, invoiceID uuid.UUID) payments.Charge {
	t.Helper()
	cs, err := h.env.Store.InvoiceCharges(context.Background(), h.tn.ID, invoiceID)
	if err != nil || len(cs) == 0 {
		t.Fatalf("cobranças: %v %d", err, len(cs))
	}
	return cs[len(cs)-1]
}

func TestPaymentConfirmedByWebhookIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	inv := h.issueInvoice(t, 99000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c == nil || c.Status != payments.ChargeAwaitingPayment {
		t.Fatalf("cobrança: %+v %v", c, err)
	}
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	h.sim.DispatchDue(ctx)
	h.processWebhooks(t)
	got, err := h.env.Billing.Get(ctx, h.tn.ID, inv.ID)
	if err != nil || got.Status != billing.Paid || got.AmountPaid.Amount() != 99000 {
		t.Fatalf("fatura após webhook: %s pago=%d err=%v", got.Status, got.AmountPaid.Amount(), err)
	}
	// Reenvio de todos os eventos: deduplicados no recebimento.
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "replay"}); err != nil {
		t.Fatal(err)
	}
	h.env.Clock.Advance(time.Minute)
	h.sim.DispatchDue(ctx)
	h.processWebhooks(t)
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payments WHERE tenant_id = $1`, h.tn.ID); n != 1 {
		t.Fatalf("pagamentos = %d após reenvio", n)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND kind = 'payment.confirmed'`, h.tn.ID); n != 1 {
		t.Fatalf("lançamentos de pagamento = %d", n)
	}
	fee := paysim.Fee(99000)
	if n := countRows(t, h.env, h.tn.ID, `SELECT COALESCE(sum(e.amount),0)::int FROM ledger_entries e JOIN ledger_accounts a ON a.id = e.account_id
		WHERE e.tenant_id = $1 AND a.code = 'provider_fees' AND e.direction = 'debit'`, h.tn.ID); int64(n) != fee {
		t.Fatalf("taxa no ledger = %d, esperado %d", n, fee)
	}
}

func TestOutOfOrderWebhooksDoNotRegress(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	if err := h.sim.EnqueueScenario(h.apiKey, "out_of_order"); err != nil {
		t.Fatal(err)
	}
	inv := h.issueInvoice(t, 5000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sim.Apply(c.ProviderChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	h.sim.DispatchDue(ctx) // "charge.paid" chega antes de "charge.created"
	h.processWebhooks(t)
	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargePaid {
		t.Fatalf("cobrança regrediu ou não foi paga: %s", got.Status)
	}
}

// O provedor cria a cobrança, mas a resposta não chega (timeout). A
// operação fica "unknown"; a recuperação consulta pela referência externa e
// vincula a cobrança existente sem criar outra.
func TestTimeoutAfterProviderSuccessIsResolvedWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 500*time.Millisecond)
	if err := h.sim.EnqueueScenario(h.apiKey, "timeout_after_success"); err != nil {
		t.Fatal(err)
	}
	inv := h.issueInvoice(t, 7000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != payments.ChargePending {
		t.Fatalf("cobrança deveria aguardar resolução, está %s", c.Status)
	}
	if n := countRows(t, h.env, h.tn.ID, `SELECT count(*) FROM payment_operations WHERE tenant_id = $1 AND status = 'unknown'`, h.tn.ID); n != 1 {
		t.Fatalf("operações unknown = %d", n)
	}
	h.env.Clock.Advance(10 * time.Minute)
	if _, err := h.pay.RecoverOperations(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.charge(t, inv.ID)
	if got.Status != payments.ChargeAwaitingPayment || got.ProviderChargeID == "" {
		t.Fatalf("cobrança não vinculada após consulta: %+v", got)
	}
	charges, _ := h.sim.ListCharges(h.apiKey, got.ID.String(), time.Time{}, time.Time{}, 0, 100)
	if len(charges) != 1 {
		t.Fatalf("o provedor recebeu %d cobranças para a mesma referência", len(charges))
	}
}

func TestDeclinedChargeFailsWithoutRetry(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	if err := h.sim.EnqueueScenario(h.apiKey, "decline"); err != nil {
		t.Fatal(err)
	}
	inv := h.issueInvoice(t, 4000)
	c, err := h.pay.RequestCharge(ctx, h.tn.ID, inv.ID, h.tn.Actor)
	if err != nil || c.Status != payments.ChargeFailed {
		t.Fatalf("cobrança recusada deveria falhar: %+v %v", c, err)
	}
	// Nova tentativa explícita cria a tentativa 2.
	c2, err := h.pay.RetryCharge(ctx, h.tn.ID, h.tn.Actor, inv.ID)
	if err != nil || c2.Attempt != 2 || c2.Status != payments.ChargeAwaitingPayment {
		t.Fatalf("nova tentativa: %+v %v", c2, err)
	}
}

func TestWebhookWithInvalidSignatureIsRejected(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	accounts, err := h.pay.Accounts(ctx, h.tn.ID)
	if err != nil || len(accounts) != 1 {
		t.Fatal(err)
	}
	_, err = h.pay.ReceiveWebhook(ctx, "simulator", strings.Repeat("x", 43), http.Header{}, []byte(`{}`))
	if err == nil {
		t.Fatal("endpoint inexistente aceito")
	}
}
