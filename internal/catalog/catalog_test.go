package catalog

import (
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var testNow = time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("racional inválido: " + s)
	}
	return r
}

// assertAppErr verifica categoria e código do erro e devolve os campos
// inválidos informados.
func assertAppErr(t testing.TB, err error, kind apperr.Kind, code string) []string {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("esperado *apperr.Error %s/%s, obtido %v", kind, code, err)
	}
	if e.Kind != kind || e.Code != code {
		t.Fatalf("erro = %s/%s, esperado %s/%s", e.Kind, e.Code, kind, code)
	}
	return fieldNames(e)
}

func fieldNames(e *apperr.Error) []string {
	out := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		out = append(out, f.Field)
	}
	return out
}

func activeMetric() *Metric {
	return &Metric{ID: uuid.New(), Code: "api.requests", Aggregation: AggSum, Status: Active}
}

// newPrice cria uma versão de preço válida ou falha o teste.
func newPrice(t testing.TB, in PriceInput, metric *Metric) PriceVersion {
	t.Helper()
	pv, err := NewPriceVersion(uuid.New(), uuid.New(), uuid.New(), 1, money.BRL, in, metric, nil, testNow)
	if err != nil {
		t.Fatalf("NewPriceVersion: %v", err)
	}
	return pv
}

func TestNewPriceVersionValid(t *testing.T) {
	m := activeMetric()
	cases := []struct {
		name   string
		in     PriceInput
		metric *Metric
		check  func(*testing.T, PriceVersion)
	}{
		{"valor fixo", PriceInput{Model: Flat, FlatAmount: ptr(int64(9990))}, nil, func(t *testing.T, pv PriceVersion) {
			if pv.FlatAmount == nil || pv.FlatAmount.Amount() != 9990 || pv.UnitAmount != nil || pv.MetricID != nil || pv.IncludedQuantity != nil {
				t.Fatalf("versão inesperada: %+v", pv)
			}
		}},
		{"valor fixo zero", PriceInput{Model: Flat, FlatAmount: ptr(int64(0))}, nil, func(t *testing.T, pv PriceVersion) {
			if !pv.FlatAmount.IsZero() {
				t.Fatalf("valor = %v", pv.FlatAmount)
			}
		}},
		{"valor fixo no limite", PriceInput{Model: Flat, FlatAmount: ptr(money.MaxAmount)}, nil, nil},
		{"por unidade com fração de centavo", PriceInput{Model: PerUnit, UnitAmount: "4990.5"}, nil, func(t *testing.T, pv PriceVersion) {
			if pv.UnitAmount.Cmp(rat("4990.5")) != 0 || pv.FlatAmount != nil || pv.MetricID != nil {
				t.Fatalf("versão inesperada: %+v", pv)
			}
		}},
		{"por unidade com 10 casas", PriceInput{Model: PerUnit, UnitAmount: "0.0035000001"}, nil, nil},
		{"consumo", PriceInput{Model: Usage, UnitAmount: "0.35", MetricID: &m.ID}, m, func(t *testing.T, pv PriceVersion) {
			if pv.MetricID == nil || *pv.MetricID != m.ID || pv.UnitAmount.Cmp(rat("0.35")) != 0 || pv.FlatAmount != nil {
				t.Fatalf("versão inesperada: %+v", pv)
			}
		}},
		{"fixo com excedente", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(5000)), UnitAmount: "2", MetricID: &m.ID,
			IncludedQuantity: "1000.5"}, m, func(t *testing.T, pv PriceVersion) {
			if pv.FlatAmount.Amount() != 5000 || pv.UnitAmount.Cmp(rat("2")) != 0 || *pv.MetricID != m.ID ||
				pv.IncludedQuantity.Cmp(rat("1000.5")) != 0 {
				t.Fatalf("versão inesperada: %+v", pv)
			}
		}},
		{"fixo com excedente e franquia zero", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(0)), UnitAmount: "0",
			MetricID: &m.ID, IncludedQuantity: "0"}, m, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, tenantID, planID, by := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			pv, err := NewPriceVersion(id, tenantID, planID, 3, money.BRL, tc.in, tc.metric, &by, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if pv.ID != id || pv.TenantID != tenantID || pv.PlanID != planID || pv.Version != 3 || pv.Model != tc.in.Model ||
				pv.Currency != money.BRL || pv.CreatedBy == nil || *pv.CreatedBy != by || !pv.CreatedAt.Equal(testNow) {
				t.Fatalf("metadados inesperados: %+v", pv)
			}
			if tc.check != nil {
				tc.check(t, pv)
			}
		})
	}
}

