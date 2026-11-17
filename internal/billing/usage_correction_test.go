package billing

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// As assinaturas de teste começam em 01/10/2026; o período de consumo
// corrigido é 01/10/2026 a 31/10/2026, já faturado em 01/11/2026.
var (
	correctionStart = local(2026, time.October, 1, 0, 0)
	correctionNow   = local(2026, time.November, 16, 14, 30)
)

type correctionFixture struct {
	store     *advancedFake
	clock     *clock.Manual
	corrector *UsageCorrector
	tenantID  uuid.UUID
	metered   offer // R$ 0,02 por requisição
	metric    catalog.Metric
}

func newCorrectionFixture(t *testing.T) *correctionFixture {
	t.Helper()
	f := &correctionFixture{store: newAdvancedFake(), clock: clock.NewManual(correctionNow), tenantID: uuid.New()}
	f.metered = newOffer("api", "Plano API", calendar.Monthly, catalog.Usage, 2)
	f.store.plans[f.metered.plan.ID] = f.metered.plan
	f.store.prices[f.metered.price.ID] = f.metered.price
	f.metric = catalog.Metric{ID: *f.metered.price.MetricID, Code: "api_calls", Name: "Requisições", Unit: "requisicao",
		Aggregation: catalog.AggSum, Status: catalog.Active}
	f.store.metrics[f.metric.ID] = f.metric
	f.corrector = NewUsageCorrector(f.store, f.clock)
	return f
}

// addSub cria uma assinatura ativa do tenant com os períodos 0 e 1 faturados.
func (f *correctionFixture) addSub(t *testing.T, tenantID uuid.UUID, mutate func(*subscriptions.Subscription)) subscriptions.Subscription {
	t.Helper()
	sub, err := subscriptions.New(subscriptions.NewInput{ID: uuid.New(), TenantID: tenantID, CustomerID: uuid.New(), Plan: f.metered.plan,
		Price: f.metered.price, Quantity: 1, StartAt: correctionStart, Location: saoPaulo, Now: correctionStart})
	if err != nil {
		t.Fatal(err)
	}
	sched, err := sub.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := sub.ApplyStep(subscriptions.BillingStep{Kind: subscriptions.StepBill, Index: i, Period: sched.Period(i)}, sched.Period(i).Start); err != nil {
			t.Fatal(err)
		}
	}
	sub.PopEvents()
	if mutate != nil {
		mutate(&sub)
	}
	f.store.subs = append(f.store.subs, sub)
	return sub
}

// addLate registra eventos atrasados do período 0: billed é a quantidade
// faturada no fechamento (item de R$ 0,02 por unidade) e recount a
// quantidade recalculada com todos os eventos.
func (f *correctionFixture) addLate(t *testing.T, sub subscriptions.Subscription, billed, recount int64) (LateGroup, ClosedAggregate) {
	t.Helper()
	sched, err := sub.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	period := sched.Period(0)
	g := LateGroup{SubscriptionID: sub.ID, MetricID: f.metric.ID, PeriodStart: period.Start}
	itemID, priceID := uuid.New(), f.metered.price.ID
	agg := ClosedAggregate{ID: uuid.New(), SubscriptionID: sub.ID, MetricID: f.metric.ID, Period: period, Quantity: big.NewRat(billed, 1),
		InvoiceItemID: &itemID, PriceVersionID: &priceID}
	f.store.itemAmounts[itemID] = brl(billed * 2)
	f.store.aggregates[lateKey(g)] = agg
	f.store.late[sub.TenantID] = append(f.store.late[sub.TenantID], g)
	f.store.recompute[lateKey(g)] = recomputed{qty: big.NewRat(recount, 1), events: 3}
	if !slices.Contains(f.store.lateTenants, sub.TenantID) {
		f.store.lateTenants = append(f.store.lateTenants, sub.TenantID)
	}
	return g, agg
}

func (f *correctionFixture) run(t *testing.T) int {
	t.Helper()
	n, err := f.corrector.RunAll(context.Background())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	return n
}

func (f *correctionFixture) correctionAudit(t *testing.T) audit.Entry {
	t.Helper()
	for _, a := range f.store.audits {
		if a.Action == "usage.corrected" {
			return a
		}
	}
	t.Fatalf("auditoria da correção ausente: %v", auditActions(f.store.audits))
	return audit.Entry{}
}

