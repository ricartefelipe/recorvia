package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// recoveryFixture devolve a disputa perdida de cobrança paga com 25,00 de
// excedente, dos quais o cliente já havia usado 20,00: 5,00 de crédito
// consumido e 20,00 a recuperar do cliente (ADR-070).
func recoveryFixture(t *testing.T) (*payFixture, Dispute) {
	t.Helper()
	f, c := overpaidFixture(t)
	f.useCredit(t, 2000)
	if w := f.lose(t, c, "evt_lost", 1); w.Status != "processed" {
		t.Fatalf("webhook: %s (%s)", w.Status, w.Error)
	}
	d := f.latestDispute(t, c.ID)
	if d.Status != DisputeLost || d.RecoveryAmount.Amount() != 2000 || d.CreditAmount.Amount() != 500 {
		t.Fatalf("pré-condição: disputa perdida com valor a recuperar: %+v", d)
	}
	f.store.events, f.store.audits = nil, nil
	return f, d
}

func recoveryInput(d Dispute, outcome RecoveryOutcome, amount int64, key string) RecoverySettlementInput {
	return RecoverySettlementInput{DisputeID: d.ID, Outcome: outcome, Amount: brl(amount),
		Justification: "Cliente quitou por transferência identificada no extrato.", RequestKey: key}
}

func (f *payFixture) settle(t *testing.T, in RecoverySettlementInput) (RecoverySettlement, Dispute) {
	t.Helper()
	st, d, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, in)
	if err != nil {
		t.Fatalf("baixa: %v", err)
	}
	return st, d
}

func settlementLedger(f *payFixture) []ledger.Transaction {
	var out []ledger.Transaction
	for _, t := range f.store.posted {
		if t.ReferenceType == ledger.RecoverySettlementReference {
			out = append(out, t)
		}
	}
	return out
}

func eventsOfType(f *payFixture, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range f.store.events {
		if e.Type == typ {
			out = append(out, e.Payload.(map[string]any))
		}
	}
	return out
}

// A baixa tira o valor de customer_recoverables: a recebida leva ao
// disponível liquidado e a perdida às perdas com inadimplência. Baixas
// parciais se acumulam até o valor a recuperar; o ledger fica balanceado e a
// conta zera quando tudo foi baixado.
func TestSettleRecoveryReceivedAndWrittenOff(t *testing.T) {
	type step struct {
		outcome RecoveryOutcome
		amount  int64
	}
	cases := []struct {
		name       string
		steps      []step
		received   int64
		writtenOff int64
		left       int64
	}{
		{name: "recebido integralmente", steps: []step{{RecoveryReceived, 2000}}, received: 2000},
		{name: "perdido integralmente", steps: []step{{RecoveryWrittenOff, 2000}}, writtenOff: 2000},
		{name: "recebido em parte", steps: []step{{RecoveryReceived, 1200}}, received: 1200, left: 800},
		{name: "recebido em parte e o restante perdido", steps: []step{{RecoveryReceived, 1500}, {RecoveryWrittenOff, 500}},
			received: 1500, writtenOff: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d := recoveryFixture(t)
			var last Dispute
			for i, s := range tc.steps {
				st, got := f.settle(t, recoveryInput(d, s.outcome, s.amount, "chave-"+string(rune('a'+i))))
				if st.DisputeID != d.ID || st.Outcome != s.outcome || st.Amount.Amount() != s.amount || st.TenantID != f.tenantID ||
					st.SettledBy == nil || *st.SettledBy != f.userID || st.LedgerTransactionID == uuid.Nil || !st.CreatedAt.Equal(payNow) ||
					st.EffectiveDate != calendar.DateOf(payNow, saoPaulo) || st.Justification != "Cliente quitou por transferência identificada no extrato." {
					t.Fatalf("baixa: %+v", st)
				}
				last = got
			}
			if last.RecoveryReceived.Amount() != tc.received || last.RecoveryWrittenOff.Amount() != tc.writtenOff || last.RecoveryAmount.Amount() != 2000 {
				t.Fatalf("totais da disputa: %+v", last)
			}
			if left, err := last.RecoveryOutstanding(); err != nil || left.Amount() != tc.left {
				t.Fatalf("saldo a recuperar: %v %v", left, err)
			}
			posted := settlementLedger(f)
			if len(posted) != len(tc.steps) {
				t.Fatalf("lançamentos das baixas: %d", len(posted))
			}
			for i, p := range posted {
				if err := p.Validate(); err != nil {
					t.Fatalf("lançamento %s desbalanceado: %v", p.Kind, err)
				}
				want := "dispute.recovery_received"
				if tc.steps[i].outcome == RecoveryWrittenOff {
					want = "dispute.recovery_written_off"
				}
				if p.Kind != want || p.ReferenceID != f.store.settlements[i].ID || p.ID != f.store.settlements[i].LedgerTransactionID {
					t.Fatalf("lançamento %d: %s ref=%v", i, p.Kind, p.ReferenceID)
				}
			}
			if net := ledgerNet(posted); net[ledger.Cash] != tc.received || net[ledger.BadDebt] != tc.writtenOff ||
				net[ledger.CustomerRecoverables] != -(tc.received+tc.writtenOff) {
				t.Fatalf("partidas das baixas: %v", net)
			}
			// Ciclo inteiro: o que resta em customer_recoverables é o saldo
			// a recuperar da disputa; chargebacks não muda com a baixa.
			if net := ledgerNet(f.store.posted); net[ledger.CustomerRecoverables] != tc.left || net[ledger.Chargebacks] != 10000 {
				t.Fatalf("ledger: %v", net)
			}
			events := eventsOfType(f, outbox.EventDisputeRecoverySettled)
			if len(events) != len(tc.steps) {
				t.Fatalf("eventos: %d", len(events))
			}
			lastEvent := events[len(events)-1]
			if lastEvent["dispute_id"] != d.ID || lastEvent["charge_id"] != d.ChargeID || lastEvent["recovery_outstanding_amount"] != tc.left ||
				lastEvent["recovery_amount"] != int64(2000) || lastEvent["customer_id"] != f.customer || lastEvent["effective_date"] != "2026-11-16" {
				t.Fatalf("evento: %v", lastEvent)
			}
			audits := f.store.audits
			if len(audits) != len(tc.steps) {
				t.Fatalf("auditoria: %d", len(audits))
			}
			a := audits[len(audits)-1]
			if a.Action != "dispute.recovery_settled" || a.ResourceType != "dispute" || a.ResourceID != d.ID.String() || a.Actor != f.actor ||
				a.Details["justification"] == "" || a.Details["recovery_outstanding_amount"] != tc.left {
				t.Fatalf("auditoria: %+v", a)
			}
			if len(f.store.disputeLocks) != len(tc.steps) || f.store.disputeLocks[0] != d.ID {
				t.Fatalf("a baixa bloqueia a disputa: %v", f.store.disputeLocks)
			}
		})
	}
}

