package platformbilling

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

type fixture struct {
	store    *fakeStore
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	actor    audit.Actor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := newFakeStore()
	store.plans["essencial"] = essencial()
	store.plans["escala"] = escala()
	store.addons["implantacao_assistida"] = Addon{Code: "implantacao_assistida", Name: "Implantação assistida",
		Price: money.New(250000, money.BRL), Kind: "one_time", Active: true}
	store.addons["legado"] = Addon{Code: "legado", Price: money.New(1000, money.BRL), Kind: "one_time", Active: false}
	c := clock.NewManual(utc(2026, time.November, 20, 12, 0))
	f := &fixture{store: store, clock: c, svc: NewService(store, c), tenantID: uuid.New(), actor: audit.User(uuid.New(), "Ana")}
	st := store.tenant(f.tenantID)
	st.loc = saoPaulo(t)
	st.hasSub = true
	st.sub = Subscription{TenantID: f.tenantID, PlanCode: "escala", Status: StatusActive,
		Anchor:      calendar.Date{Year: 2026, Month: time.November, Day: 16},
		PeriodStart: utc(2026, time.November, 16, 3, 0), PeriodEnd: utc(2026, time.December, 16, 3, 0), Version: 4}
	st.usage = Usage{ActiveUsers: 2, ActiveSubscriptions: 40}
	return f
}

func (f *fixture) state() *tenantState { return f.store.tenant(f.tenantID) }

func TestOverview(t *testing.T) {
	f := newFixture(t)
	ov, err := f.svc.Overview(context.Background(), f.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Plan.Code != "escala" || ov.Subscription.Version != 4 || ov.Usage.ActiveSubscriptions != 40 {
		t.Fatalf("visão geral inesperada: %+v", ov)
	}
	f.state().pending = 2
	if ov, err = f.svc.Overview(context.Background(), f.tenantID); err != nil || ov.Usage.PendingInvitations != 2 || ov.Usage.ReservedUsers() != 4 {
		t.Fatalf("convites pendentes devem constar no consumo: %+v %v", ov.Usage, err)
	}

	for _, op := range []string{"Subscription", "Plan", "Usage", "PendingInvitations"} {
		t.Run("falha em "+op, func(t *testing.T) {
			f := newFixture(t)
			f.store.fail[op] = errBoom
			if _, err := f.svc.Overview(context.Background(), f.tenantID); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
		})
	}
}

func TestPlansAndAddons(t *testing.T) {
	f := newFixture(t)
	plans, err := f.svc.Plans(context.Background())
	if err != nil || len(plans) != 2 {
		t.Fatalf("Plans = %v, %v", plans, err)
	}
	addons, err := f.svc.Addons(context.Background())
	if err != nil || len(addons) != 2 {
		t.Fatalf("Addons = %v, %v", addons, err)
	}
	f.store.fail["Plans"], f.store.fail["Addons"] = errBoom, errBoom
	if _, err := f.svc.Plans(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("Plans deveria propagar o erro: %v", err)
	}
	if _, err := f.svc.Addons(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("Addons deveria propagar o erro: %v", err)
	}
}

func TestChangePlanSamePlanHasNoEffect(t *testing.T) {
	f := newFixture(t)
	// Nem o plano de destino é consultado: a troca para o mesmo plano é neutra.
	f.store.fail["Plan"] = errBoom
	sub, err := f.svc.ChangePlan(context.Background(), f.tenantID, f.actor, "escala")
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanCode != "escala" || sub.Version != 4 {
		t.Fatalf("assinatura alterada: %+v", sub)
	}
	if st := f.state(); st.updates != 0 || len(st.audits) != 0 {
		t.Fatalf("não deveria gravar nem auditar: updates=%d audits=%d", st.updates, len(st.audits))
	}
}

func TestChangePlanSuccess(t *testing.T) {
	f := newFixture(t)
	f.state().pending = 1 // dois membros e um convite cabem nos três usuários do plano
	sub, err := f.svc.ChangePlan(context.Background(), f.tenantID, f.actor, "essencial")
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanCode != "essencial" || sub.Version != 5 {
		t.Fatalf("assinatura inesperada: %+v", sub)
	}
	st := f.state()
	if st.sub.PlanCode != "essencial" || st.updates != 1 {
		t.Fatalf("assinatura não gravada: %+v", st.sub)
	}
	// O período corrente é mantido: a mudança vale a partir dele.
	if !st.sub.PeriodStart.Equal(utc(2026, time.November, 16, 3, 0)) {
		t.Errorf("período alterado: %v", st.sub.PeriodStart)
	}
	if len(st.audits) != 1 {
		t.Fatalf("esperada uma entrada de auditoria, obtidas %d", len(st.audits))
	}
	e := st.audits[0]
	if e.Action != "platform.plan_changed" || e.Actor != f.actor || e.Details["from"] != "escala" || e.Details["to"] != "essencial" ||
		e.ResourceID != f.tenantID.String() || !e.OccurredAt.Equal(f.clock.Now()) {
		t.Fatalf("auditoria inesperada: %+v", e)
	}
}

func TestChangePlanRejected(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fixture)
		plan  string
		kind  apperr.Kind
		code  string
	}{
		{name: "plano inexistente", plan: "ouro", kind: apperr.KindNotFound, code: "platform_plan_not_found"},
		{name: "plano inativo", plan: "antigo", kind: apperr.KindInvalid, code: "platform_plan_inactive", setup: func(f *fixture) {
			p := escala()
			p.Code, p.Active = "antigo", false
			f.store.plans["antigo"] = p
		}},
		{name: "downgrade com usuários acima do limite", plan: "essencial", kind: apperr.KindConflict, code: "platform_downgrade_users",
			setup: func(f *fixture) { f.state().usage.ActiveUsers = 4 }},
		// Dois membros e dois convites pendentes não cabem nos três usuários
		// do plano: aceitos depois da troca, passariam do limite.
		{name: "downgrade com convites pendentes acima do limite", plan: "essencial", kind: apperr.KindConflict, code: "platform_downgrade_users",
			setup: func(f *fixture) { f.state().pending = 2 }},
		{name: "downgrade com contratos acima do limite", plan: "essencial", kind: apperr.KindConflict, code: "platform_downgrade_subscriptions",
			setup: func(f *fixture) { f.state().usage.ActiveSubscriptions = 150 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, err := f.svc.ChangePlan(context.Background(), f.tenantID, f.actor, tc.plan)
			assertAppErr(t, err, tc.kind, tc.code)
			if st := f.state(); st.sub.PlanCode != "escala" || st.updates != 0 || len(st.audits) != 0 {
				t.Fatalf("nada deveria ser alterado: %+v", st.sub)
			}
		})
	}
}

