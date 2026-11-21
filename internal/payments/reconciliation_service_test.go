package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var notFoundAtProvider = &GatewayError{Kind: ErrKindNotFound, StatusCode: 404}

func (f *payFixture) run(t *testing.T, in RunInput) ReconciliationRun {
	t.Helper()
	r, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil {
		t.Fatalf("RunReconciliation: %v", err)
	}
	return r
}

func (f *payFixture) discrepancyOf(t *testing.T, kind DiscrepancyKind) Discrepancy {
	t.Helper()
	for _, d := range f.store.discrepancyList() {
		if d.Kind == kind {
			return d
		}
	}
	t.Fatalf("divergência %s não encontrada: %v", kind, f.store.discrepancyList())
	return Discrepancy{}
}

func providerPaidView(c Charge, amount, fee int64) ProviderCharge {
	p := provPaid(c, amount, fee)
	return p
}

// reconFixture monta: A paga e consistente, B paga com taxa divergente, C
// aguardando pagamento e inexistente no provedor, P somente no provedor e D
// com criação de resultado desconhecido.
type reconFixture struct {
	*payFixture
	a, b, c, d Charge
}

func newReconFixture(t *testing.T) *reconFixture {
	t.Helper()
	f, _, a := paidFixture(t)
	rf := &reconFixture{payFixture: f, a: a}
	invB := f.addInvoice(t, 5000)
	rf.b = f.requestCharge(t, invB.ID)
	f.process(t, f.storeWebhook(t, confirmedWebhook("evt_b", rf.b.ProviderChargeID, 5000, 150)))
	rf.b = f.store.charges[rf.b.ID]
	invC := f.addInvoice(t, 3000)
	rf.c = f.requestCharge(t, invC.ID)
	invD := f.addInvoice(t, 2000)
	f.gw.createResults = []gwResult{{err: errAmbiguous}}
	d, err := f.svc.RequestCharge(context.Background(), f.tenantID, invD.ID, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	rf.d = *d
	foreign := ProviderCharge{ProviderChargeID: "pay_foreign", ExternalReference: "pedido-externo", Status: ProviderReceived, RawStatus: "paid", Amount: brl(700)}
	f.gw.listPages = []ListResult{{Charges: []ProviderCharge{providerPaidView(a, 10000, 199), providerPaidView(rf.b, 5000, 180), foreign}}}
	f.gw.getByID = map[string]gwResult{rf.c.ProviderChargeID: {err: notFoundAtProvider}}
	f.clock.Advance(time.Hour)
	f.store.events, f.store.audits = nil, nil
	return rf
}

func TestRunReconciliationRecordsDiscrepancies(t *testing.T) {
	f := newReconFixture(t)
	run := f.run(t, RunInput{RequestKey: "rec-1"})

	if run.Status != RunCompleted || run.CheckedCount != 4 || run.DiscrepancyCount != 4 || run.Trigger != TriggerManual || run.Error != "" {
		t.Fatalf("execução: %+v", run)
	}
	if want := f.clock.Now().Add(-ManualReconciliationWindow); !run.WindowStart.Equal(want) || !run.WindowEnd.Equal(f.clock.Now()) {
		t.Fatalf("janela padrão: %v %v", run.WindowStart, run.WindowEnd)
	}
	if lr := f.gw.listCalls[0]; !lr.UpdatedFrom.Equal(run.WindowStart) || lr.Limit != 100 || lr.Offset != 0 {
		t.Fatalf("listagem: %+v", lr)
	}
	got := map[DiscrepancyKind]Discrepancy{}
	for _, d := range f.store.discrepancyList() {
		got[d.Kind] = d
	}
	if len(got) != 4 {
		t.Fatalf("divergências: %v", got)
	}
	fee := got[DiscrepancyFeeMismatch]
	if *fee.ChargeID != f.b.ID || fee.LocalAmount.Amount() != 150 || fee.ProviderAmount.Amount() != 180 || fee.Status != DiscrepancyOpen ||
		fee.RunID != run.ID || fee.Occurrences != 1 {
		t.Fatalf("taxa: %+v", fee)
	}
	if *got[DiscrepancyMissingProvider].ChargeID != f.c.ID || got[DiscrepancyMissingLocal].ProviderChargeID != "pay_foreign" {
		t.Fatal("cobranças sem correspondência")
	}
	if u := got[DiscrepancyUnknownOperation]; *u.ChargeID != f.d.ID || u.OperationID == nil {
		t.Fatalf("operação desconhecida: %+v", u)
	}
	actions := f.auditActions()
	if actions[0] != "reconciliation.run_started" || f.store.audits[0].Actor != f.actor || actions[len(actions)-1] != "reconciliation.run_completed" {
		t.Fatalf("auditoria: %v", actions)
	}
	if n := strings.Count(strings.Join(f.store.eventTypes(), ","), outbox.EventDiscrepancyDetected); n != 4 {
		t.Fatalf("eventos de divergência: %d", n)
	}
	// Mesma chave do pedido devolve a execução registrada.
	again := f.run(t, RunInput{RequestKey: "rec-1"})
	if again.ID != run.ID || len(f.gw.listCalls) != 1 {
		t.Fatal("pedido repetido não executa de novo")
	}
	if r, err := f.svc.ReconciliationRun(context.Background(), f.tenantID, run.ID); err != nil || r.ID != run.ID {
		t.Fatalf("consulta da execução: %v", err)
	}
}

func TestRunReconciliationDeduplicatesAndAutoResolves(t *testing.T) {
	f := newReconFixture(t)
	first := f.run(t, RunInput{})
	f.clock.Advance(time.Hour)
	second := f.run(t, RunInput{})
	if len(f.store.discrepancies) != 4 || second.DiscrepancyCount != 4 {
		t.Fatalf("divergências repetidas não duplicam: %d", len(f.store.discrepancies))
	}
	fee := f.discrepancyOf(t, DiscrepancyFeeMismatch)
	if fee.Occurrences != 2 || fee.LastRunID != second.ID || fee.RunID != first.ID {
		t.Fatalf("repetição: %+v", fee)
	}
	// Taxa corrigida no provedor: a divergência é resolvida automaticamente.
	f.gw.listPages[0].Charges[1] = providerPaidView(f.b, 5000, 150)
	f.clock.Advance(time.Hour)
	third := f.run(t, RunInput{})
	fee = f.discrepancyOf(t, DiscrepancyFeeMismatch)
	if fee.Status != DiscrepancyResolved || fee.Resolution != ResolutionAutoResolved || fee.ResolvedBy != nil || third.DiscrepancyCount != 3 {
		t.Fatalf("resolução automática: %+v (%d)", fee, third.DiscrepancyCount)
	}
	// A divergência volta com os mesmos valores: é reaberta.
	f.gw.listPages[0].Charges[1] = providerPaidView(f.b, 5000, 180)
	f.clock.Advance(time.Hour)
	f.run(t, RunInput{})
	fee = f.discrepancyOf(t, DiscrepancyFeeMismatch)
	if fee.Status != DiscrepancyOpen || fee.Resolution != "" || fee.Occurrences != 3 || !slices.Contains(f.auditActions(), "reconciliation.discrepancy_reopened") {
		t.Fatalf("reabertura: %+v", fee)
	}
	// Resolvida manualmente: a mesma divergência não é reaberta.
	if _, err := f.svc.ResolveDiscrepancy(context.Background(), f.tenantID, f.actor, &f.userID, ResolveInput{ID: fee.ID,
		Resolution: ResolutionKeepLocal, Justification: "Taxa conferida no contrato com o provedor."}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	last := f.run(t, RunInput{})
	if fee = f.discrepancyOf(t, DiscrepancyFeeMismatch); fee.Status != DiscrepancyResolved || fee.Resolution != ResolutionKeepLocal || last.DiscrepancyCount != 3 {
		t.Fatalf("resolução manual preservada: %+v %d", fee, last.DiscrepancyCount)
	}
	// Operação resolvida: a divergência é resolvida automaticamente.
	op := f.operationFor(t, f.d.ID)
	op.Status = OpFailed
	f.store.operations[op.ID] = op
	f.clock.Advance(time.Hour)
	f.run(t, RunInput{})
	if u := f.discrepancyOf(t, DiscrepancyUnknownOperation); u.Status != DiscrepancyResolved || u.Resolution != ResolutionAutoResolved {
		t.Fatalf("operação resolvida: %+v", u)
	}
}

func TestRunReconciliationProviderFailures(t *testing.T) {
	t.Run("listagem", func(t *testing.T) {
		f := newReconFixture(t)
		f.gw.listErr = errAmbiguous
		run := f.run(t, RunInput{})
		if run.Status != RunFailed || run.FinishedAt == nil || !strings.Contains(run.Error, "listagem do provedor") || len(f.store.discrepancies) != 0 {
			t.Fatalf("execução: %+v", run)
		}
		if !slices.Contains(f.auditActions(), "reconciliation.run_failed") {
			t.Fatalf("auditoria: %v", f.auditActions())
		}
	})
	t.Run("consulta individual", func(t *testing.T) {
		f := newReconFixture(t)
		f.gw.getByID[f.c.ProviderChargeID] = gwResult{err: errAmbiguous}
		if run := f.run(t, RunInput{}); run.Status != RunFailed || !strings.Contains(run.Error, f.c.ProviderChargeID) {
			t.Fatalf("execução: %+v", run)
		}
	})
	t.Run("provedor indisponível na instalação", func(t *testing.T) {
		f := newReconFixture(t)
		f.resolver.gwErr = errors.New("sem adaptador")
		if run := f.run(t, RunInput{}); run.Status != RunFailed {
			t.Fatalf("execução: %+v", run)
		}
	})
	t.Run("cobrança encontrada na consulta individual é comparada", func(t *testing.T) {
		f := newReconFixture(t)
		p := ProviderCharge{ProviderChargeID: f.c.ProviderChargeID, Status: ProviderReceived, Amount: brl(3000)}
		f.gw.getByID[f.c.ProviderChargeID] = gwResult{pc: p}
		f.run(t, RunInput{})
		if d := f.discrepancyOf(t, DiscrepancyPaymentNotReflected); *d.ChargeID != f.c.ID {
			t.Fatalf("pagamento não refletido: %+v", d)
		}
	})
}

func TestRunReconciliationLimits(t *testing.T) {
	f := newReconFixture(t)
	page := f.gw.listPages[0]
	page.HasMore = true
	f.gw.listPages = nil
	for range maxProviderPages + 2 {
		f.gw.listPages = append(f.gw.listPages, page)
	}
	run := f.run(t, RunInput{})
	if run.Status != RunCompleted || run.Error == "" || len(f.gw.listCalls) != maxProviderPages {
		t.Fatalf("execução parcial: %+v (%d páginas)", run, len(f.gw.listCalls))
	}
	if f.gw.listCalls[1].Offset != providerPageSize {
		t.Fatalf("paginação: %+v", f.gw.listCalls[1])
	}
}

// O limite de cobranças locais só corta cobranças da janela ausentes da
// listagem do provedor; a execução é concluída com aviso de execução parcial
// e a cobrança listada pelo provedor não vira divergência missing_local.
func TestRunReconciliationLocalLimitMarksPartialRun(t *testing.T) {
	f := newReconFixture(t)
	created := f.clock.Now().Add(-time.Hour)
	for i := range maxLocalCharges + 1 {
		c := Charge{ID: uuid.New(), TenantID: f.tenantID, InvoiceID: uuid.New(), CustomerID: f.customer, ProviderAccountID: f.account.ID,
			Provider: "simulator", Status: ChargePending, Amount: brl(int64(100 + i)), CreatedAt: created, UpdatedAt: created, Version: 1}
		f.store.charges[c.ID] = c
		f.store.chargeOrder = append([]uuid.UUID{c.ID}, f.store.chargeOrder...)
	}
	run := f.run(t, RunInput{})
	if run.Status != RunCompleted || !strings.Contains(run.Error, partialLocalCharges) {
		t.Fatalf("execução parcial: %+v", run)
	}
	for _, d := range f.store.discrepancyList() {
		if d.Kind == DiscrepancyMissingLocal && d.ProviderChargeID != "pay_foreign" {
			t.Fatalf("cobrança listada sem correspondência local: %+v", d)
		}
	}
	if fee := f.discrepancyOf(t, DiscrepancyFeeMismatch); *fee.ChargeID != f.b.ID {
		t.Fatalf("cobrança listada comparada: %+v", fee)
	}
}

// As cobranças locais correspondentes à listagem do provedor (até duas por
// cobrança listada) sempre cabem no limite da leitura local.
func TestReconciliationListedChargesFitLocalLimit(t *testing.T) {
	if listed := 2 * maxProviderPages * providerPageSize; listed >= maxLocalCharges {
		t.Fatalf("cobranças listadas (%d) não cabem no limite local (%d)", listed, maxLocalCharges)
	}
}

// Com mais operações de resultado desconhecido que o limite da leitura, a
// divergência da operação que ficou fora da leitura só é resolvida
// automaticamente quando a operação deixa de ter resultado desconhecido.
func TestRunReconciliationUnknownOperationsLimit(t *testing.T) {
	f := newReconFixture(t)
	f.run(t, RunInput{})
	target := f.discrepancyOf(t, DiscrepancyUnknownOperation)
	for i := range maxUnknownOperations {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		f.store.operations[id] = Operation{ID: id, TenantID: f.tenantID, ChargeID: f.c.ID, Kind: OpCreateCharge, Status: OpUnknown,
			NextAttemptAt: f.clock.Now().Add(time.Hour), Attempts: MaxOperationAttempts, CreatedAt: f.clock.Now()}
	}
	f.clock.Advance(time.Hour)
	second := f.run(t, RunInput{})
	if got := f.store.discrepancies[target.ID]; got.Status != DiscrepancyOpen || !strings.Contains(second.Error, partialUnknownOps) {
		t.Fatalf("operação fora do limite continua desconhecida: %s %q", got.Status, second.Error)
	}
	op := f.operationFor(t, f.d.ID)
	op.Status = OpFailed
	f.store.operations[op.ID] = op
	f.clock.Advance(time.Hour)
	f.run(t, RunInput{})
	if got := f.store.discrepancies[target.ID]; got.Status != DiscrepancyResolved || got.Resolution != ResolutionAutoResolved {
		t.Fatalf("operação resolvida fora do limite: %+v", got)
	}
	t.Run("falha ao consultar a operação", func(t *testing.T) {
		last := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
		f.store.operations[last] = Operation{ID: last, TenantID: f.tenantID, ChargeID: f.c.ID, Kind: OpCreateCharge, Status: OpUnknown,
			NextAttemptAt: f.clock.Now().Add(time.Hour), Attempts: MaxOperationAttempts, CreatedAt: f.clock.Now()}
		f.store.errs["Operation"] = errors.New("banco indisponível")
		other := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyUnknownOperation, OperationID: uuidPtr(uuid.New()), RunID: target.RunID})
		f.clock.Advance(time.Hour)
		if _, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{}); err == nil {
			t.Fatal("esperava erro")
		}
		delete(f.store.errs, "Operation")
		f.clock.Advance(time.Hour)
		f.run(t, RunInput{})
		if got := f.store.discrepancies[other.ID]; got.Status != DiscrepancyResolved {
			t.Fatalf("operação inexistente: %s", got.Status)
		}
	})
}