func TestNewPriceVersionInvalid(t *testing.T) {
	m := activeMetric()
	archived := activeMetric()
	archived.Status = Archived
	cases := []struct {
		name   string
		in     PriceInput
		metric *Metric
		fields []string
	}{
		{"modelo desconhecido", PriceInput{Model: "tiered"}, nil, []string{"pricing_model"}},
		{"fixo sem valor", PriceInput{Model: Flat}, nil, []string{"flat_amount"}},
		{"fixo negativo", PriceInput{Model: Flat, FlatAmount: ptr(int64(-1))}, nil, []string{"flat_amount"}},
		{"fixo acima do limite", PriceInput{Model: Flat, FlatAmount: ptr(money.MaxAmount + 1)}, nil, []string{"flat_amount"}},
		{"fixo com preço unitário", PriceInput{Model: Flat, FlatAmount: ptr(int64(100)), UnitAmount: "1"}, nil, []string{"unit_amount_decimal"}},
		{"fixo com métrica", PriceInput{Model: Flat, FlatAmount: ptr(int64(100)), MetricID: &m.ID}, m, []string{"metric_id"}},
		{"por unidade com valor fixo", PriceInput{Model: PerUnit, UnitAmount: "1", FlatAmount: ptr(int64(1))}, nil, []string{"flat_amount"}},
		{"por unidade sem preço", PriceInput{Model: PerUnit}, nil, []string{"unit_amount_decimal"}},
		{"por unidade negativo", PriceInput{Model: PerUnit, UnitAmount: "-1"}, nil, []string{"unit_amount_decimal"}},
		{"por unidade em notação científica", PriceInput{Model: PerUnit, UnitAmount: "1e3"}, nil, []string{"unit_amount_decimal"}},
		{"por unidade com vírgula", PriceInput{Model: PerUnit, UnitAmount: "49,90"}, nil, []string{"unit_amount_decimal"}},
		{"por unidade com 11 casas", PriceInput{Model: PerUnit, UnitAmount: "0.00000000001"}, nil, []string{"unit_amount_decimal"}},
		{"por unidade acima do limite", PriceInput{Model: PerUnit, UnitAmount: "100000000000000.1"}, nil, []string{"unit_amount_decimal"}},
		{"por unidade com métrica", PriceInput{Model: PerUnit, UnitAmount: "1", MetricID: &m.ID}, m, []string{"metric_id"}},
		{"consumo sem métrica", PriceInput{Model: Usage, UnitAmount: "1"}, nil, []string{"metric_id"}},
		{"consumo com métrica inexistente", PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID}, nil, []string{"metric_id"}},
		{"consumo com métrica arquivada", PriceInput{Model: Usage, UnitAmount: "1", MetricID: &archived.ID}, archived, []string{"metric_id"}},
		{"consumo com valor fixo", PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID, FlatAmount: ptr(int64(1))}, m, []string{"flat_amount"}},
		{"excedente sem franquia", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(1)), UnitAmount: "1", MetricID: &m.ID}, m,
			[]string{"included_quantity"}},
		{"excedente com franquia negativa", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(1)), UnitAmount: "1", MetricID: &m.ID,
			IncludedQuantity: "-5"}, m, []string{"included_quantity"}},
		{"excedente sem nenhum campo", PriceInput{Model: FlatPlusOverage}, nil,
			[]string{"flat_amount", "unit_amount_decimal", "metric_id", "included_quantity"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv, err := NewPriceVersion(uuid.New(), uuid.New(), uuid.New(), 1, money.BRL, tc.in, tc.metric, nil, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_price")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
			if pv.ID != uuid.Nil {
				t.Fatal("versão inválida não deve ser devolvida")
			}
		})
	}
}

func TestNewPriceVersionRejectsIncludedQuantityOutsideOverage(t *testing.T) {
	// A franquia só existe no modelo com excedente; nos demais é rejeitada
	// como os outros campos que o modelo não usa.
	_, err := NewPriceVersion(uuid.New(), uuid.New(), uuid.New(), 1, money.BRL,
		PriceInput{Model: PerUnit, UnitAmount: "10", IncludedQuantity: "500"}, nil, nil, testNow)
	fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_price")
	if !slices.Equal(fields, []string{"included_quantity"}) {
		t.Fatalf("campos = %v", fields)
	}
}

