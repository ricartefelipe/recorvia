package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

var (
	errAmbiguous = &GatewayError{Kind: ErrKindAmbiguous, StatusCode: 502, Code: "bad_gateway", Message: "resposta perdida"}
	errRetryable = &GatewayError{Kind: ErrKindRetryable, StatusCode: 429, Code: "rate_limited", Message: "muitas requisições"}
	errPermanent = &GatewayError{Kind: ErrKindPermanent, StatusCode: 400, Code: "invalid_customer", Message: "CPF/CNPJ inválido"}
)

func TestRequestChargeCreatesChargeAndExecutesOperation(t *testing.T) {
	// A bolha do synctest torna exato o prazo derivado do relógio real.
	synctest.Test(t, testRequestChargeCreatesChargeAndExecutesOperation)
}

func testRequestChargeCreatesChargeAndExecutesOperation(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	var leaseDuringCall *time.Time
	var statusDuringCall OperationStatus
	f.gw.onCreate = func() {
		for _, op := range f.store.operations {
			leaseDuringCall, statusDuringCall = op.LeaseUntil, op.Status
		}
	}

	c := f.requestCharge(t, inv.ID)

	if c.Status != ChargeAwaitingPayment || c.ProviderChargeID != "pay_1" || c.CheckoutURL != "https://sandbox.exemplo/c/pay_1" {
		t.Fatalf("cobrança após execução: %+v", c)
	}
	if c.Attempt != 1 || c.Amount.Amount() != 10000 || c.InvoiceID != inv.ID || c.ProviderAccountID != f.account.ID || c.Provider != "simulator" {
		t.Fatalf("dados da cobrança: %+v", c)
	}
	if c.DueDate != (calendar.Date{Year: 2026, Month: time.November, Day: 21}) {
		t.Fatalf("vencimento da cobrança segue o da fatura: %v", c.DueDate)
	}
	if len(f.gw.createCalls) != 1 {
		t.Fatalf("CreateCharge chamado %d vezes", len(f.gw.createCalls))
	}
	req := f.gw.createCalls[0]
	if req.ExternalReference != c.ID.String() || req.Amount.Amount() != 10000 || req.DueDate != c.DueDate ||
		req.Description != "Fatura FAT-2026-000123" || req.Customer.ProviderCustomerID != "cus_1" || req.Customer.Name != "Padaria Central" {
		t.Fatalf("pedido ao provedor: %+v", req)
	}
	if len(f.gw.findCalls) != 0 {
		t.Fatal("primeira tentativa não consulta o provedor")
	}
	if len(f.gw.ensureCalls) != 1 || f.store.providerCustomers[f.customer.String()+"|simulator"] != "cus_1" {
		t.Fatalf("cliente do provedor deve ser criado e guardado: %+v", f.store.providerCustomers)
	}
	if statusDuringCall != OpInFlight || leaseDuringCall == nil || !leaseDuringCall.Equal(payNow.Add(60*time.Second)) {
		t.Fatalf("a chamada ocorre com a operação reivindicada e lease de 3x o timeout: %s %v", statusDuringCall, leaseDuringCall)
	}
	if len(f.gw.createDeadline) != 1 || f.gw.createDeadline[0] != 20*time.Second {
		t.Fatalf("a chamada ao provedor tem timeout padrão de 20s: %v", f.gw.createDeadline)
	}

	op := f.operationFor(t, c.ID)
	if op.Kind != OpCreateCharge || op.Status != OpSucceeded || op.Attempts != 1 || op.CompletedAt == nil || op.LeaseUntil != nil ||
		op.ExternalReference != c.ID.String() || op.ProviderResponse["provider_charge_id"] != "pay_1" {
		t.Fatalf("operação: %+v", op)
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventChargeCreated}) {
		t.Fatalf("eventos: %v", got)
	}
	if p := f.store.events[0].Payload.(map[string]any); p["checkout_url"] != c.CheckoutURL || p["invoice_id"] != inv.ID {
		t.Fatalf("payload: %+v", p)
	}
	if len(f.store.audits) != 2 || f.store.audits[0].Action != "charge.requested" || f.store.audits[0].Actor != f.actor ||
		f.store.audits[1].Action != "payment_operation.succeeded" || f.store.audits[1].Actor.Type != audit.ActorSystem {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
}

func TestRequestChargeIsIdempotentWhileChargeIsOpen(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	first := f.requestCharge(t, inv.ID)
	second := f.requestCharge(t, inv.ID)
	if second.ID != first.ID || len(f.store.charges) != 1 || len(f.store.operations) != 1 || len(f.gw.createCalls) != 1 {
		t.Fatalf("cobrança em andamento deve ser reutilizada: %d cobranças, %d chamadas", len(f.store.charges), len(f.gw.createCalls))
	}
}

func TestRequestChargeReusesPendingChargeAfterFailedExecution(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errRetryable}}
	first := f.requestCharge(t, inv.ID)
	if first.Status != ChargePending {
		t.Fatalf("falha repetível mantém a cobrança pendente: %s", first.Status)
	}
	again := f.requestCharge(t, inv.ID)
	if again.ID != first.ID || len(f.store.charges) != 1 {
		t.Fatal("cobrança pendente deve ser reutilizada")
	}
}

func TestRequestChargeWithoutWork(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*payFixture, billing.Invoice)
	}{
		{"sem provedor configurado", func(f *payFixture, _ billing.Invoice) {
			a := f.account
			a.IsDefault = false
			f.store.accounts[a.ID] = a
		}},
		{"fatura paga", func(f *payFixture, inv billing.Invoice) {
			inv.Status, inv.AmountDue = billing.Paid, brl(0)
			f.store.invoices[inv.ID] = inv
		}},
		{"fatura cancelada", func(f *payFixture, inv billing.Invoice) {
			inv.Status = billing.Void
			f.store.invoices[inv.ID] = inv
		}},
		{"fatura em aberto sem saldo", func(f *payFixture, inv billing.Invoice) {
			inv.AmountDue = brl(0)
			f.store.invoices[inv.ID] = inv
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			c.setup(f, inv)
			got, err := f.svc.RequestCharge(context.Background(), f.tenantID, inv.ID, f.actor)
			if err != nil || got != nil {
				t.Fatalf("esperava nenhuma cobrança e nenhum erro: %+v %v", got, err)
			}
			if len(f.store.charges) != 0 || len(f.store.operations) != 0 || len(f.store.audits) != 0 || len(f.resolver.accounts) != 0 {
				t.Fatal("nada deve ser criado nem chamado")
			}
		})
	}
}

func TestRequestChargeNewAttemptAfterFailure(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errPermanent}}
	failed := f.requestCharge(t, inv.ID)
	if failed.Status != ChargeFailed {
		t.Fatalf("erro permanente: %s", failed.Status)
	}
	retry := f.requestCharge(t, inv.ID)
	if retry.ID == failed.ID || retry.Attempt != 2 || retry.Status != ChargeAwaitingPayment {
		t.Fatalf("nova tentativa: %+v", retry)
	}
	if len(f.gw.createCalls) != 2 || f.gw.createCalls[1].ExternalReference != retry.ID.String() {
		t.Fatal("a nova tentativa usa a própria referência externa")
	}
}

func TestRequestChargeDueDateNeverInThePast(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.clock.Set(time.Date(2026, time.December, 10, 15, 0, 0, 0, time.UTC))
	c := f.requestCharge(t, inv.ID)
	if c.DueDate != (calendar.Date{Year: 2026, Month: time.December, Day: 10}) {
		t.Fatalf("fatura vencida gera cobrança com vencimento hoje: %v", c.DueDate)
	}
}

func TestRequestChargeErrors(t *testing.T) {
	for _, m := range []string{"Invoice", "LatestCharge", "DefaultAccount", "Location", "InsertCharge", "InsertOperation", "Audit"} {
		t.Run(m, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if _, err := f.svc.RequestCharge(context.Background(), f.tenantID, inv.ID, f.actor); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.charges) != 0 || len(f.gw.createCalls) != 0 {
				t.Fatal("falha não pode deixar cobrança nem chamar o provedor")
			}
		})
	}
	t.Run("fatura inexistente", func(t *testing.T) {
		f := newPayFixture(t)
		_, err := f.svc.RequestCharge(context.Background(), f.tenantID, uuid.New(), f.actor)
		wantAppErr(t, err, apperr.KindNotFound, "invoice_not_found")
	})
}

func TestRequestChargeDefersExecutionWhenItFails(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.store.errs["CustomerData"] = errors.New("banco indisponível")
	c := f.requestCharge(t, inv.ID)
	if c.Status != ChargePending || len(f.gw.createCalls) != 0 {
		t.Fatalf("cobrança registrada e execução adiada: %s", c.Status)
	}
	if op := f.operationFor(t, c.ID); op.Status != OpPending || op.Attempts != 0 {
		t.Fatalf("operação permanece pendente para a recuperação: %+v", op)
	}
	if !strings.Contains(f.logs.String(), "criação da cobrança adiada para a recuperação") {
		t.Fatalf("o adiamento deve ser registrado no log: %s", f.logs.String())
	}
}