// Pela credencial de API a baixa fica sem autoria na tabela e com o ator
// api_credential na auditoria (ADR-059); a data efetiva informada é mantida e
// a data padrão é a de hoje no fuso do tenant.
func TestSettleRecoveryAuthorshipAndEffectiveDate(t *testing.T) {
	f, d := recoveryFixture(t)
	cred := audit.APICredential(uuid.New(), "ERP interno")
	in := recoveryInput(d, RecoveryReceived, 700, "")
	st, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, cred, nil, in)
	if err != nil || st.SettledBy != nil {
		t.Fatalf("baixa por credencial: %+v %v", st, err)
	}
	if a := f.store.audits[len(f.store.audits)-1]; a.Actor.Type != audit.ActorAPICredential {
		t.Fatalf("ator da auditoria: %+v", a.Actor)
	}
	// 01:00 UTC de 17/11 ainda é 16/11 em São Paulo.
	f.clock.Set(time.Date(2026, time.November, 17, 1, 0, 0, 0, time.UTC))
	st, _ = f.settle(t, recoveryInput(d, RecoveryReceived, 300, ""))
	if st.EffectiveDate.String() != "2026-11-16" {
		t.Fatalf("data efetiva padrão: %s", st.EffectiveDate)
	}
	f.clock.Set(time.Date(2026, time.November, 20, 15, 0, 0, 0, time.UTC))
	date := calendar.Date{Year: 2026, Month: time.November, Day: 18}
	in = recoveryInput(d, RecoveryWrittenOff, 100, "")
	in.EffectiveDate = &date
	if st, _ = f.settle(t, in); st.EffectiveDate != date {
		t.Fatalf("data efetiva informada: %s", st.EffectiveDate)
	}
	// O ledger usa o instante do registro, não a data efetiva.
	if posted := settlementLedger(f); !posted[len(posted)-1].OccurredAt.Equal(f.clock.Now()) {
		t.Fatalf("instante do lançamento: %v", posted[len(posted)-1].OccurredAt)
	}
}