const correctionDesc = "Correção de consumo de Requisições: 100 para 150 requisicao (01/10/2026 a 31/10/2026)"

func TestUsageCorrectorOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*subscriptions.Subscription)
		recount     int64
		wantOutcome string
		wantDelta   int64
	}{
		{"aumento em assinatura ativa vira item pendente", nil, 150, "next_invoice", 100},
		{"aumento em assinatura inadimplente vira item pendente", func(s *subscriptions.Subscription) { s.Status = subscriptions.PastDue },
			150, "next_invoice", 100},
		{"aumento com cancelamento agendado é faturado imediatamente", func(s *subscriptions.Subscription) { s.CancelAtPeriodEnd = true },
			150, "invoiced_now", 100},
		{"aumento em assinatura encerrada é faturado imediatamente", func(s *subscriptions.Subscription) { s.Status = subscriptions.Canceled },
			150, "invoiced_now", 100},
		{"redução vira crédito do cliente", nil, 80, "customer_credit", -40},
		{"quantidade igual não tem efeito", nil, 100, "no_effect", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			sub := f.addSub(t, f.tenantID, c.mutate)
			g, agg := f.addLate(t, sub, 100, c.recount)

			if n := f.run(t); n != 1 {
				t.Fatalf("grupos corrigidos: %d", n)
			}
			stored := f.store.aggregates[lateKey(g)]
			if stored.Quantity.Cmp(big.NewRat(100, 1)) != 0 || stored.InvoiceItemID == nil || *stored.InvoiceItemID != *agg.InvoiceItemID {
				t.Fatal("a quantidade e o item faturados no fechamento não mudam")
			}
			if stored.CorrectedQuantity.Cmp(big.NewRat(c.recount, 1)) != 0 || stored.CorrectedAmount.Amount() != c.recount*2 {
				t.Fatalf("correção acumulada no agregado: %v %v", stored.CorrectedQuantity, stored.CorrectedAmount)
			}
			if len(f.store.late[f.tenantID]) != 0 || len(f.store.marks) != 1 || f.store.marks[0].aggregateID != agg.ID ||
				!f.store.marks[0].at.Equal(correctionNow) {
				t.Fatalf("eventos atrasados marcados como corrigidos: %+v", f.store.marks)
			}
			a := f.correctionAudit(t)
			if a.Actor != audit.System("billing.usage_correction") || a.ResourceID != sub.ID.String() ||
				a.Details["outcome"] != c.wantOutcome || a.Details["delta"] != c.wantDelta || a.Details["events"] != 3 ||
				a.Details["billed_quantity"] != "100" || a.Details["corrected_quantity"] != money.RatString(big.NewRat(c.recount, 1)) ||
				a.Details["previous_amount"] != int64(200) || a.Details["price_version_id"] != f.metered.price.ID {
				t.Fatalf("auditoria da correção: %+v", a.Details)
			}

			switch c.wantOutcome {
			case "next_invoice":
				if len(f.store.newPending) != 1 || len(f.store.invoices) != 0 || len(f.store.grants) != 0 || len(f.store.posted) != 0 {
					t.Fatalf("somente item pendente: pendentes=%d faturas=%d", len(f.store.newPending), len(f.store.invoices))
				}
				it := f.store.newPending[0]
				if it.Kind != ItemAdjustment || it.Source != "usage_correction" || it.Amount.Amount() != 100 || it.CustomerID != sub.CustomerID ||
					it.SubscriptionID != sub.ID || !samePeriod(*it.Period, agg.Period) || *it.UsageAggregateID != agg.ID || it.Description != correctionDesc {
					t.Fatalf("item pendente da correção: %+v", it)
				}
				if f.store.marks[0].itemID == nil || *f.store.marks[0].itemID != it.ID {
					t.Fatal("os eventos atrasados apontam para o item de ajuste")
				}
			case "invoiced_now":
				if len(f.store.invoices) != 1 || len(f.store.newPending) != 0 || f.store.marks[0].itemID != nil {
					t.Fatalf("fatura imediata: faturas=%d pendentes=%d", len(f.store.invoices), len(f.store.newPending))
				}
				inv := f.store.invoices[0]
				if inv.Kind != KindManual || inv.Status != Open || inv.Number != "FAT-2026-000001" || *inv.SubscriptionID != sub.ID ||
					!samePeriod(*inv.BillingPeriod, agg.Period) || inv.Total.Amount() != 100 || !inv.DueAt.Equal(DueAt(correctionNow, 5, saoPaulo)) {
					t.Fatalf("fatura da correção: %+v", inv)
				}
				it := inv.Items[0]
				if len(inv.Items) != 1 || it.Kind != ItemAdjustment || it.Amount.Amount() != 100 || it.Quantity.Cmp(big.NewRat(1, 1)) != 0 ||
					it.Description != correctionDesc || it.PriceSnapshot["aggregate_id"] != agg.ID {
					t.Fatalf("item da fatura de correção: %+v", it)
				}
				if _, ok := it.PriceSnapshot["outcome"]; ok {
					t.Fatal("o snapshot do item é congelado antes do resultado")
				}
				if a.Details["invoice_id"] != inv.ID {
					t.Fatalf("auditoria referencia a fatura: %+v", a.Details)
				}
				if net := ledgerNet(t, f.store.posted); net[ledger.AccountsReceivable] != 100 || net[ledger.BilledRevenue] != -100 {
					t.Fatalf("ledger da fatura de correção: %+v", net)
				}
				if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceIssued {
					t.Fatalf("evento de emissão: %+v", f.store.events)
				}
				if acts := auditActions(f.store.audits); !equalStrings(acts, []string{"invoice.issued", "usage.corrected"}) ||
					f.store.audits[0].Actor != audit.System("billing.usage_correction") {
					t.Fatalf("auditorias: %v", acts)
				}
			case "customer_credit":
				if len(f.store.grants) != 1 || len(f.store.invoices) != 0 || len(f.store.newPending) != 0 {
					t.Fatalf("somente crédito: %+v", f.store.grants)
				}
				g := f.store.grants[0]
				if g.amount.Amount() != 40 || g.kind != "usage_correction" || g.customerID != sub.CustomerID ||
					g.reason != "Correção de consumo de Requisições: 100 para 80 requisicao (01/10/2026 a 31/10/2026)" {
					t.Fatalf("crédito da correção: %+v", g)
				}
				if net := ledgerNet(t, f.store.posted); net[ledger.RevenueAdjustments] != 40 || net[ledger.CustomerCredits] != -40 {
					t.Fatalf("ledger do crédito: %+v", net)
				}
				if f.store.credit[sub.CustomerID].Amount() != 40 {
					t.Fatal("o crédito fica disponível ao cliente")
				}
			case "no_effect":
				if len(f.store.newPending) != 0 || len(f.store.invoices) != 0 || len(f.store.grants) != 0 || len(f.store.posted) != 0 {
					t.Fatal("sem diferença não há ajuste")
				}
			}
		})
	}
}

