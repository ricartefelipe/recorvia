package simulator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

const apiKey = "sim_chave_de_teste"

type simEnv struct {
	gw    *Gateway
	sim   *paysim.Sim
	now   time.Time
	hooks chan []byte
}

func newSimEnv(t *testing.T) *simEnv {
	t.Helper()
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	env := &simEnv{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), hooks: make(chan []byte, 32)}
	env.sim = paysim.New(st, "http://checkout.test", func() time.Time { return env.now }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(env.sim.Handler())
	t.Cleanup(srv.Close)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.hooks <- b
	}))
	t.Cleanup(recv.Close)
	env.gw = New(srv.URL, apiKey, srv.Client())
	if err := env.gw.ConfigureWebhook(context.Background(), recv.URL, strings.Repeat("s", 32)); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *simEnv) charge(t *testing.T, amount int64) payments.ProviderCharge {
	t.Helper()
	ctx := context.Background()
	cust, err := e.gw.EnsureCustomer(ctx, payments.CustomerData{ID: uuid.New(), Name: "Cliente", DocumentNumber: "11222333000181"})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := e.gw.CreateCharge(ctx, payments.CreateChargeRequest{ExternalReference: uuid.NewString(), Amount: money.New(amount, money.BRL),
		DueDate: calendar.Date{Year: 2026, Month: 11, Day: 21}, Description: "Fatura", Customer: payments.CustomerData{ProviderCustomerID: cust}})
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func TestRefundAndQueryWithReference(t *testing.T) {
	e := newSimEnv(t)
	ctx := context.Background()
	pc := e.charge(t, 10000)
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	pr, err := e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: pc.ProviderChargeID, Amount: money.New(3000, money.BRL),
		Description: "Cancelamento", ExternalReference: "ref-1"})
	if err != nil || pr.State != payments.RefundStateDone || pr.ExternalReference != "ref-1" || pr.ProviderRefundID == "" || pr.Amount.Amount() != 3000 {
		t.Fatalf("reembolso: %+v %v", pr, err)
	}
	again, err := e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: pc.ProviderChargeID, Amount: money.New(3000, money.BRL),
		ExternalReference: "ref-1"})
	if err != nil || again.ProviderRefundID != pr.ProviderRefundID {
		t.Fatalf("repetição deduplicada pela referência: %+v %v", again, err)
	}
	got, err := e.gw.GetCharge(ctx, pc.ProviderChargeID)
	if err != nil || got.Status != payments.ProviderPartiallyRefunded || got.RefundedAmount.Amount() != 3000 || len(got.Refunds) != 1 ||
		got.Refunds[0].ExternalReference != "ref-1" {
		t.Fatalf("consulta: %+v %v", got, err)
	}
	if _, err := e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: pc.ProviderChargeID, Amount: money.New(8000, money.BRL),
		ExternalReference: "ref-2"}); payments.GatewayErrorKind(err) != payments.ErrKindPermanent {
		t.Fatalf("reembolso acima do saldo: %v", err)
	}
	if _, err := e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: "ch_inexistente", Amount: money.New(1, money.BRL)}); payments.GatewayErrorKind(err) != payments.ErrKindNotFound {
		t.Fatalf("cobrança inexistente: %v", err)
	}
}

func TestRefundScenarios(t *testing.T) {
	e := newSimEnv(t)
	ctx := context.Background()
	pc := e.charge(t, 10000)
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	for scenario, kind := range map[string]payments.ErrorKind{"refund_unavailable": payments.ErrKindRetryable,
		"refund_error_after_success": payments.ErrKindAmbiguous} {
		if err := e.sim.EnqueueScenario(apiKey, scenario); err != nil {
			t.Fatal(err)
		}
		_, err := e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: pc.ProviderChargeID, Amount: money.New(1000, money.BRL),
			ExternalReference: scenario})
		if payments.GatewayErrorKind(err) != kind {
			t.Fatalf("%s: %v", scenario, err)
		}
	}
	got, _ := e.gw.GetCharge(ctx, pc.ProviderChargeID)
	if got.RefundedAmount.Amount() != 1000 || got.Refunds[0].ExternalReference != "refund_error_after_success" {
		t.Fatalf("somente o cenário com resposta perdida efetiva: %+v", got)
	}
}

func TestCancelCharge(t *testing.T) {
	e := newSimEnv(t)
	ctx := context.Background()
	pc := e.charge(t, 5000)
	if err := e.gw.CancelCharge(ctx, pc.ProviderChargeID); err != nil {
		t.Fatal(err)
	}
	if err := e.gw.CancelCharge(ctx, pc.ProviderChargeID); err != nil {
		t.Fatalf("cancelamento repetido é idempotente: %v", err)
	}
	got, _ := e.gw.GetCharge(ctx, pc.ProviderChargeID)
	if got.Status != payments.ProviderCanceled {
		t.Fatalf("situação: %s", got.Status)
	}
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "pay"}); err == nil {
		t.Fatal("cobrança cancelada não pode ser paga")
	}
	paid := e.charge(t, 5000)
	_, _ = e.sim.Apply(paid.ProviderChargeID, paysim.Action{Name: "pay"})
	if err := e.gw.CancelCharge(ctx, paid.ProviderChargeID); payments.GatewayErrorKind(err) != payments.ErrKindPermanent {
		t.Fatalf("cobrança paga: %v", err)
	}
	if err := e.gw.CancelCharge(ctx, "ch_x"); payments.GatewayErrorKind(err) != payments.ErrKindNotFound {
		t.Fatalf("inexistente: %v", err)
	}
	other := e.charge(t, 100)
	if err := e.sim.EnqueueScenario(apiKey, "cancel_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := e.gw.CancelCharge(ctx, other.ProviderChargeID); payments.GatewayErrorKind(err) != payments.ErrKindRetryable {
		t.Fatalf("indisponível: %v", err)
	}
}