// A repetição com a mesma chave devolve a baixa registrada sem lançar,
// emitir ou auditar de novo; a chave usada em outra baixa é recusada.
func TestSettleRecoveryIsIdempotentByRequestKey(t *testing.T) {
	f, d := recoveryFixture(t)
	first, _ := f.settle(t, recoveryInput(d, RecoveryReceived, 1000, "chave-baixa-1"))
	posted, events, audits := len(f.store.posted), len(f.store.events), len(f.store.audits)
	again, got := f.settle(t, recoveryInput(d, RecoveryReceived, 1000, "chave-baixa-1"))
	if again.ID != first.ID || got.RecoveryReceived.Amount() != 1000 {
		t.Fatalf("repetição: %+v %+v", again, got)
	}
	if len(f.store.posted) != posted || len(f.store.events) != events || len(f.store.audits) != audits || len(f.store.settlements) != 1 {
		t.Fatal("a repetição não pode lançar, emitir nem auditar de novo")
	}
	for name, in := range map[string]RecoverySettlementInput{
		"outro valor":    recoveryInput(d, RecoveryReceived, 900, "chave-baixa-1"),
		"outro desfecho": recoveryInput(d, RecoveryWrittenOff, 1000, "chave-baixa-1"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, in)
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != "request_key_reused" {
				t.Fatalf("chave reutilizada: %v", err)
			}
		})
	}
}

// Pedidos inválidos são recusados com o campo indicado, sem nenhum efeito.
func TestSettleRecoveryValidatesInput(t *testing.T) {
	future := calendar.Date{Year: 2026, Month: time.November, Day: 17}
	beforeClose := calendar.Date{Year: 2026, Month: time.November, Day: 15}
	cases := []struct {
		name   string
		mutate func(in *RecoverySettlementInput)
		field  string
	}{
		{"desfecho desconhecido", func(in *RecoverySettlementInput) { in.Outcome = "partial" }, "outcome"},
		{"valor zero", func(in *RecoverySettlementInput) { in.Amount = brl(0) }, "amount"},
		{"valor negativo", func(in *RecoverySettlementInput) { in.Amount = brl(-10) }, "amount"},
		{"justificativa curta", func(in *RecoverySettlementInput) { in.Justification = "  curta  " }, "justification"},
		{"justificativa longa", func(in *RecoverySettlementInput) { in.Justification = strings.Repeat("a", 2001) }, "justification"},
		{"justificativa com NUL", func(in *RecoverySettlementInput) { in.Justification = "justificativa\x00inválida" }, "justification"},
		{"chave longa", func(in *RecoverySettlementInput) { in.RequestKey = strings.Repeat("k", 301) }, "request_key"},
		{"data no futuro", func(in *RecoverySettlementInput) { in.EffectiveDate = &future }, "effective_date"},
		{"data antes do encerramento", func(in *RecoverySettlementInput) { in.EffectiveDate = &beforeClose }, "effective_date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d := recoveryFixture(t)
			in := recoveryInput(d, RecoveryReceived, 500, "")
			tc.mutate(&in)
			_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, in)
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Kind != apperr.KindInvalid || len(ae.Fields) != 1 || ae.Fields[0].Field != tc.field {
				t.Fatalf("esperava campo %s inválido: %v", tc.field, err)
			}
			if len(f.store.settlements) != 0 || len(settlementLedger(f)) != 0 || len(f.store.events) != 0 || len(f.store.audits) != 0 {
				t.Fatal("pedido inválido não pode ter efeito")
			}
		})
	}
}

// A baixa só vale para disputa perdida com valor a recuperar e nunca passa
// do saldo; disputa de outro tenant não é encontrada.
func TestSettleRecoveryRejectsWhatCannotBeSettled(t *testing.T) {
	t.Run("disputa sem valor a recuperar", func(t *testing.T) {
		f, c := overpaidFixture(t)
		f.lose(t, c, "evt_lost", 1) // crédito intacto: tudo consumido, nada a recuperar
		d := f.latestDispute(t, c.ID)
		_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, recoveryInput(d, RecoveryReceived, 100, ""))
		assertConflict(t, err, "dispute_without_recovery")
	})
	t.Run("disputa aberta", func(t *testing.T) {
		f, c := overpaidFixture(t)
		f.sendSnapshot(t, "evt_open", chargebackSnapshot(c.ProviderChargeID, ProviderChargebackRequested, payNow.Add(time.Hour)))
		d := f.latestDispute(t, c.ID)
		_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, recoveryInput(d, RecoveryWrittenOff, 100, ""))
		assertConflict(t, err, "dispute_without_recovery")
	})
	t.Run("valor acima do saldo", func(t *testing.T) {
		f, d := recoveryFixture(t)
		f.settle(t, recoveryInput(d, RecoveryReceived, 1500, ""))
		_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, recoveryInput(d, RecoveryWrittenOff, 501, ""))
		assertConflict(t, err, "recovery_exceeds_outstanding")
		if len(f.store.settlements) != 1 || len(settlementLedger(f)) != 1 {
			t.Fatal("a baixa recusada não pode ter efeito")
		}
	})
	t.Run("saldo já baixado", func(t *testing.T) {
		f, d := recoveryFixture(t)
		f.settle(t, recoveryInput(d, RecoveryWrittenOff, 2000, ""))
		_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, recoveryInput(d, RecoveryReceived, 1, ""))
		assertConflict(t, err, "recovery_already_settled")
	})
	t.Run("disputa de outro tenant", func(t *testing.T) {
		f, d := recoveryFixture(t)
		_, _, err := f.svc.SettleRecovery(context.Background(), uuid.New(), f.actor, &f.userID, recoveryInput(d, RecoveryReceived, 100, ""))
		if !apperr.IsKind(err, apperr.KindNotFound) {
			t.Fatalf("disputa de outro tenant: %v", err)
		}
		if _, _, err := f.svc.RecoverySettlements(context.Background(), uuid.New(), d.ID); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Fatalf("listagem em outro tenant: %v", err)
		}
		if _, err := f.svc.Dispute(context.Background(), uuid.New(), d.ID); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Fatalf("leitura em outro tenant: %v", err)
		}
	})
}