// Cobrança paga fora da janela que o provedor não lista (o Asaas filtra a
// listagem pela data de criação) e que nada atualizou localmente (webhook
// perdido) é consultada pelo rodízio: o chargeback tardio vira divergência.
func TestRunReconciliationRotationChecksChargesOutsideWindow(t *testing.T) {
	f, _, c := paidFixture(t)
	f.clock.Advance(40 * 24 * time.Hour)
	f.gw.getByID = map[string]gwResult{c.ProviderChargeID: {pc: chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, f.clock.Now())}}
	run := f.run(t, RunInput{Trigger: TriggerManual})
	if run.Status != RunCompleted || run.Error != "" || !slices.Equal(f.gw.getCalls, []string{c.ProviderChargeID}) {
		t.Fatalf("execução: %+v consultas=%v", run, f.gw.getCalls)
	}
	if d := f.discrepancyOf(t, DiscrepancyChargebackPending); *d.ChargeID != c.ID {
		t.Fatalf("chargeback tardio: %+v", d)
	}
	if at, ok := f.store.checkedAt[c.ID]; !ok || !at.Equal(f.clock.Now()) {
		t.Fatalf("verificação registrada: %v %v", at, ok)
	}
	sc := f.store.rotationScopes[0]
	if !sc.Since.Equal(f.clock.Now().Add(-rotationSpan)) || sc.Limit != rotationLookups || !sc.WindowStart.Equal(run.WindowStart) {
		t.Fatalf("escopo do rodízio: %+v", sc)
	}
	// Cobrança criada antes do prazo do rodízio não é consultada.
	f.clock.Advance(rotationSpan)
	f.gw.getCalls = nil
	if run := f.run(t, RunInput{}); run.Status != RunCompleted || len(f.gw.getCalls) != 0 {
		t.Fatalf("fora do rodízio: %+v %v", run, f.gw.getCalls)
	}
}