func TestUsageCorrectorPricesWithClosingVersion(t *testing.T) {
	f := newCorrectionFixture(t)
	newer := f.metered.price
	newer.ID, newer.Version, newer.UnitAmount = uuid.New(), 2, big.NewRat(3, 1)
	f.store.prices[newer.ID] = newer
	sub := f.addSub(t, f.tenantID, func(s *subscriptions.Subscription) { s.PriceVersionID = newer.ID })
	f.addLate(t, sub, 100, 150)

	f.run(t)
	if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != 100 {
		t.Fatalf("a diferença usa o preço do fechamento (R$ 0,02), não o atual: %+v", f.store.newPending)
	}
	if d := f.correctionAudit(t).Details; d["price_version_id"] != f.metered.price.ID || d["corrected_amount"] != int64(300) {
		t.Fatalf("versão de preço da correção: %+v", d)
	}
}

func TestUsageCorrectorWithoutBilledItemUsesCurrentPrice(t *testing.T) {
	f := newCorrectionFixture(t)
	sub := f.addSub(t, f.tenantID, nil)
	g, _ := f.addLate(t, sub, 0, 50)
	agg := f.store.aggregates[lateKey(g)]
	agg.InvoiceItemID, agg.PriceVersionID = nil, nil
	f.store.aggregates[lateKey(g)] = agg

	f.run(t)
	if f.store.calls["InvoiceItemAmount"] != 0 {
		t.Fatal("agregado sem item faturado não consulta o valor do item")
	}
	if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != 100 {
		t.Fatalf("todo o consumo recalculado é ajuste: %+v", f.store.newPending)
	}
	if d := f.correctionAudit(t).Details; d["previous_amount"] != int64(0) || d["price_version_id"] != sub.PriceVersionID {
		t.Fatalf("sem versão do fechamento usa a versão da assinatura: %+v", d)
	}
}