func TestModelPredicates(t *testing.T) {
	cases := []struct {
		model             PricingModel
		hasUsage, hasFixd bool
	}{
		{Flat, false, true},
		{PerUnit, false, true},
		{Usage, true, false},
		{FlatPlusOverage, true, true},
	}
	for _, tc := range cases {
		pv := PriceVersion{Model: tc.model}
		if pv.HasUsage() != tc.hasUsage || pv.HasFixed() != tc.hasFixd {
			t.Errorf("%s: HasUsage=%v HasFixed=%v", tc.model, pv.HasUsage(), pv.HasFixed())
		}
	}
}

func TestFixedCharge(t *testing.T) {
	m := activeMetric()
	cases := []struct {
		name     string
		in       PriceInput
		metric   *Metric
		quantity int
		amount   int64
		qty      string
		unit     string
	}{
		{"fixo ignora a quantidade", PriceInput{Model: Flat, FlatAmount: ptr(int64(9990))}, nil, 7, 9990, "1", "9990"},
		{"fixo com excedente cobra só o fixo", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(5000)), UnitAmount: "2",
			MetricID: &m.ID, IncludedQuantity: "10"}, m, 3, 5000, "1", "5000"},
		{"por unidade arredonda o total uma vez", PriceInput{Model: PerUnit, UnitAmount: "4990.5"}, nil, 3, 14972, "3", "4990.5"},
		{"empate vai para o par (abaixo)", PriceInput{Model: PerUnit, UnitAmount: "4990.5"}, nil, 1, 4990, "1", "4990.5"},
		{"empate vai para o par (acima)", PriceInput{Model: PerUnit, UnitAmount: "4991.5"}, nil, 1, 4992, "1", "4991.5"},
		{"empate com quantidade par", PriceInput{Model: PerUnit, UnitAmount: "0.5"}, nil, 5, 2, "5", "0.5"},
		// Arredondar o unitário antes (0) daria zero; o total exato 0,9999999999 vira 1.
		{"sem arredondamento intermediário", PriceInput{Model: PerUnit, UnitAmount: "0.3333333333"}, nil, 3, 1, "3", "0.3333333333"},
		{"quantidade zero", PriceInput{Model: PerUnit, UnitAmount: "4990"}, nil, 0, 0, "0", "4990"},
		{"consumo não tem componente fixo", PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID}, m, 5, 0, "0", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv := newPrice(t, tc.in, tc.metric)
			amount, qty, unit, err := pv.FixedCharge(tc.quantity)
			if err != nil {
				t.Fatal(err)
			}
			if amount.Amount() != tc.amount || amount.Currency() != money.BRL {
				t.Fatalf("valor = %v, esperado %d", amount, tc.amount)
			}
			if qty.Cmp(rat(tc.qty)) != 0 || unit.Cmp(rat(tc.unit)) != 0 {
				t.Fatalf("quantidade=%s unitário=%s, esperado %s e %s", qty.RatString(), unit.RatString(), tc.qty, tc.unit)
			}
		})
	}
}

func TestFixedChargeOverflow(t *testing.T) {
	// Preço unitário no limite (MaxAmount) vezes 100.000 unidades excede int64.
	pv := newPrice(t, PriceInput{Model: PerUnit, UnitAmount: "100000000000000"}, nil)
	if _, _, _, err := pv.FixedCharge(100_000); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("esperado ErrOverflow, obtido %v", err)
	}
}

func FuzzPriceFixedCharge(f *testing.F) {
	f.Add(int64(49905), uint8(1), uint16(3))
	f.Add(int64(5), uint8(1), uint16(1))
	f.Add(int64(3333333333), uint8(10), uint16(3))
	f.Add(int64(0), uint8(0), uint16(0))
	f.Fuzz(func(t *testing.T, minor int64, scale uint8, quantity uint16) {
		if minor < 0 {
			minor = -(minor + 1)
		}
		minor %= 1_000_000_000_000
		sc := int(scale % (money.MaxDecimalScale + 1))
		unitText := money.FormatMinor(minor, sc)
		pv, err := NewPriceVersion(uuid.New(), uuid.New(), uuid.New(), 1, money.BRL, PriceInput{Model: PerUnit, UnitAmount: unitText}, nil, nil, testNow)
		if err != nil {
			t.Fatalf("preço %q deveria ser aceito: %v", unitText, err)
		}
		amount, _, _, err := pv.FixedCharge(int(quantity))
		if err != nil {
			t.Fatalf("FixedCharge(%d) com %q: %v", quantity, unitText, err)
		}
		exact := new(big.Rat).Mul(rat(unitText), big.NewRat(int64(quantity), 1))
		diff := new(big.Rat).Sub(new(big.Rat).SetInt64(amount.Amount()), exact)
		half := big.NewRat(1, 2)
		switch diff.Abs(diff).Cmp(half) {
		case 1:
			t.Fatalf("%s × %d = %d diverge mais de meia unidade de %s", unitText, quantity, amount.Amount(), exact.FloatString(10))
		case 0:
			if amount.Amount()%2 != 0 {
				t.Fatalf("empate deve ir para o par: %s × %d = %d", unitText, quantity, amount.Amount())
			}
		}
	})
}