// addRotationCharge acrescenta uma cobrança paga há 40 dias, fora da janela
// da execução, vinculada ao provedor e sujeita ao rodízio, igual à A.
func (f *reconFixture) addRotationCharge(pid string) Charge {
	c := f.a
	created := f.clock.Now().Add(-40 * 24 * time.Hour)
	c.ID, c.InvoiceID, c.ProviderChargeID, c.CreatedAt, c.UpdatedAt = uuid.New(), uuid.New(), pid, created, created
	f.store.charges[c.ID] = c
	f.store.chargeOrder = append(f.store.chargeOrder, c.ID)
	return c
}

func (f *payFixture) auditOf(t *testing.T, action string) audit.Entry {
	t.Helper()
	for _, a := range f.store.audits {
		if a.Action == action {
			return a
		}
	}
	t.Fatalf("auditoria %s não encontrada", action)
	return audit.Entry{}
}

// O rodízio é de melhor esforço: a falha de uma consulta não encerra a
// execução como failed nem a marca como parcial, e a janela é comparada e
// concluída normalmente. A cobrança cuja consulta falhou não é registrada
// como verificada (volta primeiro na próxima execução); a recusa definitiva,
// própria da cobrança, não interrompe o rodízio, e três falhas seguidas de
// provedor indisponível o interrompem.
func TestRunReconciliationRotationBestEffort(t *testing.T) {
	cases := []struct {
		name string
		// results é o resultado da consulta de cada cobrança do rodízio, na
		// ordem do rodízio (nil: cobrança consistente com o provedor).
		results      []error
		wantChecked  []bool
		wantCalls    int
		wantFailures int
	}{
		{name: "falha isolada é pulada", results: []error{errAmbiguous, nil}, wantChecked: []bool{false, true}, wantCalls: 2, wantFailures: 1},
		{name: "recusa definitiva não interrompe o rodízio", results: []error{errPermanent, errPermanent, errPermanent, nil},
			wantChecked: []bool{false, false, false, true}, wantCalls: 4, wantFailures: 3},
		{name: "três falhas seguidas interrompem o rodízio", results: []error{errAmbiguous, errRetryable, errAmbiguous, nil},
			wantChecked: []bool{false, false, false, false}, wantCalls: 3, wantFailures: 3},
		{name: "consulta bem-sucedida reinicia a contagem", results: []error{errAmbiguous, errAmbiguous, nil, errRetryable, errAmbiguous, nil},
			wantChecked: []bool{false, false, true, false, false, true}, wantCalls: 6, wantFailures: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconFixture(t)
			old := make([]Charge, len(tc.results))
			for i, res := range tc.results {
				old[i] = f.addRotationCharge(fmt.Sprintf("pay_antiga_%d", i))
				f.gw.getByID[old[i].ProviderChargeID] = gwResult{pc: providerPaidView(old[i], 10000, 199), err: res}
			}
			run := f.run(t, RunInput{Trigger: TriggerManual})
			if run.Status != RunCompleted || run.Error != "" || run.DiscrepancyCount != 4 {
				t.Fatalf("execução: %+v", run)
			}
			if fee := f.discrepancyOf(t, DiscrepancyFeeMismatch); *fee.ChargeID != f.b.ID {
				t.Fatalf("janela comparada: %+v", fee)
			}
			if _, ok := f.store.checkedAt[f.b.ID]; !ok {
				t.Fatal("cobrança da janela registrada como verificada")
			}
			calls := 0
			for i, c := range old {
				if slices.Contains(f.gw.getCalls, c.ProviderChargeID) {
					calls++
				}
				if _, ok := f.store.checkedAt[c.ID]; ok != tc.wantChecked[i] {
					t.Errorf("cobrança %d do rodízio verificada = %v, esperado %v", i, ok, tc.wantChecked[i])
				}
			}
			if calls != tc.wantCalls {
				t.Fatalf("consultas do rodízio = %d, esperado %d (%v)", calls, tc.wantCalls, f.gw.getCalls)
			}
			if got := f.auditOf(t, "reconciliation.run_completed").Details["rotation_failures"]; got != tc.wantFailures {
				t.Fatalf("falhas do rodízio na auditoria = %v", got)
			}
		})
	}
}