func TestExecuteOperationPermanentErrorFailsCharge(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: fmt.Errorf("criar cobrança: %w", errPermanent)}}
	c := f.requestCharge(t, inv.ID)
	if c.Status != ChargeFailed || !strings.Contains(c.FailureReason, "CPF/CNPJ inválido") {
		t.Fatalf("erro permanente falha a cobrança: %s %q", c.Status, c.FailureReason)
	}
	op := f.operationFor(t, c.ID)
	if op.Status != OpFailed || op.CompletedAt == nil || !strings.Contains(op.LastError, "status=400") {
		t.Fatalf("operação: %+v", op)
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventChargeFailed}) {
		t.Fatalf("eventos: %v", got)
	}
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil || len(f.gw.createCalls) != 1 {
		t.Fatal("operação concluída não é executada de novo")
	}
}

func TestExecuteOperationRetryableErrorSchedulesBackoff(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errRetryable}}
	c := f.requestCharge(t, inv.ID)
	op := f.operationFor(t, c.ID)
	if c.Status != ChargePending || op.Status != OpPending || op.Attempts != 1 || !op.NextAttemptAt.Equal(payNow.Add(Backoff(1))) {
		t.Fatalf("falha repetível agenda nova tentativa: %s %+v", c.Status, op)
	}
	if !strings.Contains(op.LastError, "rate_limited") || len(f.store.events) != 0 {
		t.Fatalf("erro registrado sem eventos: %q %v", op.LastError, f.store.eventTypes())
	}
	if f.store.audits[len(f.store.audits)-1].Action != "payment_operation.pending" {
		t.Fatalf("auditoria: %+v", f.store.audits[len(f.store.audits)-1])
	}

	f.clock.Advance(29 * time.Second)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil || len(f.gw.createCalls) != 1 {
		t.Fatal("antes do backoff a operação não é executada")
	}
	f.clock.Advance(time.Second)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.gw.createCalls) != 2 || f.store.charges[c.ID].Status != ChargeAwaitingPayment {
		t.Fatalf("nova tentativa após o backoff: %d chamadas, %s", len(f.gw.createCalls), f.store.charges[c.ID].Status)
	}
}

func TestExecuteOperationAmbiguousErrorQueriesProviderBeforeRetrying(t *testing.T) {
	cases := map[string]error{
		"erro 5xx sem resposta confiável": errAmbiguous,
		"timeout":                         fmt.Errorf("post /payments: %w", context.DeadlineExceeded),
	}
	for name, callErr := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: callErr}}
			c := f.requestCharge(t, inv.ID)
			op := f.operationFor(t, c.ID)
			if c.Status != ChargePending || op.Status != OpUnknown || !op.NextAttemptAt.Equal(payNow.Add(30*time.Second)) {
				t.Fatalf("resultado desconhecido: %s %+v", c.Status, op)
			}

			// O provedor criou a cobrança apesar do erro.
			f.gw.findResult = &ProviderCharge{ProviderChargeID: "pay_77", ExternalReference: c.ID.String(), ProviderCustomerID: "cus_9",
				Status: ProviderPending, RawStatus: "PENDING", Amount: brl(10000), CheckoutURL: "https://sandbox.exemplo/c/pay_77"}
			f.clock.Advance(30 * time.Second)
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil {
				t.Fatal(err)
			}
			if len(f.gw.findCalls) != 1 || f.gw.findCalls[0] != c.ID.String() {
				t.Fatalf("a nova tentativa deve consultar pela referência externa: %v", f.gw.findCalls)
			}
			if len(f.gw.createCalls) != 1 {
				t.Fatalf("CreateCharge chamado %d vezes; a cobrança encontrada não pode ser criada de novo", len(f.gw.createCalls))
			}
			got := f.store.charges[c.ID]
			if got.Status != ChargeAwaitingPayment || got.ProviderChargeID != "pay_77" || got.CheckoutURL != "https://sandbox.exemplo/c/pay_77" {
				t.Fatalf("cobrança vinculada à existente no provedor: %+v", got)
			}
			if f.store.providerCustomers[f.customer.String()+"|simulator"] != "cus_9" {
				t.Fatal("cliente do provedor informado na consulta deve ser guardado")
			}
			if op := f.store.operations[op.ID]; op.Status != OpSucceeded || op.Attempts != 2 {
				t.Fatalf("operação concluída: %+v", op)
			}
		})
	}
}

func TestExecuteOperationAmbiguousNotFoundCreatesWithSameReference(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errAmbiguous}}
	c := f.requestCharge(t, inv.ID)
	f.clock.Advance(30 * time.Second)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	if len(f.gw.findCalls) != 1 || len(f.gw.createCalls) != 2 || f.gw.createCalls[1].ExternalReference != c.ID.String() {
		t.Fatalf("não encontrada: cria com a mesma referência: find=%d create=%d", len(f.gw.findCalls), len(f.gw.createCalls))
	}
	if f.store.charges[c.ID].Status != ChargeAwaitingPayment {
		t.Fatal("cobrança criada na segunda tentativa")
	}
	if len(f.gw.ensureCalls) != 2 {
		t.Fatalf("cliente do provedor ainda não estava guardado: %d", len(f.gw.ensureCalls))
	}
}

func TestExecuteOperationLookupFailures(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status OperationStatus
		charge ChargeStatus
	}{
		{"consulta rejeitada definitivamente", errPermanent, OpFailed, ChargeFailed},
		{"consulta sem resposta", errAmbiguous, OpUnknown, ChargePending},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: errAmbiguous}}
			ch := f.requestCharge(t, inv.ID)
			f.gw.findErr = c.err
			f.clock.Advance(30 * time.Second)
			opID := f.operationFor(t, ch.ID).ID
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, opID); err != nil {
				t.Fatal(err)
			}
			if len(f.gw.createCalls) != 1 {
				t.Fatal("falha na consulta nunca leva a criar às cegas")
			}
			if op := f.store.operations[opID]; op.Status != c.status || f.store.charges[ch.ID].Status != c.charge {
				t.Fatalf("operação %s cobrança %s", op.Status, f.store.charges[ch.ID].Status)
			}
		})
	}
}

func TestExecuteOperationStopsAfterMaxAttempts(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errAmbiguous}}
	f.gw.findErr = errAmbiguous
	c := f.requestCharge(t, inv.ID)
	opID := f.operationFor(t, c.ID).ID
	for range 10 {
		f.clock.Advance(time.Hour)
		if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, opID); err != nil {
			t.Fatal(err)
		}
	}
	op := f.store.operations[opID]
	if op.Attempts != MaxOperationAttempts || op.Status != OpUnknown {
		t.Fatalf("após o limite a operação fica desconhecida para a conciliação: %+v", op)
	}
	if len(f.gw.findCalls) != MaxOperationAttempts-1 || len(f.gw.createCalls) != 1 {
		t.Fatalf("consultas=%d criações=%d", len(f.gw.findCalls), len(f.gw.createCalls))
	}
}

func TestExecuteOperationFoundChargeAlreadyPaidAppliesPayment(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errAmbiguous}}
	c := f.requestCharge(t, inv.ID)
	paidAt := payNow.Add(10 * time.Second)
	found := providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, &paidAt)
	found.ProviderChargeID = "pay_88"
	f.gw.findResult = found
	f.clock.Advance(30 * time.Second)
	if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, f.operationFor(t, c.ID).ID); err != nil {
		t.Fatal(err)
	}
	if got := f.store.charges[c.ID]; got.Status != ChargePaid || got.Fee.Amount() != 199 || !got.PaidAt.Equal(paidAt) {
		t.Fatalf("cobrança já paga no provedor: %+v", got)
	}
	if f.invoice(inv.ID).Status != billing.Paid || len(f.store.payments) != 1 {
		t.Fatal("o pagamento deve ser aplicado à fatura")
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventChargeCreated, outbox.EventPaymentConfirmed, outbox.EventInvoicePaid}) {
		t.Fatalf("eventos: %v", got)
	}
}

func TestExecuteOperationDoesNotOverwriteOperationCompletedElsewhere(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.onCreate = func() {
		// Outra instância (lease expirado) concluiu a operação durante a chamada.
		for id, op := range f.store.operations {
			op.Status, op.LeaseUntil = OpSucceeded, nil
			f.store.operations[id] = op
			c := f.store.charges[op.ChargeID]
			c.Status, c.ProviderChargeID = ChargeAwaitingPayment, "pay_outra_instancia"
			f.store.charges[c.ID] = c
		}
	}
	c := f.requestCharge(t, inv.ID)
	if c.ProviderChargeID != "pay_outra_instancia" || c.Status != ChargeAwaitingPayment {
		t.Fatalf("resultado da outra instância deve prevalecer: %+v", c)
	}
	if len(f.store.events) != 0 {
		t.Fatalf("nenhum evento duplicado: %v", f.store.eventTypes())
	}
	if op := f.operationFor(t, c.ID); op.Status != OpSucceeded || op.ProviderResponse != nil {
		t.Fatalf("operação não pode ser sobrescrita: %+v", op)
	}
}

