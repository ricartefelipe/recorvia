package subscriptions

import (
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// priceFlat cria uma versão de preço fixa do plano com o valor em centavos.
func priceFlat(plan catalog.Plan, cents int64) catalog.PriceVersion {
	m := money.New(cents, money.BRL)
	return catalog.PriceVersion{ID: uuid.New(), PlanID: plan.ID, Version: 1, Model: catalog.Flat, FlatAmount: &m, Currency: money.BRL}
}

// pricePerUnit cria uma versão de preço por quantidade com o preço unitário
// em centavos (pode ser fracionário).
func pricePerUnit(plan catalog.Plan, unit *big.Rat) catalog.PriceVersion {
	return catalog.PriceVersion{ID: uuid.New(), PlanID: plan.ID, Version: 1, Model: catalog.PerUnit, UnitAmount: unit, Currency: money.BRL}
}

// priceUsage cria uma versão de preço somente por consumo.
func priceUsage(plan catalog.Plan) catalog.PriceVersion {
	metric := uuid.New()
	return catalog.PriceVersion{ID: uuid.New(), PlanID: plan.ID, Version: 1, Model: catalog.Usage, UnitAmount: big.NewRat(35, 100),
		MetricID: &metric, Currency: money.BRL}
}

// priceFlatPlusOverage cria uma versão de preço fixa com excedente de consumo.
func priceFlatPlusOverage(plan catalog.Plan, cents int64) catalog.PriceVersion {
	pv := priceUsage(plan)
	m := money.New(cents, money.BRL)
	pv.Model, pv.FlatAmount, pv.IncludedQuantity = catalog.FlatPlusOverage, &m, big.NewRat(1000, 1)
	return pv
}

// billedSubscription cria uma assinatura sem avaliação iniciada em start
// (data local no fuso de start) e fatura o primeiro período, como faz o motor.
func billedSubscription(t *testing.T, plan catalog.Plan, price catalog.PriceVersion, qty int, start time.Time) Subscription {
	t.Helper()
	s := mustNew(t, NewInput{ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), Plan: plan, Price: price,
		Quantity: qty, StartAt: start, Location: start.Location(), Now: start})
	billOnce(t, &s, start)
	s.PopEvents()
	return s
}

func fieldNames(err error) []string {
	e, ok := apperr.As(err)
	if !ok {
		return nil
	}
	out := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		out[i] = f.Field
	}
	return out
}

func TestEffectivePrice(t *testing.T) {
	plan := fixturePlan(calendar.Monthly)
	override := big.NewRat(10890, 1)
	cases := []struct {
		name      string
		price     catalog.PriceVersion
		override  *big.Rat
		wantFlat  *int64
		wantUnit  *big.Rat
		keepsUnit bool
	}{
		{"sem reajuste devolve o preço contratado", priceFlat(plan, 9900), nil, new(int64(9900)), nil, false},
		{"preço fixo usa o valor reajustado", priceFlat(plan, 9900), override, new(int64(10890)), nil, false},
		{"fixo com excedente reajusta só a mensalidade", priceFlatPlusOverage(plan, 9900), override, new(int64(10890)), big.NewRat(35, 100), true},
		{"preço por unidade usa o preço unitário reajustado", pricePerUnit(plan, big.NewRat(2500, 1)), override, nil, big.NewRat(10890, 1), false},
		{"preço por unidade mantém a fração de centavo do reajuste", pricePerUnit(plan, big.NewRat(49905, 10)), big.NewRat(548955, 100), nil,
			big.NewRat(548955, 100), false},
		{"preço por consumo não é reajustado", priceUsage(plan), override, nil, big.NewRat(35, 100), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := c.price
			var origFlat int64
			if orig.FlatAmount != nil {
				origFlat = orig.FlatAmount.Amount()
			}
			origUnit := new(big.Rat)
			if orig.UnitAmount != nil {
				origUnit.Set(orig.UnitAmount)
			}
			got := Subscription{PriceOverride: c.override}.EffectivePrice(c.price)
			if got.ID != orig.ID || got.Model != orig.Model || got.Currency != orig.Currency {
				t.Fatalf("identificação do preço não pode mudar: %+v", got)
			}
			if c.wantFlat != nil && (got.FlatAmount == nil || got.FlatAmount.Amount() != *c.wantFlat) {
				t.Fatalf("valor fixo: obtive %v, esperava %d", got.FlatAmount, *c.wantFlat)
			}
			if c.wantUnit != nil && (got.UnitAmount == nil || got.UnitAmount.Cmp(c.wantUnit) != 0) {
				t.Fatalf("preço unitário: obtive %v, esperava %v", got.UnitAmount, c.wantUnit)
			}
			if c.keepsUnit && got.UnitAmount != orig.UnitAmount {
				t.Fatal("o componente de consumo não pode ser substituído")
			}
			if c.override != nil && got.UnitAmount == c.override {
				t.Fatal("o preço efetivo não pode compartilhar o valor reajustado da assinatura")
			}
			if orig.FlatAmount != nil && orig.FlatAmount.Amount() != origFlat {
				t.Fatal("a versão de preço original não pode ser alterada")
			}
			if orig.UnitAmount != nil && orig.UnitAmount.Cmp(origUnit) != 0 {
				t.Fatal("o preço unitário original não pode ser alterado")
			}
		})
	}
}