// slowGetGateway espera o fim do prazo da chamada na consulta da cobrança
// lenta, como um provedor que não responde.
type slowGetGateway struct {
	*fakeGateway
	slow string
}

func (g *slowGetGateway) GetCharge(ctx context.Context, id string) (ProviderCharge, error) {
	if id != g.slow {
		return g.fakeGateway.GetCharge(ctx, id)
	}
	g.getCalls = append(g.getCalls, id)
	<-ctx.Done()
	return ProviderCharge{}, &GatewayError{Kind: ErrKindAmbiguous, Message: "resposta não recebida", Err: ctx.Err()}
}

// O prazo da coleta esgotado durante o rodízio, ou o cancelamento de quem
// chamou, interrompe somente o rodízio: a execução é concluída sem aviso de
// execução parcial, porque a janela foi toda verificada, e o que não foi
// consultado fica para as próximas execuções.
func TestRunReconciliationRotationInterruptedKeepsRunComplete(t *testing.T) {
	t.Run("prazo da coleta esgotado", func(t *testing.T) {
		f := newReconFixture(t)
		f.svc = NewService(f.store, f.resolver, f.clock, Config{ReconciliationBudget: 80 * time.Millisecond, CallTimeout: time.Minute})
		slow, next := f.addRotationCharge("pay_antiga_lenta"), f.addRotationCharge("pay_antiga_seguinte")
		f.resolver.override = &slowGetGateway{fakeGateway: f.gw, slow: slow.ProviderChargeID}
		run := f.run(t, RunInput{})
		if run.Status != RunCompleted || run.Error != "" {
			t.Fatalf("execução: %+v", run)
		}
		if slices.Contains(f.gw.getCalls, next.ProviderChargeID) {
			t.Fatalf("rodízio continuou depois do prazo: %v", f.gw.getCalls)
		}
		if _, ok := f.store.checkedAt[slow.ID]; ok {
			t.Fatal("cobrança sem resposta registrada como verificada")
		}
		if fee := f.discrepancyOf(t, DiscrepancyFeeMismatch); *fee.ChargeID != f.b.ID {
			t.Fatalf("janela comparada: %+v", fee)
		}
	})
	t.Run("cancelamento da execução agendada", func(t *testing.T) {
		f := newReconFixture(t)
		f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
		first, next := f.addRotationCharge("pay_antiga_1"), f.addRotationCharge("pay_antiga_2")
		f.gw.getByID[first.ProviderChargeID] = gwResult{pc: providerPaidView(first, 10000, 199)}
		f.gw.getByID[next.ProviderChargeID] = gwResult{pc: providerPaidView(next, 10000, 199)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.gw.onGet = func() {
			if f.gw.getCalls[len(f.gw.getCalls)-1] == first.ProviderChargeID {
				cancel()
			}
		}
		run, err := f.svc.RunReconciliation(ctx, f.tenantID, f.actor, nil, RunInput{Trigger: TriggerScheduled})
		if err != nil || run.Status != RunCompleted || run.Error != "" {
			t.Fatalf("execução: %+v %v", run, err)
		}
		if slices.Contains(f.gw.getCalls, next.ProviderChargeID) {
			t.Fatalf("rodízio continuou depois do cancelamento: %v", f.gw.getCalls)
		}
		if _, ok := f.store.checkedAt[first.ID]; !ok {
			t.Fatal("cobrança consultada antes do cancelamento registrada como verificada")
		}
	})
}

// partialListGateway devolve a primeira página da listagem e espera o fim do
// prazo da chamada nas seguintes, como um provedor lento.
type partialListGateway struct {
	*fakeGateway
	deadlines []time.Duration
}

func (g *partialListGateway) ListCharges(ctx context.Context, req ListRequest) (ListResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		g.deadlines = append(g.deadlines, time.Until(dl))
	}
	if req.Offset == 0 {
		return g.fakeGateway.ListCharges(ctx, req)
	}
	<-ctx.Done()
	return ListResult{}, &GatewayError{Kind: ErrKindAmbiguous, Message: "resposta não recebida", Err: ctx.Err()}
}