func TestWebhookCarriesRefundsAndListPaginates(t *testing.T) {
	e := newSimEnv(t)
	ctx := context.Background()
	pc := e.charge(t, 10000)
	_, _ = e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "pay"})
	_, _ = e.gw.Refund(ctx, payments.RefundRequest{ProviderChargeID: pc.ProviderChargeID, Amount: money.New(10000, money.BRL), ExternalReference: "r"})
	e.sim.DispatchDue(ctx)
	var last payments.WebhookEvent
	for len(e.hooks) > 0 {
		ev, err := Verifier{}.Parse(<-e.hooks)
		if err != nil {
			t.Fatal(err)
		}
		last = ev
	}
	if last.Type != "charge.refunded" || last.Status != payments.ProviderRefunded || len(last.Charge.Refunds) != 1 ||
		last.Charge.Refunds[0].State != payments.RefundStateDone {
		t.Fatalf("evento de reembolso: %+v", last)
	}
	for range 3 {
		e.charge(t, 100)
	}
	page, err := e.gw.ListCharges(ctx, payments.ListRequest{UpdatedFrom: e.now.Add(-time.Hour), UpdatedTo: e.now.Add(time.Hour), Limit: 2})
	if err != nil || len(page.Charges) != 2 || !page.HasMore {
		t.Fatalf("primeira página: %+v %v", page, err)
	}
	rest, err := e.gw.ListCharges(ctx, payments.ListRequest{UpdatedFrom: e.now.Add(-time.Hour), UpdatedTo: e.now.Add(time.Hour), Offset: 2, Limit: 2})
	if err != nil || len(rest.Charges) != 2 || rest.HasMore {
		t.Fatalf("segunda página: %+v %v", rest, err)
	}
}

func TestRefundStateMapping(t *testing.T) {
	for status, want := range map[string]payments.RefundState{"succeeded": payments.RefundStateDone, "failed": payments.RefundStateFailed,
		"canceled": payments.RefundStateFailed, "pending": payments.RefundStatePending} {
		if got := refundState(status); got != want {
			t.Fatalf("%s: %s", status, got)
		}
	}
	var dto chargeDTO
	if err := json.Unmarshal([]byte(`{"id":"ch_1","status":"paid","paid_amount":100,"fee":10,"net_amount":90,"refunds":[{"id":"rf_1","amount":40,"status":"succeeded"}]}`), &dto); err != nil {
		t.Fatal(err)
	}
	if pc := dto.toProvider(); len(pc.Refunds) != 1 || pc.Refunds[0].ProviderRefundID != "rf_1" || pc.Refunds[0].Amount.Amount() != 40 {
		t.Fatalf("conversão: %+v", pc)
	}
}

// O simulador distingue o pagamento confirmado e ainda não creditado (confirm)
// do recebido (pay, ou receive depois de confirm), como o Asaas (CONFIRMED e
// RECEIVED).
func TestConfirmedAndReceivedStatuses(t *testing.T) {
	e := newSimEnv(t)
	ctx := context.Background()
	pc := e.charge(t, 10000)
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "receive"}); err == nil {
		t.Fatal("cobrança pendente não pode ser creditada")
	}
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "confirm"}); err != nil {
		t.Fatal(err)
	}
	got, err := e.gw.GetCharge(ctx, pc.ProviderChargeID)
	if err != nil || got.Status != payments.ProviderConfirmed || got.RawStatus != "confirmed" || got.Fee == nil || got.Fee.Amount() != paysim.Fee(10000) ||
		got.PaidAt == nil {
		t.Fatalf("confirmada: %+v %v", got, err)
	}
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "refund"}); err == nil {
		t.Fatal("o simulador só reembolsa cobrança creditada")
	}
	if _, err := e.sim.Apply(pc.ProviderChargeID, paysim.Action{Name: "receive"}); err != nil {
		t.Fatal(err)
	}
	if got, err = e.gw.GetCharge(ctx, pc.ProviderChargeID); err != nil || got.Status != payments.ProviderReceived || got.Fee.Amount() != paysim.Fee(10000) {
		t.Fatalf("recebida: %+v %v", got, err)
	}
	e.sim.DispatchDue(ctx)
	var types []string
	var statuses []payments.ProviderStatus
	for len(e.hooks) > 0 {
		ev, err := Verifier{}.Parse(<-e.hooks)
		if err != nil {
			t.Fatal(err)
		}
		types, statuses = append(types, ev.Type), append(statuses, ev.Status)
	}
	if strings.Join(types, ",") != "charge.created,charge.confirmed,charge.received" ||
		statuses[1] != payments.ProviderConfirmed || statuses[2] != payments.ProviderReceived {
		t.Fatalf("eventos: %v %v", types, statuses)
	}
	if MapStatus("confirmed", "") != payments.ProviderConfirmed || MapStatus("paid", "") != payments.ProviderReceived {
		t.Fatal("mapeamento das situações")
	}
}