func TestFixedBase(t *testing.T) {
	plan := fixturePlan(calendar.Monthly)
	cases := []struct {
		name  string
		price catalog.PriceVersion
		want  *big.Rat
	}{
		{"preço fixo", priceFlat(plan, 9900), big.NewRat(9900, 1)},
		{"fixo com excedente", priceFlatPlusOverage(plan, 4900), big.NewRat(4900, 1)},
		{"preço unitário inteiro", pricePerUnit(plan, big.NewRat(2500, 1)), big.NewRat(2500, 1)},
		{"preço unitário com meio centavo não é arredondado", pricePerUnit(plan, big.NewRat(49905, 10)), big.NewRat(49905, 10)},
		{"preço unitário abaixo de meio centavo não vira zero", pricePerUnit(plan, big.NewRat(35, 100)), big.NewRat(35, 100)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FixedBase(c.price)
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(c.want) != 0 {
				t.Fatalf("base: obtive %s, esperava %s centavos", got.RatString(), c.want.RatString())
			}
			if c.price.UnitAmount != nil && got == c.price.UnitAmount {
				t.Fatal("a base não pode compartilhar o preço unitário da versão")
			}
		})
	}

	t.Run("preço somente por consumo não tem componente fixo", func(t *testing.T) {
		_, err := FixedBase(priceUsage(plan))
		wantKind(t, err, apperr.KindInvalid, "price_without_fixed_component")
	})
}

type changeArgs struct {
	sub     Subscription
	current catalog.PriceVersion
	plan    catalog.Plan
	price   catalog.PriceVersion
	qty     int
	timing  ChangeTiming
}

