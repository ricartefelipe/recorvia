package payments

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// pendingNeverClaimed grava a cobrança com a operação pendente e nunca
// reivindicada (attempts = 0), como num banco restaurado de backup em que a
// chamada ao provedor aconteceu depois do instante da cópia.
func pendingNeverClaimed(t *testing.T, f *payFixture) (Charge, Operation) {
	t.Helper()
	inv := f.addInvoice(t, 10000)
	f.store.errs["CustomerData"] = errors.New("banco indisponível")
	ch := f.requestCharge(t, inv.ID)
	delete(f.store.errs, "CustomerData")
	op := f.operationFor(t, ch.ID)
	if op.Status != OpPending || op.Attempts != 0 || len(f.gw.createCalls) != 0 {
		t.Fatalf("pré-condição: operação pendente sem tentativa: %+v", op)
	}
	return ch, op
}

// A recuperação nunca cria às cegas: mesmo sem tentativa registrada, a
// operação pode ter sido executada fora do registro local.
func TestRecoveryLooksUpProviderBeforeFirstAttempt(t *testing.T) {
	cases := []struct {
		name        string
		existing    bool
		wantCreates int
	}{
		{"cobrança já criada no provedor é vinculada", true, 0},
		{"cobrança inexistente é criada com a mesma referência", false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			ch, op := pendingNeverClaimed(t, f)
			if c.existing {
				f.gw.findResult = &ProviderCharge{ProviderChargeID: "pay_janela", ExternalReference: ch.ID.String(), ProviderCustomerID: "cus_1",
					Status: ProviderPending, RawStatus: "PENDING", Amount: brl(10000), CheckoutURL: "https://sandbox.exemplo/c/pay_janela"}
			}
			r, err := f.svc.RecoverOperations(context.Background())
			if err != nil || r.Executed != 1 {
				t.Fatalf("recuperação: %+v %v", r, err)
			}
			if len(f.gw.findCalls) != 1 || f.gw.findCalls[0] != ch.ID.String() {
				t.Fatalf("a primeira tentativa da recuperação consulta pela referência externa: %v", f.gw.findCalls)
			}
			if len(f.gw.createCalls) != c.wantCreates {
				t.Fatalf("criações no provedor: %d, esperadas %d", len(f.gw.createCalls), c.wantCreates)
			}
			if c.wantCreates == 1 && f.gw.createCalls[0].ExternalReference != ch.ID.String() {
				t.Fatalf("a criação usa a referência da cobrança local: %q", f.gw.createCalls[0].ExternalReference)
			}
			got := f.store.charges[ch.ID]
			if got.Status != ChargeAwaitingPayment || (c.existing && got.ProviderChargeID != "pay_janela") {
				t.Fatalf("cobrança: %+v", got)
			}
			if done := f.store.operations[op.ID]; done.Status != OpSucceeded || done.Attempts != 1 {
				t.Fatalf("operação: %+v", done)
			}
		})
	}
}

// A criação logo após gravar a operação (mesmo fluxo, sem chamada anterior
// possível) não consulta o provedor.
func TestImmediateExecutionCreatesWithoutLookup(t *testing.T) {
	f := newPayFixture(t)
	inv := f.addInvoice(t, 10000)
	f.requestCharge(t, inv.ID)
	if len(f.gw.findCalls) != 0 || len(f.gw.createCalls) != 1 {
		t.Fatalf("consultas=%d criações=%d", len(f.gw.findCalls), len(f.gw.createCalls))
	}
}