func TestUsageCorrectorAccumulatesCorrections(t *testing.T) {
	f := newCorrectionFixture(t)
	sub := f.addSub(t, f.tenantID, nil)
	g, _ := f.addLate(t, sub, 100, 150)
	f.run(t)

	// Novos eventos atrasados reduzem o total recalculado do mesmo período.
	f.store.late[f.tenantID] = []LateGroup{g}
	f.store.recompute[lateKey(g)] = recomputed{qty: big.NewRat(120, 1), events: 5}
	f.clock.Advance(time.Hour)
	f.store.audits = nil
	f.run(t)

	if len(f.store.newPending) != 1 || f.store.newPending[0].Amount.Amount() != 100 {
		t.Fatalf("o primeiro ajuste permanece: %+v", f.store.newPending)
	}
	if len(f.store.grants) != 1 || f.store.grants[0].amount.Amount() != 60 {
		t.Fatalf("a segunda correção compara com o valor já corrigido (R$ 3,00 para R$ 2,40): %+v", f.store.grants)
	}
	d := f.correctionAudit(t).Details
	if d["billed_quantity"] != "100" || d["previous_quantity"] != "150" || d["corrected_quantity"] != "120" ||
		d["previous_amount"] != int64(300) || d["corrected_amount"] != int64(240) || d["delta"] != int64(-60) {
		t.Fatalf("auditoria da correção acumulada: %+v", d)
	}
	if agg := f.store.aggregates[lateKey(g)]; agg.CorrectedAmount.Amount() != 240 || agg.CorrectedQuantity.Cmp(big.NewRat(120, 1)) != 0 {
		t.Fatalf("agregado com a última correção: %+v", agg)
	}

	// Repetir sem novos eventos não gera outro ajuste.
	f.store.late[f.tenantID] = []LateGroup{g}
	f.store.audits = nil
	f.run(t)
	if len(f.store.grants) != 1 || len(f.store.newPending) != 1 || f.correctionAudit(t).Details["outcome"] != "no_effect" {
		t.Fatal("correção repetida não pode gerar novo ajuste")
	}
}

func TestUsageCorrectorImmediateInvoiceTerms(t *testing.T) {
	cases := []struct {
		name     string
		customer *int
		sub      *int
		wantDays int
	}{
		{"prazo do tenant", nil, nil, 5},
		{"prazo do cliente", intPtr(10), nil, 10},
		{"prazo da assinatura", intPtr(10), intPtr(15), 15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			f.store.customerTerms = c.customer
			sub := f.addSub(t, f.tenantID, func(s *subscriptions.Subscription) {
				s.Status, s.PaymentTermsDays = subscriptions.Canceled, c.sub
			})
			f.store.credit[sub.CustomerID] = brl(30)
			f.addLate(t, sub, 100, 150)
			f.run(t)
			inv := f.store.invoices[0]
			if !inv.DueAt.Equal(DueAt(correctionNow, c.wantDays, saoPaulo)) {
				t.Fatalf("vencimento %v, esperado prazo de %d dias", inv.DueAt, c.wantDays)
			}
			if inv.CreditApplied.Amount() != 30 || inv.AmountDue.Amount() != 70 {
				t.Fatalf("crédito do cliente aplicado na emissão: %v %v", inv.CreditApplied, inv.AmountDue)
			}
		})
	}
}