func TestChangePlanStoreErrors(t *testing.T) {
	for _, op := range []string{"Subscription", "Plan", "Usage", "PendingInvitations", "UpdateSubscription", "Audit"} {
		t.Run(op, func(t *testing.T) {
			f := newFixture(t)
			f.store.fail[op] = errBoom
			if _, err := f.svc.ChangePlan(context.Background(), f.tenantID, f.actor, "essencial"); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
		})
	}
}

func TestRequestAddon(t *testing.T) {
	f := newFixture(t)
	requester := uuid.New()
	order, err := f.svc.RequestAddon(context.Background(), f.tenantID, f.actor, requester, "implantacao_assistida", "Migrar 300 contratos")
	if err != nil {
		t.Fatal(err)
	}
	if order.ID == uuid.Nil || order.TenantID != f.tenantID || order.AddonCode != "implantacao_assistida" ||
		order.Status != "requested" || order.Notes != "Migrar 300 contratos" || order.Price.Amount() != 250000 ||
		order.RequestedBy != requester || !order.CreatedAt.Equal(f.clock.Now()) {
		t.Fatalf("pedido inesperado: %+v", order)
	}
	st := f.state()
	if len(st.orders) != 1 || st.orders[0].ID != order.ID {
		t.Fatalf("pedido não gravado: %+v", st.orders)
	}
	if len(st.audits) != 1 || st.audits[0].Action != "platform.addon_requested" || st.audits[0].ResourceID != order.ID.String() ||
		st.audits[0].Details["addon"] != "implantacao_assistida" {
		t.Fatalf("auditoria inesperada: %+v", st.audits)
	}
	// Observações não vão para a auditoria (podem conter dados pessoais).
	if _, ok := st.audits[0].Details["notes"]; ok {
		t.Error("observações não deveriam constar na auditoria")
	}
}