func TestExecuteOperationEdgeCases(t *testing.T) {
	t.Run("gateway indisponível para a conta", func(t *testing.T) {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		f.resolver.gwErr = errors.New("credencial não pode ser decifrada")
		c := f.requestCharge(t, inv.ID)
		if c.Status != ChargeFailed || !strings.Contains(c.FailureReason, "decifrada") {
			t.Fatalf("falha permanente: %s %q", c.Status, c.FailureReason)
		}
	})
	t.Run("cliente já cadastrado no provedor", func(t *testing.T) {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		f.store.providerCustomers[f.customer.String()+"|simulator"] = "cus_antigo"
		f.requestCharge(t, inv.ID)
		if len(f.gw.ensureCalls) != 0 || f.gw.createCalls[0].Customer.ProviderCustomerID != "cus_antigo" {
			t.Fatal("cliente existente no provedor é reutilizado")
		}
	})
	for name, ensureErr := range map[string]error{"cadastro do cliente rejeitado": errPermanent, "cadastro do cliente sem resposta": errAmbiguous} {
		t.Run(name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.ensureErr = ensureErr
			c := f.requestCharge(t, inv.ID)
			if len(f.gw.createCalls) != 0 {
				t.Fatal("sem cliente não há criação de cobrança")
			}
			want := map[error]OperationStatus{errPermanent: OpFailed, errAmbiguous: OpUnknown}[ensureErr]
			if op := f.operationFor(t, c.ID); op.Status != want {
				t.Fatalf("operação %s, esperada %s", op.Status, want)
			}
		})
	}
	t.Run("operação de tipo não suportado", func(t *testing.T) {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		c := f.requestCharge(t, inv.ID)
		op := Operation{ID: uuid.New(), TenantID: f.tenantID, ChargeID: c.ID, Kind: "settlement", Status: OpPending, NextAttemptAt: payNow}
		f.store.operations[op.ID] = op
		err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID)
		if err == nil || !strings.Contains(err.Error(), "não suportada") {
			t.Fatalf("esperava erro de operação não suportada: %v", err)
		}
	})
	t.Run("operação inexistente", func(t *testing.T) {
		f := newPayFixture(t)
		err := f.svc.ExecuteOperation(context.Background(), f.tenantID, uuid.New())
		wantAppErr(t, err, apperr.KindNotFound, "operation_not_found")
	})
	for _, m := range []string{"UpdateOperation", "Charge", "Account", "Invoice"} {
		t.Run("falha ao reivindicar: "+m, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: errRetryable}}
			c := f.requestCharge(t, inv.ID)
			op := f.operationFor(t, c.ID)
			f.clock.Advance(time.Minute)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if got := f.store.operations[op.ID]; got.Attempts != op.Attempts || got.Status != OpPending {
				t.Fatalf("a reivindicação deve ser desfeita: %+v", got)
			}
		})
	}
}

func TestRecordOutcomeFailureIsRecoveredWithoutDuplicateCreation(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.store.errs["SaveProviderCustomer"] = errors.New("conexão perdida no commit")
	c := f.requestCharge(t, inv.ID)
	if c.Status != ChargePending || len(f.gw.createCalls) != 1 {
		t.Fatalf("resultado não registrado: %s", c.Status)
	}
	op := f.operationFor(t, c.ID)
	if op.Status != OpInFlight || op.LeaseUntil == nil {
		t.Fatalf("a operação fica reivindicada até o lease expirar: %+v", op)
	}

	delete(f.store.errs, "SaveProviderCustomer")
	f.gw.findResult = &ProviderCharge{ProviderChargeID: "pay_1", ExternalReference: c.ID.String(), ProviderCustomerID: "cus_1",
		Status: ProviderPending, RawStatus: "PENDING", Amount: brl(10000), CheckoutURL: "https://sandbox.exemplo/c/pay_1"}
	if r, err := f.svc.RecoverOperations(context.Background()); err != nil || r.Executed != 0 {
		t.Fatalf("lease vigente: nada a recuperar: %d %v", r.Executed, err)
	}
	f.clock.Advance(61 * time.Second)
	r, err := f.svc.RecoverOperations(context.Background())
	if err != nil || r.Executed != 1 {
		t.Fatalf("recuperação: %d %v", r.Executed, err)
	}
	if len(f.gw.createCalls) != 1 || len(f.gw.findCalls) != 1 {
		t.Fatalf("a recuperação consulta e não cria de novo: create=%d find=%d", len(f.gw.createCalls), len(f.gw.findCalls))
	}
	if got := f.store.charges[c.ID]; got.Status != ChargeAwaitingPayment || got.ProviderChargeID != "pay_1" {
		t.Fatalf("cobrança vinculada: %+v", got)
	}
}

func TestRecoverOperations(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errRetryable}}
	f.requestCharge(t, inv.ID)

	other := uuid.New()
	otherAccount := ProviderAccount{ID: uuid.New(), TenantID: other, Provider: "simulator", IsDefault: true}
	f.store.accounts[otherAccount.ID] = otherAccount
	otherInv := f.addInvoice(t, 5000)
	otherInv.TenantID = other
	f.store.invoices[otherInv.ID] = otherInv
	f.gw.createResults = []gwResult{{err: errRetryable}}
	if _, err := f.svc.RequestCharge(context.Background(), other, otherInv.ID, f.actor); err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(Backoff(1))
	r, err := f.svc.RecoverOperations(context.Background())
	if err != nil || r.Executed != 2 || !r.Complete {
		t.Fatalf("operações dos dois tenants: %+v %v", r, err)
	}
	if f.store.dueLimit != 50 {
		t.Fatalf("lote por tenant: %d", f.store.dueLimit)
	}
	for _, c := range f.store.charges {
		if c.Status != ChargeAwaitingPayment {
			t.Fatalf("cobrança %s não recuperada: %s", c.ID, c.Status)
		}
	}
}

func TestRecoverOperationsCollectsErrors(t *testing.T) {
	f := newPayFixture(t)
	invA := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errRetryable}}
	f.requestCharge(t, invA.ID)
	broken := uuid.New()
	f.store.operations[uuid.New()] = Operation{ID: uuid.New(), TenantID: broken, Kind: OpCreateCharge, Status: OpPending, NextAttemptAt: payNow}
	boom := errors.New("tenant indisponível")
	f.store.tenantDueErr[broken] = boom
	orphan := Operation{ID: uuid.New(), TenantID: f.tenantID, ChargeID: uuid.New(), Kind: OpCreateCharge, Status: OpPending, NextAttemptAt: payNow}
	f.store.operations[orphan.ID] = orphan

	f.clock.Advance(time.Minute)
	r, err := f.svc.RecoverOperations(context.Background())
	if r.Executed != 1 {
		t.Fatalf("a operação válida deve ser recuperada mesmo com falhas em outras: %d", r.Executed)
	}
	if r.Complete {
		t.Fatal("com um tenant sem consulta, a contagem de operações esgotadas é parcial")
	}
	if !errors.Is(err, boom) || !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("erros devem ser agregados: %v", err)
	}

	f.store.tenantsErr = errors.New("consulta falhou")
	if _, err := f.svc.RecoverOperations(context.Background()); !errors.Is(err, f.store.tenantsErr) {
		t.Fatalf("falha ao listar tenants: %v", err)
	}
}

func TestRecoverOperationsHonorsCancellation(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.gw.createResults = []gwResult{{err: errRetryable}}
	f.requestCharge(t, inv.ID)
	f.clock.Advance(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := f.svc.RecoverOperations(ctx)
	if r.Executed != 0 || r.Complete || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelamento: %+v %v", r, err)
	}
	if len(f.gw.createCalls) != 1 {
		t.Fatal("nenhuma chamada após cancelamento")
	}
}

func TestNewServiceDefaults(t *testing.T) {
	synctest.Test(t, testNewServiceDefaults)
}