// A coleta tem prazo próprio (uma fração do lease da execução): esgotado o
// prazo, a execução é concluída com o que foi coletado e com aviso de
// execução parcial, sem atribuir a falha ao provedor.
func TestRunReconciliationCollectionBudget(t *testing.T) {
	f := newReconFixture(t)
	const budget = 80 * time.Millisecond
	f.svc = NewService(f.store, f.resolver, f.clock, Config{ReconciliationBudget: budget, CallTimeout: time.Minute})
	f.gw.listPages[0].HasMore = true
	gw := &partialListGateway{fakeGateway: f.gw}
	f.resolver.override = gw
	run, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCompleted || !strings.Contains(run.Error, partialBudget) || strings.Contains(run.Error, "provedor:") {
		t.Fatalf("execução parcial pelo prazo: %+v", run)
	}
	if len(gw.deadlines) != 2 || gw.deadlines[0] < budget/2 || gw.deadlines[0] > budget {
		t.Fatalf("prazo da coleta: %v", gw.deadlines)
	}
	if fee := f.discrepancyOf(t, DiscrepancyFeeMismatch); *fee.ChargeID != f.b.ID {
		t.Fatalf("página coletada é comparada: %+v", fee)
	}
	for _, d := range f.store.discrepancyList() {
		if d.Kind == DiscrepancyMissingProvider {
			t.Fatalf("cobrança não consultada não vira divergência: %+v", d)
		}
	}
	t.Run("prazo configurado nunca passa do lease", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second, reconciliationLease} {
			if got := NewService(f.store, f.resolver, f.clock, Config{ReconciliationBudget: d}).cfg.ReconciliationBudget; got != ReconciliationCollectionBudget {
				t.Fatalf("prazo %v: %v", d, got)
			}
		}
		if ReconciliationCollectionBudget >= reconciliationLease {
			t.Fatal("a coleta precisa terminar antes do lease")
		}
	})
}

func TestRunReconciliationRules(t *testing.T) {
	t.Run("janela inválida", func(t *testing.T) {
		f := newReconFixture(t)
		now := f.clock.Now()
		for name, in := range map[string]RunInput{
			"início depois do fim": {WindowStart: now, WindowEnd: now.Add(-time.Hour)},
			"fim no futuro":        {WindowEnd: now.Add(time.Hour)},
			"janela longa":         {WindowStart: now.Add(-100 * 24 * time.Hour), WindowEnd: now},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, in)
				wantAppErr(t, err, apperr.KindInvalid, "invalid_reconciliation_window")
			})
		}
	})
	t.Run("sem provedor padrão", func(t *testing.T) {
		f := newPayFixture(t)
		a := f.account
		a.IsDefault = false
		f.store.accounts[a.ID] = a
		_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
		wantAppErr(t, err, apperr.KindConflict, "provider_not_configured")
	})
	t.Run("execução em andamento", func(t *testing.T) {
		f := newReconFixture(t)
		lease := f.clock.Now().Add(time.Minute)
		running := ReconciliationRun{ID: uuid.New(), TenantID: f.tenantID, ProviderAccountID: f.account.ID, Status: RunRunning, LeaseUntil: &lease,
			Attempts: 1}
		f.store.runs[running.ID] = running
		_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
		wantAppErr(t, err, apperr.KindConflict, "reconciliation_running")
		// Lease expirado: a execução interrompida continua sendo a execução
		// em andamento da conta (será retomada), e outra não é registrada.
		f.clock.Advance(time.Hour)
		_, err = f.svc.StartReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
		wantAppErr(t, err, apperr.KindConflict, "reconciliation_running")
		if f.store.runs[running.ID].Status != RunRunning || len(f.store.runs) != 1 {
			t.Fatalf("execução interrompida: %+v", f.store.runs[running.ID])
		}
	})
	t.Run("agendada exige o recurso do plano", func(t *testing.T) {
		f := newReconFixture(t)
		_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{Trigger: TriggerScheduled})
		wantAppErr(t, err, apperr.KindForbidden, "platform_feature_unavailable")
		f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
		run := f.run(t, RunInput{Trigger: TriggerScheduled})
		if run.Trigger != TriggerScheduled || !run.WindowStart.Equal(run.WindowEnd.Add(-ScheduledReconciliationWindow)) {
			t.Fatalf("execução agendada: %+v", run)
		}
	})
	t.Run("execução encerrada por outra instância", func(t *testing.T) {
		f := newReconFixture(t)
		f.gw.onList = func() {
			for id, r := range f.store.runs {
				r.Status = RunFailed
				f.store.runs[id] = r
			}
		}
		_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
		wantAppErr(t, err, apperr.KindConflict, "reconciliation_run_interrupted")
	})
	for _, m := range []string{"ReconciliationRunByRequestKey", "PlatformPlan", "DefaultAccount", "InsertReconciliationRun"} {
		t.Run("falha ao iniciar: "+m, func(t *testing.T) {
			f := newReconFixture(t)
			f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
			boom := errors.New(m)
			f.store.errs[m] = boom
			_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{Trigger: TriggerScheduled, RequestKey: "k"})
			if !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
	for _, m := range []string{"ChargesForReconciliation", "ChargesForRotation", "RefundsForCharges", "UnknownOperations"} {
		t.Run("falha ao ler: "+m, func(t *testing.T) {
			f := newReconFixture(t)
			f.store.errs[m] = errors.New(m)
			if run := f.run(t, RunInput{}); run.Status != RunFailed {
				t.Fatalf("execução: %+v", run)
			}
		})
	}
	for _, m := range []string{"DiscrepancyByKey", "InsertDiscrepancy", "Emit", "OpenDiscrepanciesForSubjects", "MarkChargesReconciled",
		"FinishReconciliationRun"} {
		t.Run("falha ao concluir: "+m, func(t *testing.T) {
			f := newReconFixture(t)
			boom := errors.New(m)
			f.gw.onList = func() { f.store.errs[m] = boom }
			_, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{})
			if !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
		})
	}
	t.Run("falha ao registrar a falha da execução", func(t *testing.T) {
		f := newReconFixture(t)
		f.gw.listErr = errAmbiguous
		f.store.errs["ReconciliationRun"] = errors.New("x")
		if _, err := f.svc.RunReconciliation(context.Background(), f.tenantID, f.actor, nil, RunInput{}); err == nil {
			t.Fatal("esperava erro")
		}
	})
	t.Run("consulta de execução inexistente", func(t *testing.T) {
		f := newPayFixture(t)
		_, err := f.svc.ReconciliationRun(context.Background(), f.tenantID, uuid.New())
		wantAppErr(t, err, apperr.KindNotFound, "reconciliation_run_not_found")
	})
}