func TestRequestAddonNotesLimitCountsCharacters(t *testing.T) {
	f := newFixture(t)
	// 2.000 caracteres acentuados ocupam 4.000 bytes e ainda são aceitos.
	if _, err := f.svc.RequestAddon(context.Background(), f.tenantID, f.actor, uuid.New(), "implantacao_assistida",
		strings.Repeat("ç", 2000)); err != nil {
		t.Fatalf("2.000 caracteres deveriam ser aceitos: %v", err)
	}

	f = newFixture(t)
	f.store.fail["InTenant"] = errBoom // a validação acontece antes de abrir a transação
	_, err := f.svc.RequestAddon(context.Background(), f.tenantID, f.actor, uuid.New(), "implantacao_assistida", strings.Repeat("a", 2001))
	assertAppErr(t, err, apperr.KindInvalid, "addon_notes_too_long")
}

func TestRequestAddonRejected(t *testing.T) {
	cases := []struct {
		name string
		code string
		kind apperr.Kind
		want string
	}{
		{"serviço inativo", "legado", apperr.KindInvalid, "addon_inactive"},
		{"serviço inexistente", "consultoria", apperr.KindNotFound, "addon_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.RequestAddon(context.Background(), f.tenantID, f.actor, uuid.New(), tc.code, "")
			assertAppErr(t, err, tc.kind, tc.want)
			if len(f.state().orders) != 0 || len(f.state().audits) != 0 {
				t.Fatal("nenhum pedido deveria ser gravado")
			}
		})
	}
	for _, op := range []string{"Addon", "InsertAddonOrder", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			f := newFixture(t)
			f.store.fail[op] = errBoom
			if _, err := f.svc.RequestAddon(context.Background(), f.tenantID, f.actor, uuid.New(), "implantacao_assistida", ""); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
		})
	}
}

// dueFixture prepara uma assinatura ativa com três períodos encerrados até
// 20/11/2026: [16/08, 16/09), [16/09, 16/10) e [16/10, 16/11).
func dueFixture(t *testing.T) *fixture {
	f := newFixture(t)
	f.store.due = []uuid.UUID{f.tenantID}
	st := f.state()
	st.sub.PlanCode = "essencial"
	st.sub.Anchor = calendar.Date{Year: 2026, Month: time.August, Day: 16}
	st.sub.PeriodStart, st.sub.PeriodEnd = utc(2026, time.August, 16, 3, 0), utc(2026, time.September, 16, 3, 0)
	return f
}

func statementStarts(sts []Statement) []time.Time {
	out := make([]time.Time, len(sts))
	for i, s := range sts {
		out[i] = s.PeriodStart
	}
	return out
}