func assertConflict(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Kind != apperr.KindConflict || ae.Code != code {
		t.Fatalf("esperava conflito %s: %v", code, err)
	}
}

// Falha em qualquer etapa desfaz a transação inteira: nenhuma baixa,
// lançamento, evento ou auditoria fica gravado.
func TestSettleRecoveryRollsBackOnFailure(t *testing.T) {
	for _, step := range []string{"Dispute", "RecoverySettlementByRequestKey", "Location", "PostLedger", "InsertRecoverySettlement", "Charge",
		"Emit", "Audit"} {
		t.Run(step, func(t *testing.T) {
			f, d := recoveryFixture(t)
			posted := len(f.store.posted)
			boom := errors.New("falha de " + step)
			f.store.errs[step] = boom
			_, _, err := f.svc.SettleRecovery(context.Background(), f.tenantID, f.actor, &f.userID, recoveryInput(d, RecoveryReceived, 500, "chave"))
			if !errors.Is(err, boom) {
				t.Fatalf("erro: %v", err)
			}
			if len(f.store.settlements) != 0 || len(f.store.posted) != posted || len(f.store.events) != 0 || len(f.store.audits) != 0 {
				t.Fatal("a falha deixou efeitos")
			}
			if got := f.store.disputes[d.ID]; !got.RecoveryReceived.IsZero() {
				t.Fatalf("totais da disputa depois da falha: %+v", got)
			}
		})
	}
}

// A listagem devolve as baixas da disputa em ordem de registro, com a
// disputa atualizada.
func TestRecoverySettlementsListsInOrder(t *testing.T) {
	f, d := recoveryFixture(t)
	first, _ := f.settle(t, recoveryInput(d, RecoveryReceived, 600, ""))
	f.clock.Advance(time.Minute)
	second, _ := f.settle(t, recoveryInput(d, RecoveryWrittenOff, 400, ""))
	got, list, err := f.svc.RecoverySettlements(context.Background(), f.tenantID, d.ID)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("baixas: %+v %v", list, err)
	}
	if left, _ := got.RecoveryOutstanding(); left.Amount() != 1000 {
		t.Fatalf("saldo a recuperar: %v", left)
	}
	if one, err := f.svc.Dispute(context.Background(), f.tenantID, d.ID); err != nil || one.RecoveryWrittenOff.Amount() != 400 {
		t.Fatalf("disputa: %+v %v", one, err)
	}
	f.store.errs["RecoverySettlements"] = errors.New("banco indisponível")
	if _, _, err := f.svc.RecoverySettlements(context.Background(), f.tenantID, d.ID); err == nil {
		t.Fatal("a falha da leitura deve ser propagada")
	}
}

// Sem baixas, o saldo a recuperar é o próprio valor a recuperar, inclusive
// para disputa lida sem os totais inicializados.
func TestDisputeRecoveryOutstanding(t *testing.T) {
	cases := []struct {
		name string
		d    Dispute
		want int64
	}{
		{"sem valores", Dispute{Amount: brl(100)}, 0},
		{"sem baixas", Dispute{Amount: brl(100), RecoveryAmount: brl(40)}, 40},
		{"com baixas", Dispute{Amount: brl(100), RecoveryAmount: brl(40), RecoveryReceived: brl(15), RecoveryWrittenOff: brl(5)}, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.d.RecoveryOutstanding(); err != nil || got.Amount() != tc.want {
				t.Fatalf("saldo: %v %v", got, err)
			}
		})
	}
	if !RecoveryReceived.Valid() || !RecoveryWrittenOff.Valid() || RecoveryOutcome("lost").Valid() {
		t.Fatal("desfechos conhecidos")
	}
}