func testNewServiceDefaults(t *testing.T) {
	f := newPayFixture(t)
	svc := NewService(f.store, f.resolver, f.clock, Config{CallTimeout: 5 * time.Second, Lease: 4 * time.Second})
	inv := f.addInvoice(t, 10000)
	var lease time.Time
	f.gw.onCreate = func() {
		for _, op := range f.store.operations {
			if op.Status == OpInFlight {
				lease = *op.LeaseUntil
			}
		}
	}
	if _, err := svc.RequestCharge(context.Background(), f.tenantID, inv.ID, f.actor); err != nil {
		t.Fatal(err)
	}
	if !lease.Equal(payNow.Add(15 * time.Second)) {
		t.Fatalf("lease menor que o timeout deve virar 3x o timeout: %v", lease.Sub(payNow))
	}
	if f.gw.createDeadline[0] != 5*time.Second {
		t.Fatalf("timeout configurado: %v", f.gw.createDeadline[0])
	}

	svc = NewService(f.store, f.resolver, f.clock, Config{CallTimeout: 5 * time.Second, Lease: time.Minute})
	inv2 := f.addInvoice(t, 10000)
	if _, err := svc.RequestCharge(context.Background(), f.tenantID, inv2.ID, f.actor); err != nil {
		t.Fatal(err)
	}
	if !lease.Equal(payNow.Add(time.Minute)) {
		t.Fatalf("lease configurado acima do timeout é mantido: %v", lease.Sub(payNow))
	}
	if !svc.Now().Equal(payNow) {
		t.Fatal("Now expõe o relógio do serviço")
	}
}

// Webhooks.

func TestReceiveWebhookStoresEvent(t *testing.T) {
	f := newPayFixture(t)
	body := []byte("evento:evt_1")
	occurred := payNow.Add(-time.Minute)
	f.verifier.events[string(body)] = WebhookEvent{EventID: "evt_1", Type: "PAYMENT_CONFIRMED", ProviderChargeID: "pay_1", OccurredAt: &occurred}

	dup, err := f.svc.ReceiveWebhook(context.Background(), "simulator", endpointToken, http.Header{"X-Assinatura": {"abc"}}, body)
	if err != nil || dup {
		t.Fatalf("recebimento: dup=%v err=%v", dup, err)
	}
	if len(f.store.webhooks) != 1 {
		t.Fatalf("webhooks: %d", len(f.store.webhooks))
	}
	var w StoredWebhook
	for _, v := range f.store.webhooks {
		w = v
	}
	if w.Status != "received" || w.EventID != "evt_1" || w.ProviderChargeID != "pay_1" || w.TenantID != f.tenantID ||
		w.ProviderAccountID != f.account.ID || string(w.Payload) != string(body) || !w.ReceivedAt.Equal(payNow) || !w.OccurredAt.Equal(occurred) {
		t.Fatalf("webhook persistido: %+v", w)
	}
	if len(f.verifier.verifyCalls) != 1 || f.verifier.verifyCalls[0].secret != f.account.Secret || !f.verifier.verifyCalls[0].now.Equal(payNow) {
		t.Fatalf("verificação com o segredo da conta: %+v", f.verifier.verifyCalls)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventProviderWebhook || f.store.events[0].AggregateID != w.ID {
		t.Fatalf("evento de processamento assíncrono: %+v", f.store.events)
	}

	dup, err = f.svc.ReceiveWebhook(context.Background(), "simulator", endpointToken, nil, body)
	if err != nil || !dup {
		t.Fatalf("evento repetido: dup=%v err=%v", dup, err)
	}
	if len(f.store.webhooks) != 1 || len(f.store.events) != 1 {
		t.Fatal("evento repetido não gera novo registro nem novo evento na outbox")
	}
}

func TestReceiveWebhookRejections(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		token    string
		body     []byte
		setup    func(*payFixture)
		kind     apperr.Kind
		code     string
	}{
		{"provedor não suportado", "stripe", endpointToken, []byte("evento:x"), nil, apperr.KindNotFound, "webhook_endpoint_not_found"},
		{"token curto", "simulator", "curto", []byte("evento:x"), nil, apperr.KindNotFound, "webhook_endpoint_not_found"},
		{"token longo", "simulator", strings.Repeat("t", 201), []byte("evento:x"), nil, apperr.KindNotFound, "webhook_endpoint_not_found"},
		{"endpoint desconhecido", "simulator", strings.Repeat("z", 40), []byte("evento:x"), nil, apperr.KindNotFound, "webhook_endpoint_not_found"},
		{"token de outro provedor", "asaas", endpointToken, []byte("evento:x"), nil, apperr.KindNotFound, "webhook_endpoint_not_found"},
		{"corpo acima do limite", "simulator", endpointToken, make([]byte, MaxWebhookBody+1), nil, apperr.KindInvalid, "webhook_too_large"},
		{"verificação falha", "simulator", endpointToken, []byte("evento:x"), func(f *payFixture) {
			f.verifier.verifyErr = errors.New("assinatura HMAC divergente")
		}, apperr.KindUnauthenticated, "webhook_unauthenticated"},
		{"corpo inválido", "simulator", endpointToken, []byte("{nao-e-json"), nil, apperr.KindInvalid, "webhook_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			f.verifier.events["evento:x"] = WebhookEvent{EventID: "x"}
			if c.setup != nil {
				c.setup(f)
			}
			dup, err := f.svc.ReceiveWebhook(context.Background(), c.provider, c.token, nil, c.body)
			if dup {
				t.Fatal("rejeição não é duplicidade")
			}
			wantAppErr(t, err, c.kind, c.code)
			if len(f.store.webhooks) != 0 || len(f.store.events) != 0 {
				t.Fatal("webhook rejeitado não é persistido")
			}
		})
	}
}

func TestReceiveWebhookInfrastructureErrors(t *testing.T) {
	t.Run("falha ao resolver o endpoint", func(t *testing.T) {
		f := newPayFixture(t)
		boom := errors.New("banco indisponível")
		f.store.errs["ResolveWebhookEndpoint"] = boom
		if _, err := f.svc.ReceiveWebhook(context.Background(), "simulator", endpointToken, nil, []byte("x")); !errors.Is(err, boom) {
			t.Fatalf("esperava %v, obtive %v", boom, err)
		}
	})
	t.Run("verificador indisponível", func(t *testing.T) {
		f := newPayFixture(t)
		f.resolver.verifierErr = errors.New("sem verificador")
		if _, err := f.svc.ReceiveWebhook(context.Background(), "simulator", endpointToken, nil, []byte("x")); !errors.Is(err, f.resolver.verifierErr) {
			t.Fatalf("erro: %v", err)
		}
	})
	for _, m := range []string{"Account", "InsertWebhook", "Emit"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f := newPayFixture(t)
			f.verifier.events["evento:x"] = WebhookEvent{EventID: "x"}
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if _, err := f.svc.ReceiveWebhook(context.Background(), "simulator", endpointToken, nil, []byte("evento:x")); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.webhooks) != 0 {
				t.Fatal("falha desfaz o registro")
			}
		})
	}
}

// awaitingCharge cria fatura de 100,00 e cobrança aguardando pagamento (pay_1).
func awaitingCharge(t *testing.T, f *payFixture) (billing.Invoice, Charge) {
	t.Helper()
	inv := f.addInvoice(t, 10000)
	c := f.requestCharge(t, inv.ID)
	f.store.events, f.store.audits = nil, nil
	return inv, c
}

func TestProcessWebhookConfirmationAppliesPayment(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	paidAt := payNow.Add(2 * time.Hour)
	f.clock.Advance(3 * time.Hour)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", Type: "PAYMENT_CONFIRMED", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, &paidAt)})

	msg := f.process(t, wid)

	if len(f.gw.getCalls) != 0 {
		t.Fatal("confirmação é a próxima etapa natural: não exige consulta ao provedor")
	}
	got := f.store.charges[c.ID]
	if got.Status != ChargePaid || got.AmountPaid.Amount() != 10000 || got.Fee.Amount() != 199 || !got.PaidAt.Equal(paidAt) || got.ProviderStatus != "CONFIRMED" {
		t.Fatalf("cobrança paga: %+v", got)
	}
	if len(f.store.payments) != 1 {
		t.Fatalf("pagamentos: %d", len(f.store.payments))
	}
	p := f.store.payments[0]
	if *p.ChargeID != c.ID || p.Source != "provider" || p.Method != "provider" || p.Amount.Amount() != 10000 || p.Fee.Amount() != 199 ||
		!p.PaidAt.Equal(paidAt) || p.InvoiceID != inv.ID || p.CustomerID != f.customer {
		t.Fatalf("pagamento: %+v", p)
	}
	updated := f.invoice(inv.ID)
	if updated.Status != billing.Paid || updated.AmountPaid.Amount() != 10000 || !updated.AmountDue.IsZero() {
		t.Fatalf("fatura: %+v", updated)
	}
	if len(f.store.invoiceHistory) != 1 || f.store.invoiceHistory[0].actor.Type != audit.ActorProvider || f.store.invoiceHistory[0].changes[0].To != billing.Paid {
		t.Fatalf("histórico da fatura: %+v", f.store.invoiceHistory)
	}
	if got := f.store.ledgerKinds(); !slices.Equal(got, []string{"payment.confirmed"}) {
		t.Fatalf("lançamentos: %v", got)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.ProviderClearing] != 9801 || net[ledger.AccountsReceivable] != -10000 || net[ledger.ProviderFees] != 199 {
		t.Fatalf("ledger: bruto sai de contas a receber, taxa é despesa: %+v", net)
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePaid}) {
		t.Fatalf("eventos: %v", got)
	}
	if !slices.Equal(f.store.standing, []uuid.UUID{*inv.SubscriptionID}) {
		t.Fatalf("situação da assinatura deve ser atualizada: %v", f.store.standing)
	}
	if w := f.store.webhooks[wid]; w.Status != "processed" || w.Error != "" {
		t.Fatalf("webhook: %+v", w)
	}
	if !f.store.inbox[inboxKey(ConsumerWebhooks, msg)] {
		t.Fatal("mensagem registrada no inbox")
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "payment.applied" || f.store.audits[0].Details["applied"] != int64(10000) {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
}

func TestProcessWebhookPartialProviderPayment(t *testing.T) {
	f := newPayFixture(t)
	inv, _ := awaitingCharge(t, f)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderReceived,
		Charge: providerSnapshot(ProviderReceived, "RECEIVED", 4000, 0, nil)})
	f.process(t, wid)
	if got := f.invoice(inv.ID); got.Status != billing.PartiallyPaid || got.AmountDue.Amount() != 6000 {
		t.Fatalf("fatura parcialmente paga: %s %v", got.Status, got.AmountDue)
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePartiallyPaid}) {
		t.Fatalf("eventos: %v", got)
	}
	if len(f.store.standing) != 0 {
		t.Fatal("pagamento parcial não regulariza a assinatura")
	}
	if !f.store.payments[0].PaidAt.Equal(payNow) {
		t.Fatal("sem data do provedor, vale o instante do processamento")
	}
}