func TestUsageCorrectorRunAllIsolatesTenantFailures(t *testing.T) {
	f := newCorrectionFixture(t)
	okA, broken, down, okB := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	subA := f.addSub(t, okA, nil)
	f.addLate(t, subA, 100, 150)
	subBroken := f.addSub(t, broken, nil)
	f.addLate(t, subBroken, 100, 150)
	subMissing := f.addSub(t, broken, nil)
	gMissing, _ := f.addLate(t, subMissing, 100, 150)
	delete(f.store.aggregates, lateKey(gMissing))
	subDown := f.addSub(t, down, nil)
	f.addLate(t, subDown, 100, 150)
	boom := errors.New("banco indisponível")
	f.store.failTenant[down] = boom
	subB := f.addSub(t, okB, nil)
	f.addLate(t, subB, 100, 80)

	n, err := f.corrector.RunAll(context.Background())
	if n != 2 {
		t.Fatalf("os tenants saudáveis são corrigidos: %d", n)
	}
	if !errors.Is(err, boom) || !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("os erros dos tenants são devolvidos juntos: %v", err)
	}
	for _, want := range []string{"tenant " + broken.String(), "consumo da assinatura " + subMissing.ID.String(), "tenant " + down.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("erro deve conter %q: %v", want, err)
		}
	}
	if len(f.store.newPending) != 1 || f.store.newPending[0].SubscriptionID != subA.ID {
		t.Fatalf("a correção já feita no tenant com falha é desfeita: %+v", f.store.newPending)
	}
	if len(f.store.grants) != 1 || f.store.grants[0].customerID != subB.CustomerID {
		t.Fatalf("o tenant seguinte é processado: %+v", f.store.grants)
	}
	if len(f.store.late[broken]) != 2 || len(f.store.late[down]) != 1 {
		t.Fatal("os eventos dos tenants com falha continuam pendentes")
	}
	g := LateGroup{SubscriptionID: subBroken.ID, MetricID: f.metric.ID, PeriodStart: correctionStart}
	if f.store.aggregates[lateKey(g)].CorrectedAmount != nil {
		t.Fatal("o agregado do tenant com falha não pode ficar corrigido")
	}
}

func TestUsageCorrectorRespectsBatchSize(t *testing.T) {
	f := newCorrectionFixture(t)
	f.corrector.BatchSize = 1
	f.addLate(t, f.addSub(t, f.tenantID, nil), 100, 150)
	f.addLate(t, f.addSub(t, f.tenantID, nil), 100, 150)

	if n := f.run(t); n != 1 || len(f.store.late[f.tenantID]) != 1 {
		t.Fatalf("no máximo BatchSize grupos por transação: %d", n)
	}
	if n := f.run(t); n != 1 || len(f.store.late[f.tenantID]) != 0 {
		t.Fatalf("o restante é corrigido na próxima execução: %d", n)
	}
	if NewUsageCorrector(f.store, f.clock).BatchSize != 200 {
		t.Fatal("lote padrão de 200 grupos")
	}
}

func TestUsageCorrectorRunAllErrors(t *testing.T) {
	t.Run("falha ao listar tenants", func(t *testing.T) {
		f := newCorrectionFixture(t)
		boom := errors.New("consulta falhou")
		f.store.lateTenantsErr = boom
		if n, err := f.corrector.RunAll(context.Background()); n != 0 || !errors.Is(err, boom) {
			t.Fatalf("esperava %v, obtive %d %v", boom, n, err)
		}
	})
	t.Run("falha ao listar grupos", func(t *testing.T) {
		f := newCorrectionFixture(t)
		f.addLate(t, f.addSub(t, f.tenantID, nil), 100, 150)
		boom := errors.New("consulta falhou")
		f.store.errs["LateUsageGroups"] = boom
		n, err := f.corrector.RunAll(context.Background())
		if n != 0 || !errors.Is(err, boom) || !strings.Contains(err.Error(), f.tenantID.String()) {
			t.Fatalf("erro identifica o tenant: %d %v", n, err)
		}
	})
	t.Run("cancelamento", func(t *testing.T) {
		f := newCorrectionFixture(t)
		f.addLate(t, f.addSub(t, f.tenantID, nil), 100, 150)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.corrector.RunAll(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("esperava cancelamento: %v", err)
		}
		if f.store.txCount != 0 {
			t.Fatal("nenhuma transação após cancelamento")
		}
	})
	t.Run("cancelamento preserva erros anteriores", func(t *testing.T) {
		f := newCorrectionFixture(t)
		first, second := uuid.New(), uuid.New()
		f.addLate(t, f.addSub(t, first, nil), 100, 150)
		f.addLate(t, f.addSub(t, second, nil), 100, 150)
		ctx, cancel := context.WithCancel(context.Background())
		boom := errors.New("tenant indisponível")
		f.store.failTenant[first] = boom
		f.store.failTenant[second] = errors.New("não deveria ser consultado")
		f.corrector = NewUsageCorrector(cancelingStore{advancedFake: f.store, cancel: cancel}, f.clock)
		_, err := f.corrector.RunAll(ctx)
		if !errors.Is(err, boom) || !errors.Is(err, context.Canceled) {
			t.Fatalf("erro do primeiro tenant e cancelamento: %v", err)
		}
		if f.store.txCount != 1 {
			t.Fatalf("o segundo tenant não é processado após o cancelamento: %d", f.store.txCount)
		}
	})
}