func TestUsageCharge(t *testing.T) {
	m := activeMetric()
	usage := PriceInput{Model: Usage, UnitAmount: "0.35", MetricID: &m.ID}
	overage := PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(5000)), UnitAmount: "2", MetricID: &m.ID, IncludedQuantity: "1000"}
	cases := []struct {
		name       string
		in         PriceInput
		aggregated string
		amount     int64
		billable   string
	}{
		{"consumo simples", usage, "1500", 525, "1500"},
		{"consumo zero", usage, "0", 0, "0"},
		{"consumo negativo é tratado como zero", usage, "-10", 0, "0"},
		{"consumo fracionário arredondado uma vez", PriceInput{Model: Usage, UnitAmount: "0.0035", MetricID: &m.ID}, "1000001", 3500, "1000001"},
		{"empate vai para o par (abaixo)", PriceInput{Model: Usage, UnitAmount: "0.5", MetricID: &m.ID}, "5", 2, "5"},
		{"empate vai para o par (acima)", PriceInput{Model: Usage, UnitAmount: "0.5", MetricID: &m.ID}, "7", 4, "7"},
		{"franquia cobra só o excedente", overage, "1500", 1000, "500"},
		{"excedente fracionário com empate para o par (zero)", overage, "1000.25", 0, "0.25"}, // 2 × 0,25 = 0,5 → 0
		{"excedente fracionário com empate para o par (dois)", overage, "1000.75", 2, "0.75"}, // 2 × 0,75 = 1,5 → 2
		{"consumo igual à franquia", overage, "1000", 0, "0"},
		{"consumo abaixo da franquia", overage, "800", 0, "0"},
		{"modelo fixo não cobra consumo", PriceInput{Model: Flat, FlatAmount: ptr(int64(100))}, "1000", 0, "0"},
		{"modelo por unidade não cobra consumo", PriceInput{Model: PerUnit, UnitAmount: "10"}, "1000", 0, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv := newPrice(t, tc.in, m)
			agg := rat(tc.aggregated)
			original := new(big.Rat).Set(agg)
			amount, billable, err := pv.UsageCharge(agg)
			if err != nil {
				t.Fatal(err)
			}
			if amount.Amount() != tc.amount || billable.Cmp(rat(tc.billable)) != 0 {
				t.Fatalf("valor=%d faturável=%s, esperado %d e %s", amount.Amount(), billable.RatString(), tc.amount, tc.billable)
			}
			if agg.Cmp(original) != 0 {
				t.Fatal("a quantidade agregada recebida não pode ser alterada")
			}
		})
	}
}

func TestUsageChargeDoesNotMutateIncludedQuantity(t *testing.T) {
	m := activeMetric()
	pv := newPrice(t, PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(1)), UnitAmount: "1", MetricID: &m.ID, IncludedQuantity: "100"}, m)
	for range 3 {
		if _, _, err := pv.UsageCharge(rat("150")); err != nil {
			t.Fatal(err)
		}
	}
	if pv.IncludedQuantity.Cmp(rat("100")) != 0 {
		t.Fatalf("franquia alterada: %s", pv.IncludedQuantity.RatString())
	}
}