func TestProcessWebhookOverpaymentBecomesCustomerCredit(t *testing.T) {
	f := newPayFixture(t)
	inv, _ := awaitingCharge(t, f)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderReceived,
		Charge: providerSnapshot(ProviderReceived, "RECEIVED", 10350, 199, nil)}) // juros pagos pelo cliente
	f.process(t, wid)
	if got := f.invoice(inv.ID); got.Status != billing.Paid || got.AmountPaid.Amount() != 10000 {
		t.Fatalf("fatura: %+v", got)
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 350 || f.store.credits[0].kind != "overpayment" || *f.store.credits[0].invoiceID != inv.ID {
		t.Fatalf("excedente vira crédito do cliente: %+v", f.store.credits)
	}
	if got := f.store.ledgerKinds(); !slices.Equal(got, []string{"payment.confirmed", "payment.overpayment"}) {
		t.Fatalf("lançamentos: %v", got)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.ProviderClearing] != 10350-199 || net[ledger.CustomerCredits] != -350 || net[ledger.AccountsReceivable] != -10000 {
		t.Fatalf("ledger do excedente via provedor: %+v", net)
	}
}

func TestProcessWebhookUsesChargeAmountWhenProviderAmountIsMissing(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: &ProviderCharge{ProviderChargeID: "pay_1", Status: ProviderConfirmed, RawStatus: "CONFIRMED", Amount: money.Money{}}})
	f.process(t, wid)
	if got := f.store.charges[c.ID]; got.AmountPaid.Amount() != 10000 || !got.Fee.IsZero() {
		t.Fatalf("valor da cobrança como referência: %+v", got)
	}
}

func TestProcessWebhookDeduplicatesByInbox(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)})
	msg := uuid.New()
	f.store.inbox[inboxKey(ConsumerWebhooks, msg)] = true // entregue e processado por outro consumidor

	if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, msg); err != nil {
		t.Fatal(err)
	}
	if len(f.store.payments) != 0 || len(f.store.posted) != 0 || f.store.charges[c.ID].Status != ChargeAwaitingPayment {
		t.Fatal("mensagem repetida não pode reaplicar efeitos")
	}

	f.process(t, wid)
	f.process(t, wid) // webhook já processado: nada a fazer
	if len(f.store.payments) != 1 || len(f.store.posted) != 1 {
		t.Fatalf("efeitos aplicados uma única vez: pagamentos=%d lançamentos=%d", len(f.store.payments), len(f.store.posted))
	}
}

func TestProcessWebhookRepeatedConfirmationDoesNotDuplicatePayment(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	snap := providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)
	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed, Charge: snap}))
	received := providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, nil)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_2", ProviderChargeID: "pay_1", Status: ProviderReceived, Charge: received})
	f.process(t, wid)
	if len(f.store.payments) != 1 || len(f.store.posted) != 1 {
		t.Fatalf("confirmações sucessivas não duplicam: pagamentos=%d", len(f.store.payments))
	}
	if got := f.store.charges[c.ID]; got.Status != ChargePaid || got.ProviderStatus != "RECEIVED" {
		t.Fatalf("situação do provedor atualizada: %+v", got)
	}
	if f.store.webhooks[wid].Status != "processed" {
		t.Fatal("webhook processado")
	}
}

func TestProcessWebhookOldEventDoesNotRegress(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_2", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)}))
	events := len(f.store.events)

	old := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderPending,
		Charge: providerSnapshot(ProviderPending, "PENDING", 10000, 0, nil)})
	f.process(t, old)

	got := f.store.charges[c.ID]
	if got.Status != ChargePaid || got.AmountPaid.Amount() != 10000 {
		t.Fatalf("evento antigo não regride a cobrança: %s", got.Status)
	}
	if got.ProviderStatus != "PENDING" {
		t.Fatalf("a situação recebida é apenas registrada: %q", got.ProviderStatus)
	}
	if len(f.gw.getCalls) != 0 || len(f.store.events) != events || len(f.store.payments) != 1 {
		t.Fatal("evento antigo não consulta o provedor nem gera efeitos")
	}
	if f.store.webhooks[old].Status != "processed" {
		t.Fatal("evento antigo é marcado como processado")
	}
}

func TestProcessWebhookStateJumpQueriesProvider(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	paidAt := payNow.Add(time.Hour)
	f.gw.getResult = *providerSnapshot(ProviderReceived, "RECEIVED", 10000, 150, &paidAt)
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_9", ProviderChargeID: "pay_1", Status: ProviderRefunded,
		Charge: providerSnapshot(ProviderRefunded, "REFUNDED", 10000, 999, nil)})

	f.process(t, wid)

	if !slices.Equal(f.gw.getCalls, []string{"pay_1"}) {
		t.Fatalf("salto de estado deve consultar o provedor: %v", f.gw.getCalls)
	}
	got := f.store.charges[c.ID]
	if got.Status != ChargePaid || got.Fee.Amount() != 150 || got.ProviderStatus != "RECEIVED" {
		t.Fatalf("aplica a situação autoritativa do provedor, não a do evento: %+v", got)
	}
	if len(f.resolver.accounts) == 0 || f.resolver.accounts[len(f.resolver.accounts)-1].ID != f.account.ID {
		t.Fatal("consulta usa a conta da cobrança")
	}
}

func TestProcessWebhookWithoutSnapshotQueriesProvider(t *testing.T) {
	cases := map[string]WebhookEvent{
		"evento sem dados da cobrança": {EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed},
		"situação desconhecida": {EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderUnknownStatus,
			Charge: providerSnapshot(ProviderUnknownStatus, "???", 10000, 0, nil)},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPayFixture(t)
			_, c := awaitingCharge(t, f)
			f.gw.getResult = *providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)
			f.process(t, f.storeWebhook(t, ev))
			if len(f.gw.getCalls) != 1 || f.store.charges[c.ID].Status != ChargePaid {
				t.Fatalf("consulta: %v situação: %s", f.gw.getCalls, f.store.charges[c.ID].Status)
			}
		})
	}
}

func TestProcessWebhookProviderQueryFailureKeepsMessageForRetry(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	f.gw.getErr = errAmbiguous
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed})
	msg := uuid.New()
	err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, msg)
	if err == nil || !errors.Is(err, errAmbiguous) {
		t.Fatalf("falha na consulta deve ser devolvida para nova entrega: %v", err)
	}
	if f.store.inbox[inboxKey(ConsumerWebhooks, msg)] || f.store.webhooks[wid].Status != "received" || f.store.charges[c.ID].Status != ChargeAwaitingPayment {
		t.Fatal("sem consulta não há efeito nem registro no inbox")
	}
	f.resolver.gwErr = errors.New("sem gateway")
	if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, msg); !errors.Is(err, f.resolver.gwErr) {
		t.Fatalf("gateway indisponível: %v", err)
	}
}