func TestCloseDueStatementsCatchUp(t *testing.T) {
	f := dueFixture(t)
	// Concluído no segundo período (setembro a outubro): cobrado no
	// demonstrativo desse período e vinculado a ele.
	done := utc(2026, time.October, 2, 15, 0)
	f.state().orders = []AddonOrder{
		{ID: uuid.New(), AddonCode: "implantacao_assistida", Status: AddonCompleted, Price: money.New(250000, money.BRL), CompletedAt: &done, Version: 3},
		{ID: uuid.New(), AddonCode: "implantacao_assistida", Status: AddonScheduled, Price: money.New(250000, money.BRL), Version: 2},
	}

	n, err := f.svc.CloseDueStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("esperados 3 demonstrativos, obtidos %d", n)
	}
	if !f.store.dueAt.Equal(f.clock.Now()) {
		t.Errorf("consulta de tenants deveria usar o instante do relógio: %v", f.store.dueAt)
	}
	st := f.state()
	want := []time.Time{utc(2026, time.August, 16, 3, 0), utc(2026, time.September, 16, 3, 0), utc(2026, time.October, 16, 3, 0)}
	if got := statementStarts(st.statements); !slices.Equal(got, want) {
		t.Fatalf("períodos fechados = %v, esperado %v", got, want)
	}
	totals := []int64{19900, 269900, 19900}
	for i, s := range st.statements {
		if s.Total.Amount() != totals[i] || s.PlanCode != "essencial" || s.Status != "open" || !s.CreatedAt.Equal(f.clock.Now()) {
			t.Errorf("demonstrativo %d inesperado: %+v", i, s)
		}
		if i > 0 && !s.PeriodStart.Equal(st.statements[i-1].PeriodEnd) {
			t.Errorf("demonstrativo %d não é contíguo ao anterior", i)
		}
	}
	if !st.sub.PeriodStart.Equal(utc(2026, time.November, 16, 3, 0)) || !st.sub.PeriodEnd.Equal(utc(2026, time.December, 16, 3, 0)) {
		t.Fatalf("assinatura deveria estar no período corrente: [%v, %v)", st.sub.PeriodStart, st.sub.PeriodEnd)
	}
	if st.updates != 1 {
		t.Errorf("assinatura deveria ser gravada uma vez, gravada %d", st.updates)
	}
	if id := st.orders[0].StatementID; id == nil || *id != st.statements[1].ID {
		t.Fatalf("o pedido concluído deve ficar vinculado ao demonstrativo que o cobrou: %v", id)
	}
	if st.orders[1].StatementID != nil {
		t.Fatal("pedido não concluído não é cobrado")
	}
}

func TestCloseDueStatementsIsIdempotent(t *testing.T) {
	t.Run("demonstrativo já existente não é contado", func(t *testing.T) {
		f := dueFixture(t)
		f.state().existing[utc(2026, time.September, 16, 3, 0)] = true
		n, err := f.svc.CloseDueStatements(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("esperados 2 novos demonstrativos, obtidos %d", n)
		}
		if !f.state().sub.PeriodStart.Equal(utc(2026, time.November, 16, 3, 0)) {
			t.Fatalf("a assinatura deve avançar mesmo quando o demonstrativo já existe: %v", f.state().sub.PeriodStart)
		}
	})
	t.Run("segunda execução não fecha nada", func(t *testing.T) {
		f := dueFixture(t)
		if _, err := f.svc.CloseDueStatements(context.Background()); err != nil {
			t.Fatal(err)
		}
		n, err := f.svc.CloseDueStatements(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 || len(f.state().statements) != 3 {
			t.Fatalf("segunda execução fechou %d (total %d)", n, len(f.state().statements))
		}
	})
}

func TestCloseDueStatementsTrial(t *testing.T) {
	f := dueFixture(t)
	st := f.state()
	st.sub.Status = StatusTrialing
	// A avaliação cobre os dois primeiros períodos inteiros e termina no
	// meio do terceiro.
	st.sub.TrialEndsAt = ptr(utc(2026, time.October, 30, 3, 0))
	n, err := f.svc.CloseDueStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("esperados 3 demonstrativos, obtidos %d", n)
	}
	amounts := []int64{0, 0, 19900}
	for i, s := range st.statements {
		if s.SubscriptionAmount.Amount() != amounts[i] {
			t.Errorf("demonstrativo %d: plano = %d, esperado %d", i, s.SubscriptionAmount.Amount(), amounts[i])
		}
	}
	if st.sub.Status != StatusActive {
		t.Errorf("avaliação vencida deveria ativar a assinatura, situação %s", st.sub.Status)
	}
}

func TestCloseDueStatementsSkipsCanceled(t *testing.T) {
	f := dueFixture(t)
	f.state().sub.Status = StatusCanceled
	n, err := f.svc.CloseDueStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(f.state().statements) != 0 {
		t.Fatalf("assinatura cancelada não gera demonstrativos: %d", n)
	}
	if !f.state().sub.PeriodStart.Equal(utc(2026, time.August, 16, 3, 0)) {
		t.Fatal("assinatura cancelada não deveria avançar")
	}
}