func TestMonthlyRecurring(t *testing.T) {
	m := activeMetric()
	cases := []struct {
		name     string
		in       PriceInput
		interval calendar.Interval
		quantity int
		want     string
	}{
		{"fixo mensal", PriceInput{Model: Flat, FlatAmount: ptr(int64(9990))}, calendar.Monthly, 1, "9990"},
		{"fixo anual dividido por 12", PriceInput{Model: Flat, FlatAmount: ptr(int64(120000))}, calendar.Yearly, 1, "10000"},
		{"anual mantém a fração exata", PriceInput{Model: Flat, FlatAmount: ptr(int64(1000))}, calendar.Yearly, 1, "250/3"},
		{"por unidade mensal", PriceInput{Model: PerUnit, UnitAmount: "4990.5"}, calendar.Monthly, 3, "14972"},
		{"por unidade anual", PriceInput{Model: PerUnit, UnitAmount: "1000"}, calendar.Yearly, 6, "500"},
		{"fixo com excedente considera só o fixo", PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(5000)), UnitAmount: "2",
			MetricID: &m.ID, IncludedQuantity: "10"}, calendar.Monthly, 1, "5000"},
		{"consumo não entra na recorrência", PriceInput{Model: Usage, UnitAmount: "100", MetricID: &m.ID}, calendar.Monthly, 10, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv := newPrice(t, tc.in, m)
			got, err := pv.MonthlyRecurring(tc.interval, tc.quantity)
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(rat(tc.want)) != 0 {
				t.Fatalf("MRR = %s, esperado %s", got.RatString(), tc.want)
			}
		})
	}
}

func TestMonthlyRecurringDoesNotMutatePrice(t *testing.T) {
	pv := newPrice(t, PriceInput{Model: Flat, FlatAmount: ptr(int64(1200))}, nil)
	for range 2 {
		got, err := pv.MonthlyRecurring(calendar.Yearly, 1)
		if err != nil || got.Cmp(rat("100")) != 0 {
			t.Fatalf("MRR = %v, %v", got, err)
		}
	}
	if pv.FlatAmount.Amount() != 1200 {
		t.Fatalf("valor fixo alterado: %v", pv.FlatAmount)
	}
}

func TestMonthlyRecurringOverflow(t *testing.T) {
	pv := newPrice(t, PriceInput{Model: PerUnit, UnitAmount: "100000000000000"}, nil)
	if _, err := pv.MonthlyRecurring(calendar.Monthly, 100_000); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("esperado ErrOverflow, obtido %v", err)
	}
}