func TestProcessWebhookCancellationOfPaidChargeIsIgnoredAsConflict(t *testing.T) {
	f := newPayFixture(t)
	_, c := awaitingCharge(t, f)
	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)}))
	events := len(f.store.events)

	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_2", ProviderChargeID: "pay_1", Status: ProviderCanceled,
		Charge: providerSnapshot(ProviderCanceled, "DELETED", 10000, 0, nil)})
	f.process(t, wid)

	w := f.store.webhooks[wid]
	if w.Status != "ignored" || !strings.Contains(w.Error, "provider_state_conflict") {
		t.Fatalf("conflito registrado para a conciliação: %+v", w)
	}
	if got := f.store.charges[c.ID]; got.Status != ChargePaid || got.ProviderStatus != "CONFIRMED" {
		t.Fatalf("cobrança paga não é cancelada: %+v", got)
	}
	if len(f.store.events) != events {
		t.Fatal("conflito não gera eventos")
	}
}

func TestProcessWebhookFailureAndCancellation(t *testing.T) {
	cases := []struct {
		status ProviderStatus
		raw    string
		want   ChargeStatus
	}{
		{ProviderFailed, "REFUSED", ChargeFailed},
		{ProviderCanceled, "DELETED", ChargeCanceled},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			f := newPayFixture(t)
			inv, ch := awaitingCharge(t, f)
			f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: c.status,
				Charge: providerSnapshot(c.status, c.raw, 10000, 0, nil)}))
			got := f.store.charges[ch.ID]
			if got.Status != c.want || got.ProviderStatus != c.raw {
				t.Fatalf("cobrança: %+v", got)
			}
			if c.status == ProviderFailed && got.FailureReason != "recusada pelo provedor" {
				t.Fatalf("motivo: %q", got.FailureReason)
			}
			if !slices.Equal(f.store.eventTypes(), []string{outbox.EventChargeFailed}) {
				t.Fatalf("eventos: %v", f.store.eventTypes())
			}
			if p := f.store.events[0].Payload.(map[string]any); p["provider_status"] != c.raw || p["invoice_id"] != inv.ID {
				t.Fatalf("payload: %+v", p)
			}
			if f.invoice(inv.ID).Status != billing.Open {
				t.Fatal("falha da cobrança não altera a fatura")
			}

			// Repetição da mesma situação não gera novo evento.
			f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_2", ProviderChargeID: "pay_1", Status: c.status,
				Charge: providerSnapshot(c.status, c.raw, 10000, 0, nil)}))
			if len(f.store.events) != 1 {
				t.Fatalf("situação final repetida: %v", f.store.eventTypes())
			}
		})
	}
}

func TestProcessWebhookIgnoredCases(t *testing.T) {
	cases := []struct {
		name   string
		ev     WebhookEvent
		reason string
	}{
		{"cobrança desconhecida", WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_inexistente", Status: ProviderConfirmed}, "cobrança não encontrada localmente"},
		{"evento sem cobrança", WebhookEvent{EventID: "evt_1", Type: "ACCOUNT_STATUS"}, "cobrança não encontrada localmente"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			awaitingCharge(t, f)
			wid := f.storeWebhook(t, c.ev)
			msg := f.process(t, wid)
			if w := f.store.webhooks[wid]; w.Status != "ignored" || w.Error != c.reason {
				t.Fatalf("webhook: %+v", w)
			}
			if !f.store.inbox[inboxKey(ConsumerWebhooks, msg)] || len(f.gw.getCalls) != 0 || len(f.store.payments) != 0 {
				t.Fatal("evento ignorado é registrado sem efeitos")
			}
		})
	}
}

func TestProcessWebhookErrors(t *testing.T) {
	t.Run("webhook já processado", func(t *testing.T) {
		f := newPayFixture(t)
		awaitingCharge(t, f)
		wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed})
		w := f.store.webhooks[wid]
		w.Status = "processed"
		f.store.webhooks[wid] = w
		msg := uuid.New()
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, msg); err != nil {
			t.Fatal(err)
		}
		if f.store.inbox[inboxKey(ConsumerWebhooks, msg)] || len(f.gw.getCalls) != 0 {
			t.Fatal("nada a fazer")
		}
	})
	t.Run("webhook inexistente", func(t *testing.T) {
		f := newPayFixture(t)
		err := f.svc.ProcessWebhook(context.Background(), f.tenantID, uuid.New(), uuid.New())
		wantAppErr(t, err, apperr.KindNotFound, "webhook_not_found")
	})
	t.Run("verificador indisponível", func(t *testing.T) {
		f := newPayFixture(t)
		wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1"})
		f.resolver.verifierErr = errors.New("sem verificador")
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, f.resolver.verifierErr) {
			t.Fatalf("erro: %v", err)
		}
	})
	t.Run("corpo ilegível", func(t *testing.T) {
		f := newPayFixture(t)
		wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1"})
		f.verifier.parseErr = errors.New("json inválido")
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, f.verifier.parseErr) {
			t.Fatalf("erro: %v", err)
		}
	})
	for _, m := range []string{"ChargeByProviderID", "MarkInbox", "UpdateWebhookStatus", "InsertPayment", "Invoice", "UpdateInvoiceProgress",
		"InsertInvoiceHistory", "PostLedger", "RefreshSubscriptionStanding", "Emit", "Audit", "UpdateCharge"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := awaitingCharge(t, f)
			wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
				Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)})
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if f.store.charges[c.ID].Status != ChargeAwaitingPayment || f.invoice(inv.ID).Status != billing.Open || len(f.store.payments) != 0 ||
				len(f.store.inbox) != 0 || f.store.webhooks[wid].Status != "received" {
				t.Fatal("falha desfaz todos os efeitos para nova entrega")
			}
		})
	}
	t.Run("falha ao localizar a conta da cobrança", func(t *testing.T) {
		f := newPayFixture(t)
		awaitingCharge(t, f)
		wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed})
		boom := errors.New("conta indisponível")
		f.store.errs["Account"] = boom
		if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
			t.Fatalf("erro: %v", err)
		}
	})
}

func TestNeedsAuthoritativeState(t *testing.T) {
	snap := &ProviderCharge{}
	cases := []struct {
		local ChargeStatus
		ev    ProviderStatus
		nilEv bool
		want  bool
	}{
		{ChargeAwaitingPayment, ProviderConfirmed, true, true}, // sem dados da cobrança
		{ChargeAwaitingPayment, ProviderUnknownStatus, false, true},
		{ChargeAwaitingPayment, ProviderConfirmed, false, false},
		{ChargePending, ProviderReceived, false, false},
		{ChargeAwaitingPayment, ProviderFailed, false, false},
		{ChargeAwaitingPayment, ProviderOverdue, false, false},
		{ChargeAwaitingPayment, ProviderRefundInProgress, false, true}, // reembolso antes da confirmação
		{ChargeAwaitingPayment, ProviderChargebackRequested, false, true},
		{ChargePaid, ProviderPending, false, false}, // evento antigo
		{ChargePaid, ProviderRefundInProgress, false, false},
		{ChargePaid, ProviderPartiallyRefunded, false, false},
		{ChargePaid, ProviderRefunded, false, true},
		{ChargePartiallyRefunded, ProviderRefunded, false, false},
		{ChargeRefunded, ProviderPartiallyRefunded, false, false},
		{ChargeDisputed, ProviderChargebackLost, false, false},
		{ChargeChargedBack, ProviderConfirmed, false, false},
		{ChargeFailed, ProviderConfirmed, false, false},
		{ChargeFailed, ProviderRefunded, false, true},
		{ChargeCanceled, ProviderReceived, false, false},
		{ChargeAwaitingPayment, ProviderStatus("nao_mapeado"), false, false},
	}
	for _, c := range cases {
		ev := WebhookEvent{Status: c.ev, Charge: snap}
		if c.nilEv {
			ev.Charge = nil
		}
		if got := needsAuthoritativeState(Charge{Status: c.local}, ev); got != c.want {
			t.Errorf("local=%s evento=%s sem_dados=%v: consulta=%v, esperado %v", c.local, c.ev, c.nilEv, got, c.want)
		}
	}
}

func TestProcessWebhookOverpaymentFailureRollsBack(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	boom := errors.New("falha ao conceder crédito")
	f.store.errs["GrantCustomerCredit"] = boom
	wid := f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderReceived,
		Charge: providerSnapshot(ProviderReceived, "RECEIVED", 10500, 0, nil)})
	if err := f.svc.ProcessWebhook(context.Background(), f.tenantID, wid, uuid.New()); !errors.Is(err, boom) {
		t.Fatalf("esperava %v, obtive %v", boom, err)
	}
	if f.invoice(inv.ID).Status != billing.Open || f.store.charges[c.ID].Status != ChargeAwaitingPayment || len(f.store.posted) != 0 {
		t.Fatal("sem o crédito do excedente o pagamento não é aplicado")
	}
}