// A reexecução manual antecipa a tentativa agendada e consulta o provedor
// antes de criar; a recuperação agendada espera next_attempt_at.
func TestRetryOperationAnticipatesNextAttempt(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *payFixture) (Charge, Operation)
	}{
		{"resultado desconhecido antes do backoff", func(t *testing.T, f *payFixture) (Charge, Operation) {
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: errAmbiguous}}
			ch := f.requestCharge(t, inv.ID)
			return ch, f.operationFor(t, ch.ID)
		}},
		{"pendente antes do backoff", func(t *testing.T, f *payFixture) (Charge, Operation) {
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: errRetryable}}
			ch := f.requestCharge(t, inv.ID)
			return ch, f.operationFor(t, ch.ID)
		}},
		{"pendente sem tentativa", pendingNeverClaimed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			ch, op := c.setup(t, f)
			creates := len(f.gw.createCalls)
			if op.Attempts > 0 && !op.NextAttemptAt.After(f.clock.Now()) {
				t.Fatalf("pré-condição: próxima tentativa no futuro: %+v", op)
			}
			if op.Attempts > 0 {
				if err := f.svc.ExecuteOperation(context.Background(), f.tenantID, op.ID); err != nil || len(f.gw.findCalls) != 0 {
					t.Fatalf("a recuperação agendada respeita o backoff: %v %v", err, f.gw.findCalls)
				}
			}
			f.gw.findResult = &ProviderCharge{ProviderChargeID: "pay_77", ExternalReference: ch.ID.String(), ProviderCustomerID: "cus_9",
				Status: ProviderPending, RawStatus: "PENDING", Amount: brl(10000), CheckoutURL: "https://sandbox.exemplo/c/pay_77"}
			if err := f.svc.RetryOperation(context.Background(), f.tenantID, op.ID); err != nil {
				t.Fatal(err)
			}
			if len(f.gw.findCalls) != 1 || f.gw.findCalls[0] != ch.ID.String() {
				t.Fatalf("a reexecução consulta pela referência externa: %v", f.gw.findCalls)
			}
			if len(f.gw.createCalls) != creates {
				t.Fatal("a cobrança encontrada não pode ser criada de novo")
			}
			if got := f.store.charges[ch.ID]; got.Status != ChargeAwaitingPayment || got.ProviderChargeID != "pay_77" {
				t.Fatalf("cobrança vinculada: %+v", got)
			}
			if done := f.store.operations[op.ID]; done.Status != OpSucceeded || done.Attempts != op.Attempts+1 {
				t.Fatalf("operação: %+v", done)
			}
		})
	}
}

// Sem nada a executar, a reexecução manual responde com conflito em vez de
// sucesso silencioso, e nada é enviado ao provedor.
func TestRetryOperationRejectsWhatCannotRun(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(op *Operation, now time.Time)
		kind   apperr.Kind
		code   string
	}{
		{"concluída", func(op *Operation, _ time.Time) { op.Status = OpSucceeded }, apperr.KindConflict, "operation_completed"},
		{"esgotada", func(op *Operation, _ time.Time) { op.Status, op.Attempts = OpUnknown, MaxOperationAttempts },
			apperr.KindConflict, "operation_attempts_exhausted"},
		{"em execução com lease vigente", func(op *Operation, now time.Time) {
			lease := now.Add(time.Minute)
			op.Status, op.LeaseUntil = OpInFlight, &lease
		}, apperr.KindConflict, "operation_in_progress"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			_, op := pendingNeverClaimed(t, f)
			c.mutate(&op, f.clock.Now())
			f.store.operations[op.ID] = op
			err := f.svc.RetryOperation(context.Background(), f.tenantID, op.ID)
			wantAppErr(t, err, c.kind, c.code)
			if len(f.gw.findCalls)+len(f.gw.createCalls) != 0 {
				t.Fatal("nenhuma chamada ao provedor")
			}
			if got := f.store.operations[op.ID]; got.Attempts != op.Attempts || got.Status != op.Status {
				t.Fatalf("operação inalterada: %+v", got)
			}
		})
	}
	t.Run("inexistente", func(t *testing.T) {
		f := newPayFixture(t)
		wantAppErr(t, f.svc.RetryOperation(context.Background(), f.tenantID, uuid.New()), apperr.KindNotFound, "operation_not_found")
	})
}