func TestRunScheduledReconciliations(t *testing.T) {
	f := newReconFixture(t)
	f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
	f.store.dueTenants = []uuid.UUID{f.tenantID}
	if n, err := f.svc.RunScheduledReconciliations(context.Background()); err != nil || n != 1 {
		t.Fatalf("execuções: %d %v", n, err)
	}
	// Execução em andamento e plano sem o recurso são ignorados.
	lease := f.clock.Now().Add(time.Hour)
	f.store.runs[uuid.New()] = ReconciliationRun{ProviderAccountID: f.account.ID, Status: RunRunning, LeaseUntil: &lease}
	if n, err := f.svc.RunScheduledReconciliations(context.Background()); err != nil || n != 0 {
		t.Fatalf("em andamento: %d %v", n, err)
	}
	f.store.runs = map[uuid.UUID]ReconciliationRun{}
	f.store.plan = platformbilling.Plan{}
	if n, err := f.svc.RunScheduledReconciliations(context.Background()); err != nil || n != 0 {
		t.Fatalf("sem recurso: %d %v", n, err)
	}
	f.store.plan = platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureReconciliationRun}}
	f.gw.listErr = errAmbiguous
	if _, err := f.svc.RunScheduledReconciliations(context.Background()); err == nil || !strings.Contains(err.Error(), f.tenantID.String()) {
		t.Fatalf("falha do provedor é informada: %v", err)
	}
	f.store.errs["DefaultAccount"] = errors.New("banco indisponível")
	if _, err := f.svc.RunScheduledReconciliations(context.Background()); err == nil {
		t.Fatal("erro de infraestrutura é informado")
	}
	f.store.dueErr = errors.New("descoberta")
	if _, err := f.svc.RunScheduledReconciliations(context.Background()); !errors.Is(err, f.store.dueErr) {
		t.Fatalf("descoberta: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.store.dueErr = nil
	if _, err := f.svc.RunScheduledReconciliations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelamento: %v", err)
	}
}

// --- resolução ---

func (f *payFixture) addDiscrepancy(t *testing.T, d Discrepancy) Discrepancy {
	t.Helper()
	d.ID, d.TenantID, d.Status, d.Provider, d.Version = uuid.New(), f.tenantID, DiscrepancyOpen, "simulator", 1
	if d.DedupeKey == "" {
		d.DedupeKey = string(d.Kind) + ":" + d.ID.String()
	}
	if d.LocalValue == nil {
		d.LocalValue = map[string]any{}
	}
	f.store.discrepancies[d.ID] = d
	f.store.discOrder = append(f.store.discOrder, d.ID)
	return d
}

func (f *payFixture) resolve(d Discrepancy, r Resolution) (Discrepancy, error) {
	return f.svc.ResolveDiscrepancy(context.Background(), f.tenantID, f.actor, &f.userID, ResolveInput{ID: d.ID, Resolution: r,
		Justification: "Conferido com o extrato do provedor."})
}

func mp(v int64) *money.Money { m := brl(v); return &m }

func TestResolveDiscrepancyBasicActions(t *testing.T) {
	f, _, c := paidFixture(t)
	d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyMissingLocal, ProviderChargeID: "pay_x"})
	got, err := f.resolve(d, ResolutionDismiss)
	if err != nil || got.Status != DiscrepancyDismissed || got.Resolution != ResolutionDismiss || *got.ResolvedBy != f.userID ||
		got.Justification != "Conferido com o extrato do provedor." || got.ResolvedAt == nil {
		t.Fatalf("explicada: %+v %v", got, err)
	}
	a := f.store.audits[len(f.store.audits)-1]
	if a.Action != "reconciliation.discrepancy_resolved" || a.Actor != f.actor || a.Details["justification"] != got.Justification {
		t.Fatalf("auditoria: %+v", a)
	}
	_, err = f.resolve(d, ResolutionKeepLocal)
	wantAppErr(t, err, apperr.KindConflict, "discrepancy_not_open")
	other := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyMissingLocal, ProviderChargeID: "pay_y"})
	_, err = f.resolve(other, ResolutionAcceptProvider)
	wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
	if got, err := f.resolve(other, ResolutionKeepLocal); err != nil || got.Status != DiscrepancyResolved {
		t.Fatalf("mantém o local: %+v %v", got, err)
	}
	_, err = f.svc.ResolveDiscrepancy(context.Background(), f.tenantID, f.actor, nil, ResolveInput{ID: d.ID, Resolution: ResolutionDismiss})
	wantAppErr(t, err, apperr.KindInvalid, "invalid_resolution")
	_, err = f.resolve(Discrepancy{ID: uuid.New()}, ResolutionDismiss)
	wantAppErr(t, err, apperr.KindNotFound, "discrepancy_not_found")
	if got, err := f.svc.Discrepancy(context.Background(), f.tenantID, d.ID); err != nil || got.ID != d.ID {
		t.Fatalf("consulta: %v", err)
	}
	_ = c
}