func TestValidateChange(t *testing.T) {
	start := local(2026, time.April, 1, 12, 0)
	planA := fixturePlan(calendar.Monthly)
	priceA := priceFlat(planA, 9900)
	planB := fixturePlan(calendar.Monthly)
	priceB := priceFlat(planB, 19900)
	flatSub := billedSubscription(t, planA, priceA, 1, start)

	planU := fixturePlan(calendar.Monthly)
	priceU := pricePerUnit(planU, big.NewRat(2500, 1))
	unitSub := billedSubscription(t, planU, priceU, 3, start)
	perUnit := func(a *changeArgs) {
		a.sub, a.current, a.plan, a.price = unitSub, priceU, planU, priceU
	}

	yearly := fixturePlan(calendar.Yearly)
	priceYearly := priceFlat(yearly, 99000)

	cases := []struct {
		name   string
		mod    func(*changeArgs)
		kind   apperr.Kind
		code   string
		fields []string
	}{
		{name: "troca imediata entre planos fixos"},
		{name: "troca no próximo ciclo", mod: func(a *changeArgs) { a.timing = ChangeNextCycle }},
		{name: "assinatura em avaliação pode trocar", mod: func(a *changeArgs) { a.sub.Status = Trialing }},
		{name: "assinatura inadimplente pode trocar", mod: func(a *changeArgs) { a.sub.Status = PastDue }},
		{name: "nova versão de preço do mesmo plano", mod: func(a *changeArgs) { a.plan, a.price = planA, priceFlat(planA, 12900) }},
		{name: "mudança de quantidade por unidade", mod: func(a *changeArgs) { perUnit(a); a.qty = 5 }},
		{name: "plano atual arquivado aceita mudança de quantidade", mod: func(a *changeArgs) {
			perUnit(a)
			a.plan.Status, a.qty = catalog.Archived, 5
		}},
		{name: "quantidade no limite máximo", mod: func(a *changeArgs) { perUnit(a); a.qty = MaxQuantity }},
		{name: "plano com consumo no próximo ciclo", mod: func(a *changeArgs) { a.price, a.timing = priceUsage(planB), ChangeNextCycle }},

		{"assinatura cancelada", func(a *changeArgs) { a.sub.Status = Canceled }, apperr.KindConflict, "invalid_transition", nil},
		{"assinatura suspensa", func(a *changeArgs) { a.sub.Status = Suspended }, apperr.KindConflict, "subscription_suspended", nil},
		{"momento inválido", func(a *changeArgs) { a.timing = "tomorrow" }, apperr.KindInvalid, "invalid_timing", []string{"timing"}},
		{"momento não informado", func(a *changeArgs) { a.timing = "" }, apperr.KindInvalid, "invalid_timing", []string{"timing"}},
		{"cancelamento agendado", func(a *changeArgs) { a.sub.CancelAtPeriodEnd = true }, apperr.KindConflict, "cancel_scheduled", nil},
		{"plano de destino arquivado", func(a *changeArgs) { a.plan.Status = catalog.Archived }, apperr.KindInvalid, "invalid_change", []string{"plan_id"}},
		{"versão de preço de outro plano", func(a *changeArgs) { a.price = priceFlat(fixturePlan(calendar.Monthly), 100) },
			apperr.KindInvalid, "invalid_change", []string{"plan_id"}},
		{"periodicidade diferente", func(a *changeArgs) { a.plan, a.price = yearly, priceYearly }, apperr.KindInvalid, "invalid_change", []string{"plan_id"}},
		{"moeda diferente", func(a *changeArgs) { a.price.Currency = money.Currency{} }, apperr.KindInvalid, "invalid_change", []string{"plan_id"}},
		{"quantidade zero", func(a *changeArgs) { a.qty = 0 }, apperr.KindInvalid, "invalid_change", []string{"quantity"}},
		{"quantidade acima do limite", func(a *changeArgs) { perUnit(a); a.qty = MaxQuantity + 1 }, apperr.KindInvalid, "invalid_change", []string{"quantity"}},
		{"quantidade diferente de 1 em plano fixo", func(a *changeArgs) { a.qty = 2 }, apperr.KindInvalid, "invalid_change", []string{"quantity"}},
		{"inconsistências são reunidas", func(a *changeArgs) {
			a.plan, a.price, a.qty = yearly, priceYearly, 2
			a.plan.Status = catalog.Archived
		}, apperr.KindInvalid, "invalid_change", []string{"plan_id", "plan_id", "quantity"}},
		{"sem alteração de plano e quantidade", func(a *changeArgs) { a.plan, a.price = planA, priceA }, apperr.KindInvalid, "no_change", nil},
		{"sem alteração de quantidade por unidade", func(a *changeArgs) { perUnit(a); a.qty = 3 }, apperr.KindInvalid, "no_change", nil},
		{"imediata com consumo no preço atual", func(a *changeArgs) { a.current = priceFlatPlusOverage(planA, 9900) },
			apperr.KindInvalid, "immediate_change_requires_fixed_price", []string{"timing"}},
		{"imediata com consumo no novo preço", func(a *changeArgs) { a.price = priceUsage(planB) },
			apperr.KindInvalid, "immediate_change_requires_fixed_price", []string{"timing"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := changeArgs{sub: flatSub, current: priceA, plan: planB, price: priceB, qty: 1, timing: ChangeImmediately}
			if c.mod != nil {
				c.mod(&a)
			}
			err := a.sub.ValidateChange(a.current, a.plan, a.price, a.qty, a.timing)
			if c.code == "" {
				if err != nil {
					t.Fatalf("alteração deveria ser aceita: %v", err)
				}
				return
			}
			wantKind(t, err, c.kind, c.code)
			if got := fieldNames(err); !slices.Equal(got, c.fields) {
				t.Fatalf("campos: obtive %v, esperava %v", got, c.fields)
			}
		})
	}
}

func TestApplyChange(t *testing.T) {
	start := local(2026, time.April, 1, 12, 0)
	at := local(2026, time.April, 21, 10, 0)

	t.Run("troca de versão de preço descarta o reajuste", func(t *testing.T) {
		planA, planB := fixturePlan(calendar.Monthly), fixturePlan(calendar.Monthly)
		priceA, priceB := pricePerUnit(planA, big.NewRat(2500, 1)), pricePerUnit(planB, big.NewRat(3000, 1))
		s := billedSubscription(t, planA, priceA, 3, start)
		adjID := uuid.New()
		s.PriceOverride, s.PriceOverrideAdjustmentID = big.NewRat(2750, 1), &adjID
		version, status := s.Version, s.Status

		s.ApplyChange(planB, priceB, 5, "plan_changed", at, map[string]any{"timing": ChangeImmediately})

		if s.PlanID != planB.ID || s.PriceVersionID != priceB.ID || s.Quantity != 5 {
			t.Fatalf("plano, preço e quantidade devem mudar: %+v", s)
		}
		if s.PriceOverride != nil || s.PriceOverrideAdjustmentID != nil {
			t.Fatal("o reajuste da versão anterior não pode valer para a nova versão")
		}
		if s.Status != status || s.Version != version {
			t.Fatalf("a mudança não altera situação nem versão: %s %d", s.Status, s.Version)
		}
		ev := s.PopEvents()
		if len(ev) != 1 || ev[0].Event != "plan_changed" || ev[0].From != nil || ev[0].To != status || !ev[0].At.Equal(at) {
			t.Fatalf("histórico: %+v", ev)
		}
		want := map[string]any{
			"timing": ChangeImmediately, "from_plan_id": planA.ID, "to_plan_id": planB.ID,
			"from_price_version_id": priceA.ID, "to_price_version_id": priceB.ID, "from_quantity": 3, "to_quantity": 5,
		}
		for k, v := range want {
			if ev[0].Details[k] != v {
				t.Fatalf("detalhe %q: obtive %v, esperava %v", k, ev[0].Details[k], v)
			}
		}
		if len(ev[0].Details) != len(want) {
			t.Fatalf("detalhes inesperados: %+v", ev[0].Details)
		}
	})

	t.Run("mudança só de quantidade mantém o reajuste", func(t *testing.T) {
		plan := fixturePlan(calendar.Monthly)
		price := pricePerUnit(plan, big.NewRat(2500, 1))
		s := billedSubscription(t, plan, price, 3, start)
		adjID := uuid.New()
		s.PriceOverride, s.PriceOverrideAdjustmentID = big.NewRat(2750, 1), &adjID

		s.ApplyChange(plan, price, 4, "quantity_changed", at, nil)

		if s.Quantity != 4 || !isCents(s.PriceOverride, 2750) || *s.PriceOverrideAdjustmentID != adjID {
			t.Fatalf("o reajuste da mesma versão continua valendo: %+v", s)
		}
		ev := s.PopEvents()
		if len(ev) != 1 || ev[0].Details["from_quantity"] != 3 || ev[0].Details["to_quantity"] != 4 {
			t.Fatalf("histórico sem detalhes informados deve ser criado: %+v", ev)
		}
	})
}