// exhaustedPending leva a criação da cobrança a esgotar as tentativas com
// recusas transitórias do provedor: a operação fica pendente no limite, fora
// da recuperação agendada.
func exhaustedPending(t *testing.T, f *payFixture) (Charge, Operation) {
	t.Helper()
	inv := f.addInvoice(t, 10000)
	for range MaxOperationAttempts {
		f.gw.createResults = append(f.gw.createResults, gwResult{err: errRetryable})
	}
	ch := f.requestCharge(t, inv.ID)
	for range 2 * MaxOperationAttempts {
		f.clock.Advance(time.Hour)
		if _, err := f.svc.RecoverOperations(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	op := f.operationFor(t, ch.ID)
	if op.Status != OpPending || op.Attempts != MaxOperationAttempts || op.Claimable(f.clock.Now()) {
		t.Fatalf("pré-condição: operação pendente no limite: %+v", op)
	}
	f.clock.Advance(time.Hour)
	return ch, op
}

// voidInvoice anula a fatura e cancela as cobranças em aberto, como o
// consumidor de invoice.voided.
func (f *payFixture) voidInvoice(t *testing.T, invoiceID uuid.UUID) {
	t.Helper()
	inv := f.invoice(invoiceID)
	if err := inv.Void("Emitida por engano", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	inv.PopChanges()
	f.store.invoices[invoiceID] = inv
	if err := f.svc.CancelOpenCharges(context.Background(), f.tenantID, invoiceID, uuid.New()); err != nil {
		t.Fatal(err)
	}
}

// Falhas repetíveis em todas as tentativas deixam a operação pendente no
// limite, fora da recuperação; a contagem por situação alimenta o alerta.
func TestRecoverOperationsCountsExhaustedOperations(t *testing.T) {
	t.Run("pendente após falhas repetíveis", func(t *testing.T) {
		f := newPayFixture(t)
		ch, op := exhaustedPending(t, f)
		r, err := f.svc.RecoverOperations(context.Background())
		if err != nil || !r.Complete || r.Executed != 0 {
			t.Fatalf("recuperação: %+v %v", r, err)
		}
		if !maps.Equal(r.Exhausted, map[OperationStatus]int{OpPending: 1}) {
			t.Fatalf("esgotadas: %v", r.Exhausted)
		}

		// Corrigida a causa, a reexecução manual retoma a pendente esgotada,
		// consultando o provedor antes de criar.
		finds, creates := len(f.gw.findCalls), len(f.gw.createCalls)
		if err := f.svc.RetryOperation(context.Background(), f.tenantID, op.ID); err != nil {
			t.Fatal(err)
		}
		if len(f.gw.findCalls) != finds+1 || len(f.gw.createCalls) != creates+1 {
			t.Fatalf("consulta e criação: find=%d create=%d", len(f.gw.findCalls)-finds, len(f.gw.createCalls)-creates)
		}
		if got := f.store.charges[ch.ID]; got.Status != ChargeAwaitingPayment {
			t.Fatalf("cobrança criada: %+v", got)
		}
		if r, _ := f.svc.RecoverOperations(context.Background()); len(r.Exhausted) != 0 {
			t.Fatalf("nenhuma esgotada após a reexecução: %v", r.Exhausted)
		}
	})
	t.Run("por situação e tenant", func(t *testing.T) {
		f := newPayFixture(t)
		now := f.clock.Now()
		past, future := now.Add(-time.Minute), now.Add(time.Minute)
		other := uuid.New()
		add := func(tenant uuid.UUID, status OperationStatus, next time.Time, lease *time.Time) {
			op := Operation{ID: uuid.New(), TenantID: tenant, ChargeID: uuid.New(), Kind: OpCreateCharge, Status: status,
				Attempts: MaxOperationAttempts, NextAttemptAt: next, LeaseUntil: lease}
			f.store.operations[op.ID] = op
			f.store.charges[op.ChargeID] = Charge{ID: op.ChargeID, TenantID: tenant, Status: ChargePending}
		}
		add(f.tenantID, OpPending, past, nil)
		add(f.tenantID, OpUnknown, past, nil)
		add(other, OpInFlight, past, &past)
		add(other, OpPending, future, nil)    // ainda antes da próxima tentativa
		add(other, OpInFlight, past, &future) // lease vigente
		r, err := f.svc.RecoverOperations(context.Background())
		if err != nil || !r.Complete || r.Executed != 0 {
			t.Fatalf("recuperação: %+v %v", r, err)
		}
		want := map[OperationStatus]int{OpPending: 1, OpUnknown: 1, OpInFlight: 1}
		if !maps.Equal(r.Exhausted, want) {
			t.Fatalf("esgotadas: %v, esperado %v", r.Exhausted, want)
		}
		if len(f.gw.findCalls)+len(f.gw.createCalls) != 0 {
			t.Fatal("operações esgotadas não são executadas")
		}

		boom := errors.New("contagem indisponível")
		f.store.errs["ExhaustedOperations"] = boom
		r, err = f.svc.RecoverOperations(context.Background())
		if !errors.Is(err, boom) || r.Complete {
			t.Fatalf("falha na contagem torna o resultado parcial: %+v %v", r, err)
		}
	})
}

// Cenário: a criação esgota as tentativas com recusas transitórias e, depois,
// a fatura é anulada. A operação deixa de contar como esgotada (o alerta se
// resolve), e a reexecução manual indicada no runbook só consulta o provedor:
// não cria cobrança para a fatura anulada nem anuncia charge.created; sem
// cobrança no provedor, encerra a operação.
func TestExhaustedCreationOfCanceledChargeIsNotCountedNorCreated(t *testing.T) {
	ctx := context.Background()
	f := newPayFixture(t)
	ch, op := exhaustedPending(t, f)
	f.voidInvoice(t, ch.InvoiceID)
	if got := f.store.charges[ch.ID]; got.Status != ChargeCanceled {
		t.Fatalf("pré-condição: cobrança cancelada com a fatura: %+v", got)
	}
	r, err := f.svc.RecoverOperations(ctx)
	if err != nil || !r.Complete || r.Executed != 0 {
		t.Fatalf("recuperação: %+v %v", r, err)
	}
	if len(r.Exhausted) != 0 {
		t.Fatalf("a criação de cobrança cancelada não conta como esgotada: %v", r.Exhausted)
	}

	finds, creates, events := len(f.gw.findCalls), len(f.gw.createCalls), len(f.store.events)
	if err := f.svc.RetryOperation(ctx, f.tenantID, op.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.gw.createCalls) != creates {
		t.Fatal("nenhuma cobrança é criada no provedor para a fatura anulada")
	}
	if len(f.gw.findCalls) != finds+1 || f.gw.findCalls[finds] != ch.ID.String() {
		t.Fatalf("a reexecução consulta pela referência externa: %v", f.gw.findCalls[finds:])
	}
	if got := f.store.charges[ch.ID]; got.Status != ChargeCanceled || got.ProviderChargeID != "" {
		t.Fatalf("cobrança continua cancelada e sem vínculo: %+v", got)
	}
	done := f.store.operations[op.ID]
	if done.Status != OpFailed || done.CompletedAt == nil || done.LastError != obsoleteOperationError || done.Attempts != op.Attempts+1 {
		t.Fatalf("operação encerrada: %+v", done)
	}
	if len(f.store.events) != events {
		t.Fatalf("nenhum evento: %v", f.store.eventTypes()[events:])
	}
	wantAppErr(t, f.svc.RetryOperation(ctx, f.tenantID, op.ID), apperr.KindConflict, "operation_completed")
}

// Uma criação cuja cobrança foi cancelada (fatura anulada) nunca cria a
// cobrança no provedor, pela recuperação agendada ou pela reexecução manual:
// só consulta pela referência externa, vincula a cobrança encontrada sem
// anunciá-la (um pagamento por ela vira crédito do cliente) e, sem cobrança
// no provedor, encerra a operação.
func TestObsoleteCreationOnlyLooksUpProvider(t *testing.T) {
	open := func(ch Charge) *ProviderCharge {
		return &ProviderCharge{ProviderChargeID: "pay_janela", ExternalReference: ch.ID.String(), ProviderCustomerID: "cus_1",
			Status: ProviderPending, RawStatus: "PENDING", Amount: brl(10000), CheckoutURL: "https://sandbox.exemplo/c/pay_janela"}
	}
	paid := func(ch Charge) *ProviderCharge {
		pc := open(ch)
		pc.Status, pc.RawStatus = ProviderReceived, "RECEIVED"
		return pc
	}
	cases := []struct {
		name       string
		first      error
		found      func(Charge) *ProviderCharge
		findErr    error
		manual     bool
		wantOp     OperationStatus
		wantError  string
		wantCharge ChargeStatus
		wantLinked bool
		wantCredit int64
	}{
		{"pendente após recusa transitória, recuperação agendada", errRetryable, nil, nil, false,
			OpFailed, obsoleteOperationError, ChargeCanceled, false, 0},
		{"pendente após recusa transitória, reexecução manual", errRetryable, nil, nil, true,
			OpFailed, obsoleteOperationError, ChargeCanceled, false, 0},
		{"resultado desconhecido sem cobrança no provedor", errAmbiguous, nil, nil, false,
			OpFailed, obsoleteOperationError, ChargeCanceled, false, 0},
		{"resultado desconhecido com a cobrança aberta no provedor", errAmbiguous, open, nil, true,
			OpSucceeded, "", ChargeCanceled, true, 0},
		{"resultado desconhecido com a cobrança paga no provedor", errAmbiguous, paid, nil, false,
			OpSucceeded, "", ChargePaid, true, 10000},
		{"consulta recusada definitivamente", errAmbiguous, nil, errPermanent, false,
			OpFailed, errPermanent.Error(), ChargeCanceled, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			f.gw.createResults = []gwResult{{err: c.first}}
			ch := f.requestCharge(t, inv.ID)
			f.voidInvoice(t, inv.ID)
			if c.found != nil {
				f.gw.findResult = c.found(ch)
			}
			f.gw.findErr = c.findErr
			creates, events := len(f.gw.createCalls), len(f.store.events)
			f.clock.Advance(time.Hour)
			if c.manual {
				if err := f.svc.RetryOperation(ctx, f.tenantID, f.operationFor(t, ch.ID).ID); err != nil {
					t.Fatal(err)
				}
			} else if r, err := f.svc.RecoverOperations(ctx); err != nil || r.Executed != 1 {
				t.Fatalf("recuperação: %+v %v", r, err)
			}
			if len(f.gw.createCalls) != creates {
				t.Fatal("a criação obsoleta não cria a cobrança no provedor")
			}
			if len(f.gw.findCalls) != 1 || f.gw.findCalls[0] != ch.ID.String() {
				t.Fatalf("consulta pela referência externa: %v", f.gw.findCalls)
			}
			op := f.operationFor(t, ch.ID)
			if op.Status != c.wantOp || op.LastError != c.wantError {
				t.Fatalf("operação: %s %q, esperado %s %q", op.Status, op.LastError, c.wantOp, c.wantError)
			}
			got := f.store.charges[ch.ID]
			if got.Status != c.wantCharge || (got.ProviderChargeID == "pay_janela") != c.wantLinked {
				t.Fatalf("cobrança: %s vínculo %q", got.Status, got.ProviderChargeID)
			}
			for _, ev := range f.store.eventTypes()[events:] {
				if ev == outbox.EventChargeCreated || ev == outbox.EventChargeFailed {
					t.Fatalf("a cobrança cancelada não é anunciada nem recusada: %v", f.store.eventTypes()[events:])
				}
			}
			if linked := slices.Contains(f.auditActions(), "charge.linked_after_local_cancellation"); linked != c.wantLinked {
				t.Fatalf("auditoria do vínculo após o cancelamento local: %v", f.auditActions())
			}
			var credit int64
			for _, cr := range f.store.credits {
				credit += cr.amount.Amount()
			}
			if credit != c.wantCredit {
				t.Fatalf("crédito do cliente: %d, esperado %d", credit, c.wantCredit)
			}
		})
	}
}

// A fatura anulada durante a chamada ao provedor: a recusa transitória
// encerra a operação (a cobrança não existe no provedor), a recusa
// definitiva conclui a operação sem recusar a cobrança cancelada, a resposta
// ambígua segue para a consulta, e a criação confirmada só vincula a
// cobrança, sem charge.created.
func TestChargeCanceledDuringCreationCall(t *testing.T) {
	cases := []struct {
		name       string
		result     *gwResult
		wantOp     OperationStatus
		wantError  string
		wantLinked bool
	}{
		{"recusa transitória", &gwResult{err: errRetryable}, OpFailed, obsoleteOperationError, false},
		{"recusa definitiva", &gwResult{err: errPermanent}, OpFailed, errPermanent.Error(), false},
		{"resposta ambígua", &gwResult{err: errAmbiguous}, OpUnknown, errAmbiguous.Error(), false},
		{"criação confirmada", nil, OpSucceeded, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPayFixture(t)
			inv := f.addInvoice(t, 10000)
			if c.result != nil {
				f.gw.createResults = []gwResult{*c.result}
			}
			f.gw.onCreate = func() {
				f.gw.onCreate = nil
				f.voidInvoice(t, inv.ID)
			}
			ch := f.requestCharge(t, inv.ID)
			if len(f.gw.createCalls) != 1 {
				t.Fatalf("criações: %d", len(f.gw.createCalls))
			}
			op := f.operationFor(t, ch.ID)
			if op.Status != c.wantOp || op.LastError != c.wantError || op.LeaseUntil != nil {
				t.Fatalf("operação: %s %q lease=%v, esperado %s %q", op.Status, op.LastError, op.LeaseUntil, c.wantOp, c.wantError)
			}
			got := f.store.charges[ch.ID]
			if got.Status != ChargeCanceled || (got.ProviderChargeID != "") != c.wantLinked {
				t.Fatalf("cobrança: %s vínculo %q", got.Status, got.ProviderChargeID)
			}
			if ev := f.store.eventTypes(); slices.Contains(ev, outbox.EventChargeCreated) || slices.Contains(ev, outbox.EventChargeFailed) {
				t.Fatalf("a cobrança cancelada não é anunciada nem recusada: %v", ev)
			}
		})
	}
}