func TestResolveFeeAndAmountMismatch(t *testing.T) {
	t.Run("taxa: ajuste no ledger sem alterar a cobrança", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyFeeMismatch, ChargeID: &c.ID, LocalAmount: mp(199), ProviderAmount: mp(250)})
		got, err := f.resolve(d, ResolutionAdjustLedger)
		if err != nil || got.LedgerTransactionID == nil {
			t.Fatalf("ajuste: %+v %v", got, err)
		}
		net := ledgerNet(postedByKind(f.store.posted, "reconciliation.adjustment"))
		if net[ledger.ReconciliationAdjustments] != 51 || net[ledger.ProviderClearing] != -51 || f.store.charges[c.ID].Fee.Amount() != 199 {
			t.Fatalf("ledger: %v taxa %v", net, f.store.charges[c.ID].Fee)
		}
	})
	t.Run("taxa: aceita o valor do provedor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyFeeMismatch, ChargeID: &c.ID, LocalAmount: mp(199), ProviderAmount: mp(150)})
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		net := ledgerNet(postedByKind(f.store.posted, "reconciliation.adjustment"))
		if net[ledger.ProviderClearing] != 49 || f.store.charges[c.ID].Fee.Amount() != 150 {
			t.Fatalf("ledger: %v taxa %v", net, f.store.charges[c.ID].Fee)
		}
	})
	t.Run("taxa sem valores", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyFeeMismatch, ChargeID: &c.ID})
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
		_, err = f.resolve(d, ResolutionAdjustLedger)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
	})
	t.Run("valor pago divergente", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyAmountMismatch, ChargeID: &c.ID, LocalAmount: mp(10000), ProviderAmount: mp(9900),
			LocalValue: map[string]any{"basis": "paid"}})
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if net := ledgerNet(postedByKind(f.store.posted, "reconciliation.adjustment")); net[ledger.ReconciliationAdjustments] != 100 {
			t.Fatalf("ledger: %v", net)
		}
	})
	t.Run("valor da cobrança em aberto não tem ajuste", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyAmountMismatch, ChargeID: &c.ID, LocalAmount: mp(10000), ProviderAmount: mp(9000),
			LocalValue: map[string]any{"basis": "charge"}})
		_, err := f.resolve(d, ResolutionAdjustLedger)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
		_, err = f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
	})
	t.Run("ajuste de tipo sem valor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, LocalAmount: mp(1), ProviderAmount: mp(2)})
		dd := d
		if err := f.svc.postAdjustment(context.Background(), f.store, &dd, payNow); !apperr.IsKind(err, apperr.KindConflict) {
			t.Fatalf("erro: %v", err)
		}
		dd.Kind, dd.LocalAmount = DiscrepancyFeeMismatch, &money.Money{}
		if err := f.svc.postAdjustment(context.Background(), f.store, &dd, payNow); err == nil {
			t.Fatal("moedas diferentes")
		}
	})
}

func TestResolvePaymentNotReflected(t *testing.T) {
	t.Run("aplica o pagamento do provedor", func(t *testing.T) {
		f := newPayFixture(t)
		inv, c := awaitingCharge(t, f)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyPaymentNotReflected, ChargeID: &c.ID})
		f.gw.getResult = *providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, nil)
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if f.store.charges[c.ID].Status != ChargePaid || f.invoice(inv.ID).Status != billing.Paid {
			t.Fatalf("pagamento aplicado: %s %s", f.store.charges[c.ID].Status, f.invoice(inv.ID).Status)
		}
	})
	cases := []struct {
		name string
		res  gwResult
		code string
		kind apperr.Kind
	}{
		{"provedor sem pagamento", gwResult{pc: ProviderCharge{Status: ProviderPending, Amount: brl(10000)}}, "payment_not_applicable", apperr.KindConflict},
		{"cobrança inexistente no provedor", gwResult{err: notFoundAtProvider}, "provider_charge_not_found", apperr.KindConflict},
		{"provedor indisponível", gwResult{err: errAmbiguous}, "provider_unavailable", apperr.KindUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPayFixture(t)
			_, c := awaitingCharge(t, f)
			d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyPaymentNotReflected, ChargeID: &c.ID})
			f.gw.getResult, f.gw.getErr = tc.res.pc, tc.res.err
			_, err := f.resolve(d, ResolutionAcceptProvider)
			wantAppErr(t, err, tc.kind, tc.code)
			if f.store.discrepancies[d.ID].Status != DiscrepancyOpen {
				t.Fatal("divergência continua aberta")
			}
		})
	}
	t.Run("gateway indisponível", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyPaymentNotReflected, ChargeID: &c.ID})
		f.resolver.gwErr = errors.New("sem adaptador")
		if _, err := f.resolve(d, ResolutionAcceptProvider); !errors.Is(err, f.resolver.gwErr) {
			t.Fatalf("erro: %v", err)
		}
	})
}

func TestResolveRefundAndChargebackPending(t *testing.T) {
	t.Run("reembolso sem registro no provedor é encerrado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 3000, "")
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, RefundID: &r.ID})
		f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000)}
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if got := f.store.refunds[r.ID]; got.Status != RefundFailed || !f.store.charges[c.ID].AmountRefundPending.IsZero() ||
			f.refundOperation(t, r.ID).Status != OpFailed {
			t.Fatalf("reembolso encerrado: %+v", got)
		}
	})
	t.Run("reembolso efetivado no provedor é confirmado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 3000, "")
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, RefundID: &r.ID})
		f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderPartiallyRefunded, Amount: brl(10000),
			Refunds: []ProviderRefund{{ExternalReference: r.ID.String(), State: RefundStateDone, Amount: brl(3000)}}}
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if f.store.refunds[r.ID].Status != RefundSucceeded {
			t.Fatal("confirmado")
		}
	})
	t.Run("reembolso em processamento no provedor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 3000, "")
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, RefundID: &r.ID})
		f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000),
			Refunds: []ProviderRefund{{ExternalReference: r.ID.String(), State: RefundStatePending, Amount: brl(3000)}}}
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "refund_pending_at_provider")
	})
	t.Run("reembolso em execução", func(t *testing.T) {
		f, _, c := paidFixture(t)
		f.gw.refundResults = []refundRes{{err: errAmbiguous}}
		r := f.refund(t, c.ID, 3000, "")
		op := f.refundOperation(t, r.ID)
		lease := f.clock.Now().Add(time.Minute)
		op.Status, op.LeaseUntil = OpInFlight, &lease
		f.store.operations[op.ID] = op
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, RefundID: &r.ID})
		f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000)}
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "refund_in_flight")
		// Lease expirado (processo interrompido durante a chamada): o
		// reembolso sem registro no provedor é encerrado.
		f.clock.Advance(2 * time.Minute)
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if got := f.store.refunds[r.ID]; got.Status != RefundFailed {
			t.Fatalf("reembolso encerrado: %+v", got)
		}
	})
	t.Run("chargeback sincronizado", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyChargebackPending, ChargeID: &c.ID})
		f.gw.getResult = chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour))
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if f.store.charges[c.ID].Status != ChargeDisputed || len(f.store.disputes) != 1 {
			t.Fatalf("disputa aberta: %s", f.store.charges[c.ID].Status)
		}
	})
}