// cancelingStore cancela o contexto ao fim de cada transação.
type cancelingStore struct {
	*advancedFake
	cancel context.CancelFunc
}

func (s cancelingStore) InCorrectionTx(ctx context.Context, tenantID uuid.UUID, fn func(CorrectionTx) error) error {
	defer s.cancel()
	return s.advancedFake.InCorrectionTx(ctx, tenantID, fn)
}

func TestUsageCorrectorRollsBackOnFailure(t *testing.T) {
	pending := func(*subscriptions.Subscription) {}
	now := func(s *subscriptions.Subscription) { s.Status = subscriptions.Canceled }
	cases := []struct {
		name    string
		mutate  func(*subscriptions.Subscription)
		recount int64
		method  string
		nth     int
	}{
		{"agregado", pending, 150, "ClosedAggregate", 0},
		{"assinatura", pending, 150, "Subscription", 0},
		{"métrica", pending, 150, "Metric", 0},
		{"versão de preço", pending, 150, "PriceVersion", 0},
		{"recálculo", pending, 150, "RecomputeUsage", 0},
		{"valor faturado", pending, 150, "InvoiceItemAmount", 0},
		{"configuração do tenant", pending, 150, "TenantSettings", 0},
		{"item pendente", pending, 150, "InsertPendingItem", 0},
		{"correção do agregado", pending, 150, "SetAggregateCorrection", 0},
		{"marcação dos eventos", pending, 150, "MarkLateCorrected", 0},
		{"auditoria da correção", pending, 150, "Audit", 0},
		{"dados do cliente", now, 150, "CustomerForInvoice", 0},
		{"saldo de crédito", now, 150, "CustomerCreditBalance", 0},
		{"numeração", now, 150, "NextInvoiceNumber", 0},
		{"gravação da fatura", now, 150, "InsertInvoice", 0},
		{"histórico da fatura", now, 150, "InsertStatusHistory", 0},
		{"lançamento da fatura", now, 150, "PostLedger", 0},
		{"evento da fatura", now, 150, "Emit", 0},
		{"auditoria da fatura", now, 150, "Audit", 1},
		{"auditoria após a fatura", now, 150, "Audit", 2},
		{"concessão do crédito", pending, 80, "AddCustomerCredit", 0},
		{"lançamento do crédito", pending, 80, "PostLedger", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCorrectionFixture(t)
			sub := f.addSub(t, f.tenantID, c.mutate)
			g, _ := f.addLate(t, sub, 100, c.recount)
			boom := errors.New("falha em " + c.method)
			f.store.errs[c.method] = boom
			if c.nth > 0 {
				f.store.failAt[c.method] = c.nth
			}

			n, err := f.corrector.RunAll(context.Background())
			if n != 0 || !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %d %v", boom, n, err)
			}
			if len(f.store.newPending) != 0 || len(f.store.invoices) != 0 || len(f.store.grants) != 0 || len(f.store.posted) != 0 ||
				len(f.store.audits) != 0 || len(f.store.events) != 0 || len(f.store.marks) != 0 || f.store.seq != 0 {
				t.Fatal("falha deve desfazer toda a correção")
			}
			if f.store.aggregates[lateKey(g)].CorrectedAmount != nil || len(f.store.late[f.tenantID]) != 1 {
				t.Fatal("os eventos atrasados continuam pendentes")
			}
			if f.store.credit[sub.CustomerID].Amount() != 0 {
				t.Fatal("nenhum crédito pode restar")
			}
		})
	}
}