func TestCloseDueStatementsLimitsWorkPerTransaction(t *testing.T) {
	f := dueFixture(t)
	st := f.state()
	st.sub.Anchor = calendar.Date{Year: 2024, Month: time.May, Day: 16}
	st.sub.PeriodStart, st.sub.PeriodEnd = utc(2024, time.May, 16, 3, 0), utc(2024, time.June, 16, 3, 0) // 30 períodos atrasados
	n, err := f.svc.CloseDueStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 24 {
		t.Fatalf("esperado limite de 24 períodos por transação, obtidos %d", n)
	}
	if !st.sub.PeriodStart.Equal(utc(2026, time.May, 16, 3, 0)) {
		t.Fatalf("assinatura deveria parar no 25º período: %v", st.sub.PeriodStart)
	}
	// A próxima execução recupera o restante.
	n, err = f.svc.CloseDueStatements(context.Background())
	if err != nil || n != 6 {
		t.Fatalf("segunda execução: %d, %v; esperados 6", n, err)
	}
}

func TestCloseDueStatementsMultipleTenants(t *testing.T) {
	f := dueFixture(t)
	other := uuid.New()
	ost := f.store.tenant(other)
	ost.loc, ost.hasSub = saoPaulo(t), true
	ost.sub = Subscription{TenantID: other, PlanCode: "escala", Status: StatusActive,
		Anchor:      calendar.Date{Year: 2026, Month: time.October, Day: 1},
		PeriodStart: utc(2026, time.October, 1, 3, 0), PeriodEnd: utc(2026, time.November, 1, 3, 0)}
	f.store.due = append(f.store.due, other)

	n, err := f.svc.CloseDueStatements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || len(ost.statements) != 1 || ost.statements[0].Total.Amount() != 129900 {
		t.Fatalf("esperados 3+1 demonstrativos, obtidos %d (%+v)", n, ost.statements)
	}
	if ost.statements[0].TenantID != other {
		t.Errorf("demonstrativo atribuído ao tenant errado: %s", ost.statements[0].TenantID)
	}
}

func TestCloseDueStatementsStopsWhenContextCanceled(t *testing.T) {
	f := dueFixture(t)
	other := uuid.New()
	ost := f.store.tenant(other)
	ost.loc, ost.hasSub = saoPaulo(t), true
	ost.sub = Subscription{PlanCode: "escala", Anchor: calendar.Date{Year: 2026, Month: time.October, Day: 1},
		PeriodStart: utc(2026, time.October, 1, 3, 0), PeriodEnd: utc(2026, time.November, 1, 3, 0)}
	f.store.due = append(f.store.due, other)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.store.cancel, f.store.cancelAfter = cancel, 1

	n, err := f.svc.CloseDueStatements(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("esperado context.Canceled, obtido %v", err)
	}
	if n != 3 || len(ost.statements) != 0 {
		t.Fatalf("deveria parar após o primeiro tenant: n=%d segundo=%d", n, len(ost.statements))
	}
}

func TestCloseDueStatementsErrors(t *testing.T) {
	for _, op := range []string{"TenantsDueStatement", "InTenant", "TenantLocation", "Subscription", "Plan",
		"UnbilledAddonsTotal", "InsertStatement", "MarkAddonsBilled", "UpdateSubscription"} {
		t.Run(op, func(t *testing.T) {
			f := dueFixture(t)
			f.store.fail[op] = errBoom
			if _, err := f.svc.CloseDueStatements(context.Background()); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
		})
	}
	t.Run("adicionais em outra moeda", func(t *testing.T) {
		f := dueFixture(t)
		done := utc(2026, time.August, 20, 3, 0)
		f.state().orders = []AddonOrder{{ID: uuid.New(), Status: AddonCompleted, Price: money.New(1, money.Currency{}), CompletedAt: &done}}
		if _, err := f.svc.CloseDueStatements(context.Background()); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperado ErrCurrencyMismatch, obtido %v", err)
		}
	})
	t.Run("fuso ausente impede o avanço", func(t *testing.T) {
		f := dueFixture(t)
		f.state().loc = nil
		if _, err := f.svc.CloseDueStatements(context.Background()); err == nil {
			t.Fatal("esperado erro ao avançar sem fuso")
		}
	})
}