func TestProviderPaymentForAlreadyPaidInvoiceBecomesCredit(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
		ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(10000), Method: "pix", RequestKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	f.store.posted, f.store.events, f.store.audits = nil, nil, nil

	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)}))

	if f.store.charges[c.ID].Status != ChargePaid || len(f.store.payments) != 2 {
		t.Fatal("o recebimento no provedor é registrado")
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 10000 || f.store.credits[0].kind != "overpayment" {
		t.Fatalf("pagamento de fatura já quitada vira crédito integral: %+v", f.store.credits)
	}
	if got := f.store.ledgerKinds(); len(got) == 0 || got[0] != "payment.overpayment" {
		t.Fatalf("lançamentos: %v", got)
	}
	if f.store.audits[0].Action != "payment.overpayment_credited" {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	if f.invoice(inv.ID).AmountPaid.Amount() != 10000 {
		t.Fatal("fatura já quitada não muda")
	}
}

func TestProviderPaymentForAlreadyPaidInvoiceRecognizesFee(t *testing.T) {
	f := newPayFixture(t)
	inv, _ := awaitingCharge(t, f)
	if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
		ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(10000), Method: "pix", RequestKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	f.store.posted = nil
	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 199, nil)}))
	net := ledgerNet(f.store.posted)
	if net[ledger.ProviderFees] != 199 || net[ledger.ProviderClearing] != 10000-199 {
		t.Fatalf("a taxa do provedor deve ser reconhecida também neste caminho: %+v", net)
	}
}

func TestProviderRecoveryOfUncollectibleInvoiceReversesBadDebt(t *testing.T) {
	f := newPayFixture(t)
	inv, _ := awaitingCharge(t, f)
	stored := f.invoice(inv.ID)
	written, err := stored.MarkUncollectible("cliente não localizado", payNow)
	if err != nil {
		t.Fatal(err)
	}
	stored.PopChanges()
	f.store.invoices[inv.ID] = stored
	issuedTx, _, _ := ledger.InvoiceIssued(uuid.New(), f.tenantID, inv.ID, inv.Number, inv.Total, inv.CreditApplied, payNow)
	writeOffTx, _, _ := ledger.InvoiceWrittenOff(uuid.New(), f.tenantID, inv.ID, written, payNow)
	f.store.posted = []ledger.Transaction{issuedTx, writeOffTx}

	f.process(t, f.storeWebhook(t, WebhookEvent{EventID: "evt_1", ProviderChargeID: "pay_1", Status: ProviderConfirmed,
		Charge: providerSnapshot(ProviderConfirmed, "CONFIRMED", 10000, 0, nil)}))

	net := ledgerNet(f.store.posted)
	if net[ledger.AccountsReceivable] != 0 || net[ledger.BadDebt] != 0 {
		t.Fatalf("recuperação integral deve zerar contas a receber e a perda: %+v", net)
	}
}

// Pagamentos manuais.

func TestRecordManualPaymentValidation(t *testing.T) {
	cases := []struct {
		name  string
		in    ManualPaymentInput
		field string
	}{
		{"valor zero", ManualPaymentInput{Amount: brl(0), Method: "pix"}, "amount"},
		{"valor negativo", ManualPaymentInput{Amount: brl(-1), Method: "pix"}, "amount"},
		{"valor acima do limite", ManualPaymentInput{Amount: brl(money.MaxAmount + 1), Method: "pix"}, "amount"},
		{"método desconhecido", ManualPaymentInput{Amount: brl(100), Method: "cheque"}, "method"},
		{"data no futuro", ManualPaymentInput{Amount: brl(100), Method: "pix", PaidAt: payNow.Add(time.Second)}, "paid_at"},
		{"observação longa", ManualPaymentInput{Amount: brl(100), Method: "pix", Note: strings.Repeat("n", 501)}, "note"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			c.in.InvoiceID = inv.ID
			_, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, c.in)
			wantAppErr(t, err, apperr.KindInvalid, "invalid_payment")
			e, _ := apperr.As(err)
			if len(e.Fields) != 1 || e.Fields[0].Field != c.field {
				t.Fatalf("campo: %+v", e.Fields)
			}
			if f.store.txCount != 0 {
				t.Fatal("validação ocorre antes da transação")
			}
		})
	}
	for _, m := range []string{"pix", "bank_transfer", "boleto", "cash", "other"} {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
			ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(100), Method: m, Note: "   " + strings.Repeat("ç", 500) + "  "}); err != nil {
			t.Fatalf("método %s e observação de 500 caracteres após aparar: %v", m, err)
		}
	}
}

func TestRecordManualPaymentPartial(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	paidAt := payNow.Add(-24 * time.Hour)
	pay, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, ManualPaymentInput{
		InvoiceID: inv.ID, Amount: brl(4000), PaidAt: paidAt, Method: "bank_transfer", Note: "  TED identificada no extrato ", RequestKey: "user:1|POST|k1"})
	if err != nil {
		t.Fatal(err)
	}
	if pay.Source != "manual" || pay.Method != "bank_transfer" || pay.ChargeID != nil || !pay.Fee.IsZero() || *pay.RecordedBy != f.userID ||
		pay.Note != "TED identificada no extrato" || !pay.PaidAt.Equal(paidAt) || pay.RequestKey != "user:1|POST|k1" || pay.CustomerID != f.customer {
		t.Fatalf("pagamento manual: %+v", pay)
	}
	if got := f.invoice(inv.ID); got.Status != billing.PartiallyPaid || got.AmountDue.Amount() != 6000 {
		t.Fatalf("fatura: %s %v", got.Status, got.AmountDue)
	}
	if got := f.store.ledgerKinds(); !slices.Equal(got, []string{"payment.manual"}) {
		t.Fatalf("lançamentos: %v", got)
	}
	if net := ledgerNet(f.store.posted); net[ledger.Cash] != 4000 || net[ledger.AccountsReceivable] != -4000 {
		t.Fatalf("ledger do recebimento manual: %+v", net)
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePartiallyPaid}) {
		t.Fatalf("eventos: %v", got)
	}
	if len(f.store.invoiceHistory) != 1 || f.store.invoiceHistory[0].actor != f.actor {
		t.Fatalf("histórico registra o usuário: %+v", f.store.invoiceHistory)
	}
}

func TestRecordManualPaymentDefaultsPaidAtToNow(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	pay, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, nil, ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(100), Method: "cash"})
	if err != nil || !pay.PaidAt.Equal(payNow) || pay.RecordedBy != nil {
		t.Fatalf("data padrão: %+v %v", pay, err)
	}
}

func TestRecordManualPaymentRepeatedRequestKeyReturnsExisting(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	in := ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(4000), Method: "pix", RequestKey: "user:1|POST|k1"}
	first, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	again, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || len(f.store.payments) != 1 || len(f.store.posted) != 1 {
		t.Fatalf("mesma chave devolve o pagamento existente sem reaplicar: %v %v", first.ID, again.ID)
	}
	if got := f.invoice(inv.ID); got.AmountPaid.Amount() != 4000 {
		t.Fatalf("valor pago não pode dobrar: %v", got.AmountPaid)
	}
}

func TestRecordManualPaymentRepeatedRequestKeyAfterFullPayment(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	in := ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(10000), Method: "pix", RequestKey: "user:1|POST|k1"}
	first, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("retentativa após quitação deve devolver o pagamento existente: %v %v", again.ID, err)
	}
}

func TestRecordManualPaymentExcessBecomesCredit(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
		ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(12500), Method: "pix"}); err != nil {
		t.Fatal(err)
	}
	if got := f.invoice(inv.ID); got.Status != billing.Paid || got.AmountPaid.Amount() != 10000 {
		t.Fatalf("fatura: %+v", got)
	}
	if len(f.store.credits) != 1 || f.store.credits[0].amount.Amount() != 2500 || f.store.credits[0].customerID != f.customer || f.store.credits[0].kind != "overpayment" {
		t.Fatalf("crédito do excedente: %+v", f.store.credits)
	}
	if got := f.store.ledgerKinds(); !slices.Equal(got, []string{"payment.manual", "payment.overpayment"}) {
		t.Fatalf("lançamentos: %v", got)
	}
	net := ledgerNet(f.store.posted)
	if net[ledger.Cash] != 12500 || net[ledger.CustomerCredits] != -2500 || net[ledger.AccountsReceivable] != -10000 {
		t.Fatalf("ledger: %+v", net)
	}
	if !slices.Equal(f.store.standing, []uuid.UUID{*inv.SubscriptionID}) {
		t.Fatal("quitação regulariza a assinatura")
	}
	if got := f.store.eventTypes(); !slices.Equal(got, []string{outbox.EventPaymentConfirmed, outbox.EventInvoicePaid}) {
		t.Fatalf("eventos: %v", got)
	}
}