func TestResolveUnknownOperationAndMissingProvider(t *testing.T) {
	t.Run("operação reconsultada e resolvida", func(t *testing.T) {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		f.gw.createResults = []gwResult{{err: errAmbiguous}}
		c, _ := f.svc.RequestCharge(context.Background(), f.tenantID, inv.ID, f.actor)
		op := f.operationFor(t, c.ID)
		op.Attempts = MaxOperationAttempts
		f.store.operations[op.ID] = op
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyUnknownOperation, ChargeID: &c.ID, OperationID: &op.ID})
		f.gw.findResult = &ProviderCharge{ProviderChargeID: "pay_found", ExternalReference: c.ID.String(), Status: ProviderPending, Amount: brl(10000)}
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if f.operationFor(t, c.ID).Status != OpSucceeded || f.store.charges[c.ID].ProviderChargeID != "pay_found" || len(f.gw.createCalls) != 1 {
			t.Fatalf("operação resolvida pela consulta: %s", f.operationFor(t, c.ID).Status)
		}
	})
	t.Run("operação continua desconhecida", func(t *testing.T) {
		f := newPayFixture(t)
		inv := f.addInvoice(t, 10000)
		f.gw.createResults = []gwResult{{err: errAmbiguous}}
		c, _ := f.svc.RequestCharge(context.Background(), f.tenantID, inv.ID, f.actor)
		op := f.operationFor(t, c.ID)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyUnknownOperation, ChargeID: &c.ID, OperationID: &op.ID})
		f.gw.findErr = errAmbiguous
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "operation_still_unknown")
	})
	t.Run("divergência sem operação", func(t *testing.T) {
		f := newPayFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyUnknownOperation})
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
	})
	t.Run("falha ao reconsultar", func(t *testing.T) {
		f := newPayFixture(t)
		opID := uuid.New()
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyUnknownOperation, OperationID: &opID})
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindNotFound, "operation_not_found")
	})
	t.Run("cobrança inexistente no provedor é cancelada", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyMissingProvider, ChargeID: &c.ID})
		f.gw.getErr = notFoundAtProvider
		if _, err := f.resolve(d, ResolutionAcceptProvider); err != nil {
			t.Fatal(err)
		}
		if f.store.charges[c.ID].Status != ChargeCanceled || !slices.Contains(f.auditActions(), "charge.canceled_missing_at_provider") {
			t.Fatalf("cancelada: %s", f.store.charges[c.ID].Status)
		}
	})
	t.Run("cobrança encontrada no provedor", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyMissingProvider, ChargeID: &c.ID})
		f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderPending, Amount: brl(10000)}
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "provider_charge_found")
	})
	t.Run("cobrança paga ausente no provedor", func(t *testing.T) {
		f, _, c := paidFixture(t)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyMissingProvider, ChargeID: &c.ID})
		f.gw.getErr = notFoundAtProvider
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "resolution_not_applicable")
	})
}

func TestResolveDiscrepancyStoreFailures(t *testing.T) {
	for _, m := range []string{"Discrepancy", "Charge", "Account", "UpdateDiscrepancy", "Audit", "PostLedger", "UpdateCharge"} {
		t.Run(m, func(t *testing.T) {
			f, _, c := paidFixture(t)
			d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyFeeMismatch, ChargeID: &c.ID, LocalAmount: mp(199), ProviderAmount: mp(150)})
			boom := errors.New(m)
			f.store.errs[m] = boom
			if _, err := f.resolve(d, ResolutionAcceptProvider); !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
			if f.store.discrepancies[d.ID].Status != DiscrepancyOpen {
				t.Fatal("sem efeito parcial")
			}
		})
	}
	t.Run("divergência resolvida durante a consulta", func(t *testing.T) {
		f := newPayFixture(t)
		_, c := awaitingCharge(t, f)
		d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyPaymentNotReflected, ChargeID: &c.ID})
		f.gw.getResult = *providerSnapshot(ProviderReceived, "RECEIVED", 10000, 199, nil)
		f.gw.onGet = func() {
			x := f.store.discrepancies[d.ID]
			x.Status, x.Resolution = DiscrepancyDismissed, ResolutionDismiss
			f.store.discrepancies[d.ID] = x
		}
		_, err := f.resolve(d, ResolutionAcceptProvider)
		wantAppErr(t, err, apperr.KindConflict, "discrepancy_not_open")
	})
	t.Run("falha ao encerrar reembolso", func(t *testing.T) {
		for _, m := range []string{"ChargeRefunds", "ChargeOperations", "UpdateRefund"} {
			t.Run(m, func(t *testing.T) {
				f, _, c := paidFixture(t)
				f.gw.refundResults = []refundRes{{err: errAmbiguous}}
				r := f.refund(t, c.ID, 3000, "")
				d := f.addDiscrepancy(t, Discrepancy{Kind: DiscrepancyRefundPending, ChargeID: &c.ID, RefundID: &r.ID})
				f.gw.getResult = ProviderCharge{ProviderChargeID: c.ProviderChargeID, Status: ProviderReceived, Amount: brl(10000)}
				boom := errors.New(m)
				f.gw.onGet = func() { f.store.errs[m] = boom }
				if _, err := f.resolve(d, ResolutionAcceptProvider); !errors.Is(err, boom) {
					t.Fatalf("erro: %v", err)
				}
			})
		}
	})
}