func TestNextChangeAt(t *testing.T) {
	plan := fixturePlan(calendar.Monthly)
	s := billedSubscription(t, plan, fixturePrice(plan, catalog.Flat), 1, local(2026, time.January, 31, 12, 0))
	if got := s.NextChangeAt(); !got.Equal(local(2026, time.February, 28, 0, 0)) {
		t.Fatalf("mudança no próximo ciclo vale no fim do período corrente: %v", got)
	}
	in := newInput(local(2026, time.March, 10, 9, 0), 14, local(2026, time.March, 10, 9, 0))
	trial := mustNew(t, in)
	if got := trial.NextChangeAt(); !got.Equal(*trial.TrialEndsAt) {
		t.Fatalf("na avaliação, a mudança vale no fim da avaliação: %v", got)
	}
}

func TestRecordEventKeepsStatus(t *testing.T) {
	s := mustNew(t, newInput(local(2026, time.March, 10, 9, 0), 0, local(2026, time.March, 10, 9, 0)))
	s.PopEvents()
	at := local(2026, time.March, 11, 9, 0)
	s.RecordEvent("usage_corrected", at, nil)
	s.RecordEvent("note_added", at, map[string]any{"note": "ajuste comercial"})
	ev := s.PopEvents()
	if len(ev) != 2 || ev[0].Event != "usage_corrected" || ev[0].From != nil || ev[0].To != Active || ev[0].Details == nil {
		t.Fatalf("evento sem mudança de situação: %+v", ev)
	}
	if ev[1].Details["note"] != "ajuste comercial" || !ev[1].At.Equal(at) {
		t.Fatalf("detalhes devem ser preservados: %+v", ev[1])
	}
}