func TestNewProduct(t *testing.T) {
	id, tenantID := uuid.New(), uuid.New()
	p, err := NewProduct(id, tenantID, ProductInput{Name: "  Plataforma  ", Description: " Software de cobrança "}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := Product{ID: id, TenantID: tenantID, Name: "Plataforma", Description: "Software de cobrança", Status: Active, CreatedAt: testNow, Version: 1}
	if p != want {
		t.Fatalf("produto = %+v\nesperado %+v", p, want)
	}

	cases := []struct {
		name   string
		in     ProductInput
		fields []string
	}{
		{"nome curto", ProductInput{Name: "P"}, []string{"name"}},
		{"nome longo", ProductInput{Name: strings.Repeat("p", 201)}, []string{"name"}},
		{"descrição longa", ProductInput{Name: "Produto", Description: strings.Repeat("d", 2001)}, []string{"description"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProduct(uuid.New(), uuid.New(), tc.in, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_product")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
		})
	}
	if _, err := NewProduct(uuid.New(), uuid.New(), ProductInput{Name: "Pr", Description: strings.Repeat("ã", 2000)}, testNow); err != nil {
		t.Fatalf("limites deveriam ser aceitos: %v", err)
	}
}

func validMetricInput() MetricInput {
	return MetricInput{Code: "api.requests", Name: "Requisições", Unit: "request", Aggregation: AggSum, MaxQuantityPerEvent: "1000"}
}

func TestNewMetric(t *testing.T) {
	m, err := NewMetric(uuid.New(), uuid.New(), validMetricInput(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Code != "api.requests" || m.Unit != "request" || m.Aggregation != AggSum || m.MaxQuantityPerEvent.Cmp(rat("1000")) != 0 ||
		m.LateGraceHours != 72 || m.Status != Active || !m.CreatedAt.Equal(testNow) {
		t.Fatalf("métrica inesperada: %+v", m)
	}
	for _, agg := range []Aggregation{AggSum, AggMax, AggUniqueCount, AggLast} {
		in := validMetricInput()
		in.Aggregation = agg
		if _, err := NewMetric(uuid.New(), uuid.New(), in, testNow); err != nil {
			t.Errorf("agregação %s deveria ser aceita: %v", agg, err)
		}
	}
	for _, grace := range []int{0, 720} {
		in := validMetricInput()
		in.LateGraceHours = ptr(grace)
		got, err := NewMetric(uuid.New(), uuid.New(), in, testNow)
		if err != nil || got.LateGraceHours != grace {
			t.Errorf("tolerância %d deveria ser aceita: %+v, %v", grace, got, err)
		}
	}
}

func TestNewMetricValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*MetricInput)
		fields []string
	}{
		{"código curto", func(in *MetricInput) { in.Code = "a" }, []string{"code"}},
		{"código com maiúsculas", func(in *MetricInput) { in.Code = "API.requests" }, []string{"code"}},
		{"código iniciando com ponto", func(in *MetricInput) { in.Code = ".api" }, []string{"code"}},
		{"nome curto", func(in *MetricInput) { in.Name = "R" }, []string{"name"}},
		{"unidade vazia", func(in *MetricInput) { in.Unit = "" }, []string{"unit"}},
		{"unidade com espaço", func(in *MetricInput) { in.Unit = "giga byte" }, []string{"unit"}},
		{"agregação média", func(in *MetricInput) { in.Aggregation = "avg" }, []string{"aggregation"}},
		{"agregação vazia", func(in *MetricInput) { in.Aggregation = "" }, []string{"aggregation"}},
		{"limite por evento ausente", func(in *MetricInput) { in.MaxQuantityPerEvent = "" }, []string{"max_quantity_per_event"}},
		{"limite por evento zero", func(in *MetricInput) { in.MaxQuantityPerEvent = "0" }, []string{"max_quantity_per_event"}},
		{"limite por evento negativo", func(in *MetricInput) { in.MaxQuantityPerEvent = "-1" }, []string{"max_quantity_per_event"}},
		{"tolerância negativa", func(in *MetricInput) { in.LateGraceHours = ptr(-1) }, []string{"late_grace_hours"}},
		{"tolerância acima de 30 dias", func(in *MetricInput) { in.LateGraceHours = ptr(721) }, []string{"late_grace_hours"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validMetricInput()
			tc.mutate(&in)
			_, err := NewMetric(uuid.New(), uuid.New(), in, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_metric")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
		})
	}
}

func validPlanInput() PlanInput {
	return PlanInput{ProductID: uuid.New(), Code: "pro-mensal", Name: "Pro mensal", Interval: calendar.Monthly, TrialDays: 7}
}

func TestNewPlan(t *testing.T) {
	in := validPlanInput()
	in.Code, in.Name, in.Description = "  pro-mensal ", " Pro mensal ", " Plano profissional "
	p, err := NewPlan(uuid.New(), uuid.New(), in, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != "pro-mensal" || p.Name != "Pro mensal" || p.Description != "Plano profissional" || p.Interval != calendar.Monthly ||
		p.Currency != money.BRL || p.TrialDays != 7 || p.Status != Active || p.Version != 1 || p.ProductID != in.ProductID ||
		p.CurrentPriceVersionID != nil {
		t.Fatalf("plano inesperado: %+v", p)
	}
	for _, trial := range []int{0, 365} {
		in := validPlanInput()
		in.TrialDays, in.Interval = trial, calendar.Yearly
		if _, err := NewPlan(uuid.New(), uuid.New(), in, testNow); err != nil {
			t.Errorf("trial de %d dias deveria ser aceito: %v", trial, err)
		}
	}
}

func TestNewPlanValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PlanInput)
		fields []string
	}{
		{"código inválido", func(in *PlanInput) { in.Code = "Pro Mensal" }, []string{"code"}},
		{"nome curto", func(in *PlanInput) { in.Name = "P" }, []string{"name"}},
		{"descrição longa", func(in *PlanInput) { in.Description = strings.Repeat("d", 2001) }, []string{"description"}},
		{"periodicidade semanal", func(in *PlanInput) { in.Interval = "week" }, []string{"billing_interval"}},
		{"trial negativo", func(in *PlanInput) { in.TrialDays = -1 }, []string{"trial_days"}},
		{"trial acima de um ano", func(in *PlanInput) { in.TrialDays = 366 }, []string{"trial_days"}},
		{"produto ausente", func(in *PlanInput) { in.ProductID = uuid.Nil }, []string{"product_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validPlanInput()
			tc.mutate(&in)
			_, err := NewPlan(uuid.New(), uuid.New(), in, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_plan")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
		})
	}
}