func TestRecordManualPaymentOnUncollectibleInvoice(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	stored := f.invoice(inv.ID)
	if _, err := stored.MarkUncollectible("cliente não localizado", payNow); err != nil {
		t.Fatal(err)
	}
	stored.PopChanges()
	f.store.invoices[inv.ID] = stored
	if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
		ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(10000), Method: "pix"}); err != nil {
		t.Fatalf("fatura incobrável aceita recebimento: %v", err)
	}
	if got := f.invoice(inv.ID); got.Status != billing.Paid || !got.AmountWrittenOff.IsZero() {
		t.Fatalf("baixa revertida: %+v", got)
	}
}

func TestRecordManualPaymentRules(t *testing.T) {
	for _, st := range []billing.Status{billing.Paid, billing.Void, billing.Draft} {
		t.Run("fatura "+string(st), func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			inv.Status = st
			f.store.invoices[inv.ID] = inv
			_, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
				ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(100), Method: "pix"})
			wantAppErr(t, err, apperr.KindConflict, "invoice_not_payable")
			if len(f.store.payments) != 0 {
				t.Fatal("nenhum pagamento registrado")
			}
		})
	}
	for _, m := range []string{"Invoice", "InsertPayment", "PaymentByRequestKey", "GrantCustomerCredit"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			in := ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(15000), Method: "pix", RequestKey: "k"}
			if m == "PaymentByRequestKey" {
				if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID,
					ManualPaymentInput{InvoiceID: inv.ID, Amount: brl(100), Method: "pix", RequestKey: "k"}); err != nil {
					t.Fatal(err)
				}
			}
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			before := len(f.store.payments)
			if _, err := f.svc.RecordManualPayment(context.Background(), f.tenantID, f.actor, &f.userID, in); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.payments) != before || len(f.store.credits) != 0 {
				t.Fatal("falha desfaz o registro")
			}
		})
	}
}

// Consumidores.

func TestCancelOpenCharges(t *testing.T) {
	f := newPayFixture(t)
	inv, c := awaitingCharge(t, f)
	pending, _ := NewCharge(uuid.New(), f.tenantID, inv.ID, f.customer, f.account.ID, "simulator", 2, brl(10000), c.DueDate, payNow)
	f.store.charges[pending.ID] = pending
	f.store.chargeOrder = append(f.store.chargeOrder, pending.ID)
	msg := uuid.New()

	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{c.ID, pending.ID} {
		if got := f.store.charges[id]; got.Status != ChargeCanceled || !got.UpdatedAt.Equal(payNow) {
			t.Fatalf("cobrança em aberto deve ser cancelada: %+v", got)
		}
	}
	if !f.store.inbox[inboxKey("payments.invoice_voided", msg)] {
		t.Fatal("mensagem registrada no inbox")
	}
	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatalf("mensagem repetida: %v", err)
	}
}

func TestCancelOpenChargesFailures(t *testing.T) {
	for _, m := range []string{"MarkInbox", "OpenChargesForInvoice", "UpdateCharge"} {
		t.Run(m, func(t *testing.T) {
			f := newPayFixture(t)
			inv, c := awaitingCharge(t, f)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New()); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if f.store.charges[c.ID].Status != ChargeAwaitingPayment || len(f.store.inbox) != 0 {
				t.Fatal("falha desfaz a transação")
			}
		})
	}
	t.Run("cobrança listada como aberta em situação final", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := awaitingCharge(t, f)
		stale := paidCharge(t, 10000)
		stale.TenantID, stale.InvoiceID = f.tenantID, inv.ID
		f.store.extraOpen = []Charge{stale}
		err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, inv.ID, uuid.New())
		wantAppErr(t, err, apperr.KindConflict, "invalid_charge_transition")
		if f.store.charges[c.ID].Status != ChargeAwaitingPayment || len(f.store.inbox) != 0 {
			t.Fatal("transição inválida desfaz os cancelamentos já feitos na transação")
		}
	})
}

func TestHandleInvoiceIssued(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	msg := uuid.New()
	if err := f.svc.HandleInvoiceIssued(context.Background(), f.tenantID, inv.ID, msg); err != nil {
		t.Fatal(err)
	}
	if len(f.store.charges) != 1 || !f.store.inbox[inboxKey(ConsumerInvoiceIssued, msg)] {
		t.Fatal("cobrança criada e mensagem registrada no inbox")
	}
	for _, a := range f.store.audits {
		if a.Actor.Type != audit.ActorSystem {
			t.Fatalf("consumidor age como sistema: %+v", a.Actor)
		}
	}
	// Reentrega: a cobrança é idempotente por fatura.
	if err := f.svc.HandleInvoiceIssued(context.Background(), f.tenantID, inv.ID, msg); err != nil || len(f.store.charges) != 1 {
		t.Fatalf("reentrega: %v %d", err, len(f.store.charges))
	}

	g := newPayFixture(t)
	boom := errors.New("banco indisponível")
	g.store.errs["Invoice"] = boom
	if err := g.svc.HandleInvoiceIssued(context.Background(), g.tenantID, uuid.New(), msg); !errors.Is(err, boom) {
		t.Fatalf("falha na criação: %v", err)
	}
	if len(g.store.inbox) != 0 {
		t.Fatal("sem cobrança, a mensagem não é marcada")
	}
	h := newPayFixture(t)
	hinv := h.addInvoice(t, 10000)
	h.store.errs["MarkInbox"] = boom
	if err := h.svc.HandleInvoiceIssued(context.Background(), h.tenantID, hinv.ID, msg); !errors.Is(err, boom) {
		t.Fatalf("falha no inbox: %v", err)
	}
	if len(h.store.charges) != 0 || len(h.gw.createCalls) != 0 {
		t.Fatal("sem inbox, nenhuma cobrança é gravada nem solicitada")
	}
	// Falha depois da marcação desfaz também o inbox: a reentrega cria a cobrança.
	k := newPayFixture(t)
	kinv := k.addInvoice(t, 10000)
	k.store.errs["InsertOperation"] = boom
	if err := k.svc.HandleInvoiceIssued(context.Background(), k.tenantID, kinv.ID, msg); !errors.Is(err, boom) {
		t.Fatalf("falha na gravação da operação: %v", err)
	}
	if len(k.store.inbox) != 0 || len(k.store.charges) != 0 {
		t.Fatal("falha desfaz o inbox e a cobrança")
	}
	delete(k.store.errs, "InsertOperation")
	if err := k.svc.HandleInvoiceIssued(context.Background(), k.tenantID, kinv.ID, msg); err != nil || len(k.store.charges) != 1 {
		t.Fatalf("reentrega após falha: %v %d", err, len(k.store.charges))
	}
}

// Regressão: o inbox era marcado depois da cobrança, numa transação própria,
// e a reentrega depois de uma recusa do provedor chamava RequestCharge, que
// criava a tentativa 2 no provedor. A reentrega da mensagem já registrada não
// faz nada, depois de recusa ou de sucesso; uma mensagem nova (outro evento)
// continua seguindo a regra da cobrança.
func TestHandleInvoiceIssuedRedeliveryCreatesNoCharge(t *testing.T) {
	for name, results := range map[string][]gwResult{"depois de recusa": {{err: errPermanent}}, "depois de sucesso": nil} {
		t.Run(name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = results
			msg := uuid.New()
			if err := f.svc.HandleInvoiceIssued(context.Background(), f.tenantID, inv.ID, msg); err != nil {
				t.Fatal(err)
			}
			first := f.chargesOf(inv.ID)
			if len(first) != 1 || len(f.gw.createCalls) != 1 {
				t.Fatalf("primeira entrega: %+v", first)
			}
			for range 3 {
				if err := f.svc.HandleInvoiceIssued(context.Background(), f.tenantID, inv.ID, msg); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.chargesOf(inv.ID); len(got) != 1 || got[0].Status != first[0].Status || len(f.gw.createCalls) != 1 {
				t.Fatalf("reentrega criou cobrança: %+v, %d criações", got, len(f.gw.createCalls))
			}
		})
	}
}

func TestRetryChargeAndGetters(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	c, err := f.svc.RetryCharge(context.Background(), f.tenantID, f.actor, inv.ID)
	if err != nil || c == nil || c.Status != ChargeAwaitingPayment {
		t.Fatalf("RetryCharge: %+v %v", c, err)
	}
	got, err := f.svc.Charge(context.Background(), f.tenantID, c.ID)
	if err != nil || got.ID != c.ID {
		t.Fatalf("Charge: %v", err)
	}

	paid := f.invoice(inv.ID)
	paid.Status, paid.AmountDue = billing.Paid, brl(0)
	f.store.invoices[inv.ID] = paid
	_, err = f.svc.RetryCharge(context.Background(), f.tenantID, f.actor, inv.ID)
	wantAppErr(t, err, apperr.KindConflict, "charge_not_created")

	f.store.errs["Invoice"] = errors.New("banco indisponível")
	if _, err := f.svc.RetryCharge(context.Background(), f.tenantID, f.actor, inv.ID); !errors.Is(err, f.store.errs["Invoice"]) {
		t.Fatalf("erro propagado: %v", err)
	}
}