func TestCurrentPeriodBilled(t *testing.T) {
	plan := fixturePlan(calendar.Monthly)
	price := fixturePrice(plan, catalog.Flat)
	start := local(2026, time.January, 10, 9, 0)
	cases := []struct {
		name string
		make func(t *testing.T) Subscription
		want bool
	}{
		{"período faturado antecipadamente", func(t *testing.T) Subscription {
			return billedSubscription(t, plan, price, 1, start)
		}, true},
		{"inadimplente com período faturado", func(t *testing.T) Subscription {
			s := billedSubscription(t, plan, price, 1, start)
			if err := s.MarkPastDue(start.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			return s
		}, true},
		{"assinatura recém-criada ainda não faturada", func(t *testing.T) Subscription {
			return mustNew(t, newInput(start, 0, start))
		}, false},
		{"assinatura em avaliação", func(t *testing.T) Subscription {
			return mustNew(t, newInput(start, 14, start))
		}, false},
		{"reativada em período posterior à suspensão", func(t *testing.T) Subscription {
			s := billedSubscription(t, plan, price, 1, start)
			if err := s.Suspend("", local(2026, time.January, 20, 9, 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.Reactivate(local(2026, time.March, 15, 9, 0)); err != nil {
				t.Fatal(err)
			}
			return s
		}, false},
		{"reativada no mesmo período já faturado", func(t *testing.T) Subscription {
			s := billedSubscription(t, plan, price, 1, start)
			if err := s.Suspend("", local(2026, time.January, 20, 9, 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.Reactivate(local(2026, time.January, 25, 9, 0)); err != nil {
				t.Fatal(err)
			}
			return s
		}, true},
		{"renovada no período seguinte", func(t *testing.T) Subscription {
			s := billedSubscription(t, plan, price, 1, start)
			billOnce(t, &s, local(2026, time.February, 10, 0, 0))
			return s
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.make(t)
			got, err := s.CurrentPeriodBilled()
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("período faturado: obtive %v, esperava %v (índice %d, período %v)", got, c.want, s.NextBillingIndex, s.CurrentPeriod)
			}
		})
	}

	t.Run("fuso inválido é reportado", func(t *testing.T) {
		s := billedSubscription(t, plan, price, 1, start)
		s.Timezone = "America/Inexistente"
		if _, err := s.CurrentPeriodBilled(); err == nil {
			t.Fatal("esperava erro de fuso inválido")
		}
	})
}

func TestProrateChangeExamples(t *testing.T) {
	saoPaulo2018 := func(m time.Month, d, h, mi int) time.Time { return time.Date(2018, m, d, h, mi, 0, 0, saoPaulo) }
	cases := []struct {
		name               string
		interval           calendar.Interval
		start, at          time.Time
		oldCents, newCents int64
		remaining, days    int
		credit, charge     int64
		net                int64
		wantStart          time.Time
	}{
		{"exemplo documentado: 30 dias, troca no dia 21 de R$ 100,00 para R$ 160,00", calendar.Monthly,
			local(2026, time.April, 1, 12, 0), local(2026, time.April, 21, 10, 0), 10000, 16000,
			10, 30, 3333, 5333, 2000, local(2026, time.April, 21, 0, 0)},
		{"fevereiro com 28 dias e meio centavo arredondado para o par", calendar.Monthly,
			local(2026, time.February, 1, 12, 0), local(2026, time.February, 15, 9, 0), 9901, 19903,
			14, 28, 4950, 9952, 5002, local(2026, time.February, 15, 0, 0)},
		{"fevereiro bissexto com 29 dias e redução de plano", calendar.Monthly,
			local(2028, time.February, 1, 12, 0), local(2028, time.February, 20, 16, 0), 10000, 5000,
			10, 29, 3448, 1724, -1724, local(2028, time.February, 20, 0, 0)},
		{"mês de 31 dias conforme a política documentada", calendar.Monthly,
			local(2026, time.March, 1, 12, 0), local(2026, time.March, 15, 15, 0), 10000, 20000,
			17, 31, 5484, 10968, 5484, local(2026, time.March, 15, 0, 0)},
		{"troca no primeiro instante do período", calendar.Monthly,
			local(2026, time.April, 1, 12, 0), local(2026, time.April, 1, 0, 0), 10000, 16000,
			30, 30, 10000, 16000, 6000, local(2026, time.April, 1, 0, 0)},
		{"troca no último dia do período", calendar.Monthly,
			local(2026, time.April, 1, 12, 0), local(2026, time.April, 30, 23, 59), 10000, 16000,
			1, 30, 333, 533, 200, local(2026, time.April, 30, 0, 0)},
		{"dia civil no fuso do contrato e não em UTC", calendar.Monthly,
			local(2026, time.April, 1, 12, 0), local(2026, time.April, 20, 22, 30), 10000, 16000,
			11, 30, 3667, 5867, 2200, local(2026, time.April, 20, 0, 0)},
		{"período anual de 365 dias", calendar.Yearly,
			local(2026, time.January, 10, 12, 0), local(2026, time.July, 1, 9, 0), 120000, 240000,
			193, 365, 63452, 126904, 63452, local(2026, time.July, 1, 0, 0)},
		{"início do horário de verão em Nova York (período de 743 horas)", calendar.Monthly,
			time.Date(2026, time.March, 1, 12, 0, 0, 0, newYork), time.Date(2026, time.March, 8, 12, 0, 0, 0, newYork), 10000, 16000,
			24, 31, 7742, 12387, 4645, time.Date(2026, time.March, 8, 0, 0, 0, 0, newYork)},
		{"fim do horário de verão em Nova York (período de 745 horas)", calendar.Monthly,
			time.Date(2026, time.October, 15, 12, 0, 0, 0, newYork), time.Date(2026, time.November, 1, 23, 0, 0, 0, newYork), 10000, 16000,
			14, 31, 4516, 7226, 2710, time.Date(2026, time.November, 1, 0, 0, 0, 0, newYork)},
		{"período atravessando o início do horário de verão de 2018 em São Paulo", calendar.Monthly,
			saoPaulo2018(time.October, 20, 12, 0), saoPaulo2018(time.November, 10, 8, 0), 10000, 16000,
			10, 31, 3226, 5161, 1935, saoPaulo2018(time.November, 10, 0, 0)},
		{"dia sem meia-noite começa na transição do horário de verão", calendar.Monthly,
			saoPaulo2018(time.November, 4, 12, 0), saoPaulo2018(time.November, 4, 1, 30), 10000, 16000,
			30, 30, 10000, 16000, 6000, time.Date(2018, time.November, 4, 3, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			planA, planB := fixturePlan(c.interval), fixturePlan(c.interval)
			oldPrice, newPrice := priceFlat(planA, c.oldCents), priceFlat(planB, c.newCents)
			s := billedSubscription(t, planA, oldPrice, 1, c.start)

			p, ok, err := ProrateChange(s, oldPrice, newPrice, 1, 1, c.at, ProrationBasis{Billed: big.NewRat(c.oldCents, 1)})
			if err != nil || !ok {
				t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
			}
			if want := big.NewRat(int64(c.remaining), int64(c.days)); p.Fraction.Cmp(want) != 0 {
				t.Fatalf("fração: obtive %v, esperava %v", p.Fraction, want)
			}
			if p.RemainingDays != c.remaining || p.PeriodDays != c.days {
				t.Fatalf("dias: obtive %d/%d, esperava %d/%d", p.RemainingDays, p.PeriodDays, c.remaining, c.days)
			}
			got := []int64{p.OldAmount.Amount(), p.NewAmount.Amount(), p.Credit.Amount(), p.Charge.Amount(), p.Net.Amount()}
			want := []int64{c.oldCents, c.newCents, c.credit, c.charge, c.net}
			if !slices.Equal(got, want) {
				t.Fatalf("valores (anterior, novo, crédito, cobrança, diferença): obtive %v, esperava %v", got, want)
			}
			for _, m := range []money.Money{p.Credit, p.Charge, p.Net} {
				if m.Currency() != money.BRL {
					t.Fatalf("moeda inesperada: %v", m)
				}
			}
			if !p.Period.Start.Equal(c.wantStart) || !p.Period.End.Equal(s.CurrentPeriod.End) {
				t.Fatalf("período prorrateado: obtive [%v, %v), esperava [%v, %v)", p.Period.Start, p.Period.End, c.wantStart, s.CurrentPeriod.End)
			}
			if p.Period.Start.Location() != time.UTC || p.Period.Start.Before(s.CurrentPeriod.Start) {
				t.Fatalf("início do período prorrateado deve ser UTC e dentro do período: %v", p.Period.Start)
			}
		})
	}
}

func TestProrateChangeQuantityAndAdjustedPrice(t *testing.T) {
	start := local(2026, time.April, 1, 12, 0)
	at := local(2026, time.April, 21, 10, 0)

	t.Run("aumento de quantidade por unidade", func(t *testing.T) {
		plan := fixturePlan(calendar.Monthly)
		price := pricePerUnit(plan, big.NewRat(2500, 1))
		s := billedSubscription(t, plan, price, 3, start)
		p, ok, err := ProrateChange(s, price, price, 3, 5, at, ProrationBasis{Billed: big.NewRat(7500, 1)})
		if err != nil || !ok {
			t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
		}
		// 3 × R$ 25,00 = R$ 75,00 e 5 × R$ 25,00 = R$ 125,00, ambos × 10/30.
		if p.OldAmount.Amount() != 7500 || p.NewAmount.Amount() != 12500 || p.Credit.Amount() != 2500 || p.Charge.Amount() != 4167 || p.Net.Amount() != 1667 {
			t.Fatalf("prorrata de quantidade: %+v", p)
		}
	})

	t.Run("redução de quantidade gera crédito líquido", func(t *testing.T) {
		plan := fixturePlan(calendar.Monthly)
		price := pricePerUnit(plan, big.NewRat(2500, 1))
		s := billedSubscription(t, plan, price, 5, start)
		p, ok, err := ProrateChange(s, price, price, 5, 2, at, ProrationBasis{Billed: big.NewRat(12500, 1)})
		if err != nil || !ok {
			t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
		}
		if p.Credit.Amount() != 4167 || p.Charge.Amount() != 1667 || p.Net.Amount() != -2500 {
			t.Fatalf("redução: %+v", p)
		}
	})

	t.Run("preço reajustado entra pela versão efetiva", func(t *testing.T) {
		planA, planB := fixturePlan(calendar.Monthly), fixturePlan(calendar.Monthly)
		priceA, priceB := pricePerUnit(planA, big.NewRat(2500, 1)), pricePerUnit(planB, big.NewRat(3000, 1))
		s := billedSubscription(t, planA, priceA, 3, start)
		s.PriceOverride = big.NewRat(2750, 1)
		p, ok, err := ProrateChange(s, s.EffectivePrice(priceA), priceB, 3, 3, at, ProrationBasis{Billed: big.NewRat(8250, 1)})
		if err != nil || !ok {
			t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
		}
		if p.OldAmount.Amount() != 8250 || p.Credit.Amount() != 2750 || p.Charge.Amount() != 3000 || p.Net.Amount() != 250 {
			t.Fatalf("o crédito usa o valor faturado com o preço reajustado: %+v", p)
		}
	})
}

// O crédito incide sobre o valor efetivamente faturado pelo componente
// anterior no período, e não sobre o preço vigente com o cupom ativo no
// momento da mudança; a cobrança usa o percentual que vale para o novo
// componente.
func TestProrateChangeCreditUsesBilledAmount(t *testing.T) {
	start := local(2026, time.April, 1, 12, 0)
	at := local(2026, time.April, 21, 10, 0) // 10 de 30 dias
	cases := []struct {
		name                string
		oldCents, newCents  int64
		basis               ProrationBasis
		credit, charge, net int64
		creditUnit          *big.Rat
		chargeUnit          *big.Rat
		wantBP              int64
	}{
		{"período faturado com 10% e cupom de 10% no novo plano", 10000, 16000, ProrationBasis{Billed: big.NewRat(9000, 1), NewBP: 1000},
			3000, 4800, 1800, big.NewRat(-9000, 1), big.NewRat(14400, 1), 1000},
		// O cliente pagou R$ 50,00 pelo período: a parte não usufruída vale R$ 16,67.
		{"período faturado com 50% e sem desconto no novo plano", 10000, 16000, ProrationBasis{Billed: big.NewRat(5000, 1)},
			1667, 5333, 3666, big.NewRat(-5000, 1), big.NewRat(16000, 1), 0},
		// Desconto de valor fixo distribuído no item: R$ 160,00 − R$ 30,00.
		{"período faturado com desconto de valor fixo", 16000, 10000, ProrationBasis{Billed: big.NewRat(13000, 1)},
			4333, 3333, -1000, big.NewRat(-13000, 1), big.NewRat(10000, 1), 0},
		// Cupom aplicado depois da fatura do período: o crédito é o do valor cheio.
		{"cupom aplicado no meio do período vale só para a cobrança", 10000, 16000, ProrationBasis{Billed: big.NewRat(10000, 1), NewBP: 1000},
			3333, 4800, 1467, big.NewRat(-10000, 1), big.NewRat(14400, 1), 1000},
		{"base com fração de centavo da prorrata anterior", 16000, 10000, ProrationBasis{Billed: big.NewRat(288015, 20)},
			4800, 3333, -1467, big.NewRat(-288015, 20), big.NewRat(10000, 1), 0},
		{"período faturado com 100% não gera crédito", 16000, 10000, ProrationBasis{Billed: new(big.Rat)},
			0, 3333, 3333, new(big.Rat), big.NewRat(10000, 1), 0},
		{"sem faturamento válido do período não há crédito", 16000, 10000, ProrationBasis{},
			0, 3333, 3333, new(big.Rat), big.NewRat(10000, 1), 0},
		{"base negativa é tratada como zero", 16000, 10000, ProrationBasis{Billed: big.NewRat(-100, 1)},
			0, 3333, 3333, new(big.Rat), big.NewRat(10000, 1), 0},
		{"cem por cento nos dois lados zera a diferença", 16000, 10000, ProrationBasis{Billed: new(big.Rat), NewBP: 10000},
			0, 0, 0, new(big.Rat), new(big.Rat), 10000},
		{"percentual fora do intervalo é limitado", 10000, 16000, ProrationBasis{Billed: big.NewRat(10000, 1), NewBP: 12000},
			3333, 0, -3333, big.NewRat(-10000, 1), new(big.Rat), 10000},
		{"percentual negativo é ignorado", 10000, 16000, ProrationBasis{Billed: big.NewRat(10000, 1), NewBP: -500},
			3333, 5333, 2000, big.NewRat(-10000, 1), big.NewRat(16000, 1), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			planA, planB := fixturePlan(calendar.Monthly), fixturePlan(calendar.Monthly)
			oldPrice, newPrice := priceFlat(planA, c.oldCents), priceFlat(planB, c.newCents)
			s := billedSubscription(t, planA, oldPrice, 1, start)
			p, ok, err := ProrateChange(s, oldPrice, newPrice, 1, 1, at, c.basis)
			if err != nil || !ok {
				t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
			}
			if p.Credit.Amount() != c.credit || p.Charge.Amount() != c.charge || p.Net.Amount() != c.net {
				t.Fatalf("crédito %d, cobrança %d e diferença %d esperados: %v %v %v", c.credit, c.charge, c.net, p.Credit, p.Charge, p.Net)
			}
			if p.OldAmount.Amount() != c.oldCents || p.NewAmount.Amount() != c.newCents || p.NewDiscountBP != c.wantBP {
				t.Fatalf("componentes sem desconto e percentual da cobrança: %+v", p)
			}
			if p.CreditUnitAmount().Cmp(c.creditUnit) != 0 || p.ChargeUnitAmount().Cmp(c.chargeUnit) != 0 {
				t.Fatalf("valores unitários: crédito %s cobrança %s", p.CreditUnitAmount().RatString(), p.ChargeUnitAmount().RatString())
			}
			credit, err := money.FromRat(new(big.Rat).Mul(p.CreditUnitAmount(), p.Fraction), money.BRL, money.DefaultRounding)
			if err != nil || credit.Amount() != -c.credit {
				t.Fatalf("fração × valor unitário do crédito arredonda para o crédito: %v %v", credit, err)
			}
			charge, err := money.FromRat(new(big.Rat).Mul(p.ChargeUnitAmount(), p.Fraction), money.BRL, money.DefaultRounding)
			if err != nil || charge.Amount() != c.charge {
				t.Fatalf("fração × valor unitário da cobrança arredonda para a cobrança: %v %v", charge, err)
			}
		})
	}
	t.Run("a base informada não é alterada pelo cálculo", func(t *testing.T) {
		plan := fixturePlan(calendar.Monthly)
		price := priceFlat(plan, 10000)
		billed := big.NewRat(9000, 1)
		p, ok, err := ProrateChange(billedSubscription(t, plan, price, 1, start), price, priceFlat(fixturePlan(calendar.Monthly), 16000), 1, 1, at,
			ProrationBasis{Billed: billed})
		if err != nil || !ok {
			t.Fatalf("esperava prorrata: ok=%v err=%v", ok, err)
		}
		p.CreditBase.SetInt64(1)
		if billed.Cmp(big.NewRat(9000, 1)) != 0 {
			t.Fatalf("a base do chamador foi alterada: %s", billed.RatString())
		}
		if (Proration{}).CreditUnitAmount().Sign() != 0 {
			t.Fatal("prorrata vazia tem crédito unitário zero")
		}
	})
}

func TestProrateChangeWithoutProration(t *testing.T) {
	plan := fixturePlan(calendar.Monthly)
	price := priceFlat(plan, 10000)
	newPrice := priceFlat(fixturePlan(calendar.Monthly), 16000)
	start := local(2026, time.April, 1, 12, 0)
	cases := []struct {
		name string
		make func(t *testing.T) Subscription
		at   time.Time
	}{
		{"período corrente ainda não faturado", func(t *testing.T) Subscription {
			return mustNew(t, NewInput{ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), Plan: plan, Price: price,
				Quantity: 1, StartAt: start, Location: saoPaulo, Now: start})
		}, local(2026, time.April, 21, 10, 0)},
		{"assinatura em avaliação", func(t *testing.T) Subscription {
			return mustNew(t, NewInput{ID: uuid.New(), TenantID: uuid.New(), CustomerID: uuid.New(), Plan: plan, Price: price,
				Quantity: 1, StartAt: start, TrialDays: 14, Location: saoPaulo, Now: start})
		}, local(2026, time.April, 5, 10, 0)},
		{"instante anterior ao período corrente", func(t *testing.T) Subscription {
			return billedSubscription(t, plan, price, 1, start)
		}, local(2026, time.March, 31, 23, 59)},
		{"instante no fim exclusivo do período", func(t *testing.T) Subscription {
			return billedSubscription(t, plan, price, 1, start)
		}, local(2026, time.May, 1, 0, 0)},
		{"reativada em período ainda não faturado", func(t *testing.T) Subscription {
			s := billedSubscription(t, plan, price, 1, start)
			if err := s.Suspend("", local(2026, time.April, 10, 9, 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.Reactivate(local(2026, time.June, 3, 9, 0)); err != nil {
				t.Fatal(err)
			}
			return s
		}, local(2026, time.June, 10, 9, 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok, err := ProrateChange(c.make(t), price, newPrice, 1, 1, c.at, ProrationBasis{Billed: big.NewRat(10000, 1)})
			if err != nil || ok {
				t.Fatalf("não deveria haver prorrata: ok=%v err=%v", ok, err)
			}
			if p.Fraction != nil || !p.Credit.IsZero() || !p.Charge.IsZero() {
				t.Fatalf("prorrata vazia esperada: %+v", p)
			}
		})
	}
}

func TestProrateChangeErrors(t *testing.T) {
	start := local(2026, time.April, 1, 12, 0)
	at := local(2026, time.April, 21, 10, 0)
	plan := fixturePlan(calendar.Monthly)
	flat := priceFlat(plan, 10000)
	huge := pricePerUnit(plan, big.NewRat(money.MaxAmount, 1))
	foreign := flat
	foreign.FlatAmount = &money.Money{}

	cases := []struct {
		name     string
		timezone string
		oldPrice catalog.PriceVersion
		newPrice catalog.PriceVersion
		qty      int
		target   error
	}{
		{"fuso inválido", "America/Inexistente", flat, flat, 1, nil},
		{"estouro no componente anterior", "", huge, flat, MaxQuantity, money.ErrOverflow},
		{"estouro no novo componente", "", flat, huge, MaxQuantity, money.ErrOverflow},
		{"moedas diferentes entre os componentes", "", flat, foreign, 1, money.ErrCurrencyMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := billedSubscription(t, plan, flat, 1, start)
			if c.timezone != "" {
				s.Timezone = c.timezone
			}
			p, ok, err := ProrateChange(s, c.oldPrice, c.newPrice, c.qty, c.qty, at, ProrationBasis{Billed: big.NewRat(10000, 1)})
			if err == nil || ok || p.Fraction != nil {
				t.Fatalf("esperava erro sem prorrata: ok=%v err=%v", ok, err)
			}
			if c.target != nil && !errors.Is(err, c.target) {
				t.Fatalf("esperava %v, obtive %v", c.target, err)
			}
		})
	}
}
