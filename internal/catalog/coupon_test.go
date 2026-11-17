package catalog

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// planUUID cria identificadores determinísticos para verificar ordenação.
func planUUID(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

func validCouponInput() CouponInput {
	return CouponInput{Code: "PROMO10", Name: "Promoção de lançamento", Type: DiscountPercent,
		PercentOffBP: ptr(1000), Duration: DurationOnce}
}

// couponWith aplica mutate sobre a entrada válida.
func couponWith(mutate func(*CouponInput)) CouponInput {
	in := validCouponInput()
	if mutate != nil {
		mutate(&in)
	}
	return in
}

// newTestCoupon cria um cupom válido ou falha o teste.
func newTestCoupon(t testing.TB, mutate func(*CouponInput)) Coupon {
	t.Helper()
	c, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, couponWith(mutate), testNow)
	if err != nil {
		t.Fatalf("NewCoupon: %v", err)
	}
	return c
}

func uuidsOf(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.New()
	}
	return out
}

func TestNewCouponMetadata(t *testing.T) {
	id, tenantID := uuid.New(), uuid.New()
	c, err := NewCoupon(id, tenantID, money.BRL, validCouponInput(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != id || c.TenantID != tenantID || c.Currency != money.BRL || c.Status != Active || c.Version != 1 ||
		c.RedemptionsCount != 0 || !c.CreatedAt.Equal(testNow) {
		t.Fatalf("metadados inesperados: %+v", c)
	}
	if c.Code != "PROMO10" || c.Name != "Promoção de lançamento" || c.Type != DiscountPercent || c.Duration != DurationOnce {
		t.Fatalf("dados inesperados: %+v", c)
	}
	if c.PercentOffBP == nil || *c.PercentOffBP != 1000 || c.AmountOff != nil || c.DurationCycles != nil ||
		c.MaxRedemptions != nil || c.ValidFrom != nil || c.ValidUntil != nil {
		t.Fatalf("campos opcionais inesperados: %+v", c)
	}
	if c.AppliesToPlanIDs == nil || len(c.AppliesToPlanIDs) != 0 {
		t.Fatalf("sem planos, a lista deve ser vazia e não nula: %#v", c.AppliesToPlanIDs)
	}
}

func TestNewCouponValid(t *testing.T) {
	future := testNow.Add(30 * 24 * time.Hour)
	past := testNow.Add(-24 * time.Hour)
	cases := []struct {
		name   string
		mutate func(*CouponInput)
		check  func(*testing.T, Coupon)
	}{
		{"percentual mínimo", func(in *CouponInput) { in.PercentOffBP = ptr(1) }, func(t *testing.T, c Coupon) {
			if *c.PercentOffBP != 1 {
				t.Fatalf("percentual = %d", *c.PercentOffBP)
			}
		}},
		{"percentual integral", func(in *CouponInput) { in.PercentOffBP = ptr(10000) }, nil},
		{"valor fixo", func(in *CouponInput) {
			in.Type, in.PercentOffBP, in.AmountOff = DiscountAmount, nil, ptr(int64(2500))
		}, func(t *testing.T, c Coupon) {
			if c.AmountOff == nil || c.AmountOff.Amount() != 2500 || c.AmountOff.Currency() != money.BRL || c.PercentOffBP != nil {
				t.Fatalf("desconto inesperado: %+v", c)
			}
		}},
		{"valor fixo mínimo", func(in *CouponInput) { in.Type, in.PercentOffBP, in.AmountOff = DiscountAmount, nil, ptr(int64(1)) }, nil},
		{"valor fixo no limite", func(in *CouponInput) {
			in.Type, in.PercentOffBP, in.AmountOff = DiscountAmount, nil, ptr(money.MaxAmount)
		}, func(t *testing.T, c Coupon) {
			if c.AmountOff.Amount() != money.MaxAmount {
				t.Fatalf("valor = %v", c.AmountOff)
			}
		}},
		{"recorrente por um ciclo", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(1) }, func(t *testing.T, c Coupon) {
			if c.DurationCycles == nil || *c.DurationCycles != 1 {
				t.Fatalf("ciclos = %v", c.DurationCycles)
			}
		}},
		{"recorrente no limite de ciclos", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(120) }, nil},
		{"sem prazo", func(in *CouponInput) { in.Duration = DurationForever }, func(t *testing.T, c Coupon) {
			if c.Duration != DurationForever || c.DurationCycles != nil {
				t.Fatalf("duração inesperada: %+v", c)
			}
		}},
		{"limite de um uso", func(in *CouponInput) { in.MaxRedemptions = ptr(1) }, func(t *testing.T, c Coupon) {
			if c.MaxRedemptions == nil || *c.MaxRedemptions != 1 {
				t.Fatalf("limite = %v", c.MaxRedemptions)
			}
		}},
		{"limite de usos no máximo aceito", func(in *CouponInput) { in.MaxRedemptions = ptr(MaxCouponRedemptions) }, func(t *testing.T, c Coupon) {
			if c.MaxRedemptions == nil || *c.MaxRedemptions != 1_000_000_000 {
				t.Fatalf("limite = %v", c.MaxRedemptions)
			}
		}},
		{"validade com início no passado", func(in *CouponInput) { in.ValidFrom, in.ValidUntil = &past, &future }, func(t *testing.T, c Coupon) {
			if !c.ValidFrom.Equal(past) || !c.ValidUntil.Equal(future) {
				t.Fatalf("validade = %v a %v", c.ValidFrom, c.ValidUntil)
			}
		}},
		{"início no futuro sem fim", func(in *CouponInput) { in.ValidFrom = &future }, nil},
		{"fim logo após o instante atual", func(in *CouponInput) { in.ValidUntil = ptr(testNow.Add(time.Nanosecond)) }, nil},
		{"nome com 2 caracteres", func(in *CouponInput) { in.Name = "Ab" }, nil},
		{"nome com 200 caracteres acentuados", func(in *CouponInput) { in.Name = strings.Repeat("ç", 200) }, nil},
		{"nome com espaços nas bordas", func(in *CouponInput) { in.Name = "  Black Friday  " }, func(t *testing.T, c Coupon) {
			if c.Name != "Black Friday" {
				t.Fatalf("nome = %q", c.Name)
			}
		}},
		{"exatamente 100 planos", func(in *CouponInput) { in.AppliesToPlanIDs = uuidsOf(MaxCouponPlans) }, func(t *testing.T, c Coupon) {
			if len(c.AppliesToPlanIDs) != MaxCouponPlans {
				t.Fatalf("planos = %d", len(c.AppliesToPlanIDs))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoupon(t, tc.mutate)
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

func TestNewCouponCodeNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"promo10", "PROMO10"},
		{"  Black-Friday_2026 \t", "BLACK-FRIDAY_2026"},
		{"abc", "ABC"},
		{"___", "___"},
		{strings.Repeat("x", 40), strings.Repeat("X", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			c := newTestCoupon(t, func(in *CouponInput) { in.Code = tc.in })
			if c.Code != tc.want {
				t.Fatalf("código = %q, esperado %q", c.Code, tc.want)
			}
		})
	}
}

func TestNewCouponInvalid(t *testing.T) {
	future := testNow.Add(24 * time.Hour)
	later := testNow.Add(48 * time.Hour)
	past := testNow.Add(-time.Hour)
	earlier := testNow.Add(-2 * time.Hour)
	amount := func(in *CouponInput) { in.Type, in.PercentOffBP = DiscountAmount, nil }
	cases := []struct {
		name   string
		mutate func(*CouponInput)
		fields []string
	}{
		{"código vazio", func(in *CouponInput) { in.Code = "" }, []string{"code"}},
		{"código só com espaços", func(in *CouponInput) { in.Code = "   " }, []string{"code"}},
		{"código curto", func(in *CouponInput) { in.Code = "AB" }, []string{"code"}},
		{"código curto após remover espaços", func(in *CouponInput) { in.Code = "  ab  " }, []string{"code"}},
		{"código longo", func(in *CouponInput) { in.Code = strings.Repeat("A", 41) }, []string{"code"}},
		{"código com espaço interno", func(in *CouponInput) { in.Code = "BLACK FRIDAY" }, []string{"code"}},
		{"código acentuado", func(in *CouponInput) { in.Code = "PROMOÇÃO" }, []string{"code"}},
		{"código com ponto", func(in *CouponInput) { in.Code = "PROMO.10" }, []string{"code"}},
		{"nome vazio", func(in *CouponInput) { in.Name = "" }, []string{"name"}},
		{"nome curto após remover espaços", func(in *CouponInput) { in.Name = "  A  " }, []string{"name"}},
		{"nome longo", func(in *CouponInput) { in.Name = strings.Repeat("ç", 201) }, []string{"name"}},
		{"percentual ausente", func(in *CouponInput) { in.PercentOffBP = nil }, []string{"percent_off_bp"}},
		{"percentual zero", func(in *CouponInput) { in.PercentOffBP = ptr(0) }, []string{"percent_off_bp"}},
		{"percentual negativo", func(in *CouponInput) { in.PercentOffBP = ptr(-1) }, []string{"percent_off_bp"}},
		{"percentual acima de 100%", func(in *CouponInput) { in.PercentOffBP = ptr(10001) }, []string{"percent_off_bp"}},
		{"percentual com valor fixo", func(in *CouponInput) { in.AmountOff = ptr(int64(100)) }, []string{"amount_off"}},
		{"valor fixo ausente", amount, []string{"amount_off"}},
		{"valor fixo zero", func(in *CouponInput) { amount(in); in.AmountOff = ptr(int64(0)) }, []string{"amount_off"}},
		{"valor fixo negativo", func(in *CouponInput) { amount(in); in.AmountOff = ptr(int64(-100)) }, []string{"amount_off"}},
		{"valor fixo acima do limite", func(in *CouponInput) { amount(in); in.AmountOff = ptr(money.MaxAmount + 1) }, []string{"amount_off"}},
		{"valor fixo com percentual", func(in *CouponInput) {
			amount(in)
			in.AmountOff, in.PercentOffBP = ptr(int64(100)), ptr(500)
		}, []string{"percent_off_bp"}},
		{"tipo ausente", func(in *CouponInput) { in.Type = "" }, []string{"discount_type"}},
		{"tipo desconhecido", func(in *CouponInput) { in.Type = "fixed" }, []string{"discount_type"}},
		{"recorrente sem ciclos", func(in *CouponInput) { in.Duration = DurationRepeating }, []string{"duration_cycles"}},
		{"recorrente com zero ciclos", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(0) }, []string{"duration_cycles"}},
		{"recorrente acima de 120 ciclos", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(121) }, []string{"duration_cycles"}},
		{"uso único com ciclos", func(in *CouponInput) { in.DurationCycles = ptr(3) }, []string{"duration_cycles"}},
		{"sem prazo com ciclos", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationForever, ptr(3) }, []string{"duration_cycles"}},
		{"duração ausente", func(in *CouponInput) { in.Duration = "" }, []string{"duration"}},
		{"duração desconhecida", func(in *CouponInput) { in.Duration = "monthly" }, []string{"duration"}},
		{"limite de usos zero", func(in *CouponInput) { in.MaxRedemptions = ptr(0) }, []string{"max_redemptions"}},
		{"limite de usos negativo", func(in *CouponInput) { in.MaxRedemptions = ptr(-5) }, []string{"max_redemptions"}},
		{"início igual ao fim", func(in *CouponInput) { in.ValidFrom, in.ValidUntil = &future, &future }, []string{"valid_until"}},
		{"início após o fim", func(in *CouponInput) { in.ValidFrom, in.ValidUntil = &later, &future }, []string{"valid_until"}},
		{"fim no instante atual", func(in *CouponInput) { in.ValidUntil = ptr(testNow) }, []string{"valid_until"}},
		{"fim no passado", func(in *CouponInput) { in.ValidUntil = &past }, []string{"valid_until"}},
		{"intervalo invertido no passado", func(in *CouponInput) { in.ValidFrom, in.ValidUntil = &past, &earlier }, []string{"valid_until", "valid_until"}},
		{"mais de 100 planos", func(in *CouponInput) { in.AppliesToPlanIDs = uuidsOf(MaxCouponPlans + 1) }, []string{"applies_to_plan_ids"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, couponWith(tc.mutate), testNow)
			got := assertAppErr(t, err, apperr.KindInvalid, "invalid_coupon")
			if !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
			if c.ID != uuid.Nil || c.Code != "" || c.AppliesToPlanIDs != nil {
				t.Fatalf("cupom inválido não deve ser devolvido: %+v", c)
			}
		})
	}
}

func TestNewCouponCollectsAllErrors(t *testing.T) {
	_, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, CouponInput{
		Code: "x", Name: "", Type: "fixed", Duration: "weekly", MaxRedemptions: ptr(0),
		ValidUntil: ptr(testNow.Add(-time.Minute)), AppliesToPlanIDs: uuidsOf(MaxCouponPlans + 1),
	}, testNow)
	got := assertAppErr(t, err, apperr.KindInvalid, "invalid_coupon")
	want := []string{"code", "name", "discount_type", "duration", "max_redemptions", "valid_until", "applies_to_plan_ids"}
	if !slices.Equal(got, want) {
		t.Fatalf("campos = %v, esperado %v", got, want)
	}
}

// O limite de usos é gravado em coluna integer: valores acima do máximo de
// negócio (inclusive os que truncariam para int32) são rejeitados na
// validação, com erro de campo, em vez de gravados com outro valor.
func TestNewCouponMaxRedemptionsWithinStorageRange(t *testing.T) {
	for _, v := range []int64{MaxCouponRedemptions + 1, math.MaxInt32, int64(math.MaxInt32) + 1, 1<<32 + 1, math.MaxInt64} {
		_, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, couponWith(func(in *CouponInput) { in.MaxRedemptions = ptr(int(v)) }), testNow)
		got := assertAppErr(t, err, apperr.KindInvalid, "invalid_coupon")
		if !slices.Equal(got, []string{"max_redemptions"}) {
			t.Fatalf("max_redemptions=%d: campos = %v, esperado [max_redemptions]", v, got)
		}
	}
}

func TestNewCouponPlansDeduplicatedAndSorted(t *testing.T) {
	p1, p2, p3 := planUUID(1), planUUID(2), planUUID(3)
	input := []uuid.UUID{p3, p1, p3, p2, p1}
	original := slices.Clone(input)
	c := newTestCoupon(t, func(in *CouponInput) { in.AppliesToPlanIDs = input })
	if want := []uuid.UUID{p1, p2, p3}; !slices.Equal(c.AppliesToPlanIDs, want) {
		t.Fatalf("planos = %v, esperado %v", c.AppliesToPlanIDs, want)
	}
	if !slices.Equal(input, original) {
		t.Fatalf("a lista informada não deve ser alterada: %v", input)
	}
	input[0] = uuid.New()
	if c.AppliesToPlanIDs[2] != p3 {
		t.Fatal("o cupom não deve compartilhar a lista informada")
	}
}

func TestNewCouponEmptyPlanList(t *testing.T) {
	for name, plans := range map[string][]uuid.UUID{"nula": nil, "vazia": {}} {
		t.Run(name, func(t *testing.T) {
			c := newTestCoupon(t, func(in *CouponInput) { in.AppliesToPlanIDs = plans })
			if c.AppliesToPlanIDs == nil || len(c.AppliesToPlanIDs) != 0 {
				t.Fatalf("planos = %#v, esperado lista vazia", c.AppliesToPlanIDs)
			}
		})
	}
}

func TestNewCouponCopiesNumericInputs(t *testing.T) {
	bp, cycles := 1500, 6
	c := newTestCoupon(t, func(in *CouponInput) {
		in.PercentOffBP, in.Duration, in.DurationCycles = &bp, DurationRepeating, &cycles
	})
	bp, cycles = 9999, 99
	if *c.PercentOffBP != 1500 || *c.DurationCycles != 6 {
		t.Fatalf("o cupom deve guardar cópias: percentual %d, ciclos %d", *c.PercentOffBP, *c.DurationCycles)
	}
}

func FuzzNewCouponCode(f *testing.F) {
	for _, s := range []string{"PROMO10", " promo10 ", "ab", "BLACK FRIDAY", "PROMOÇÃO", "a-b_c", "", strings.Repeat("z", 40)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, code string) {
		c, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, couponWith(func(in *CouponInput) { in.Code = code }), testNow)
		if err != nil {
			got := assertAppErr(t, err, apperr.KindInvalid, "invalid_coupon")
			if !slices.Equal(got, []string{"code"}) {
				t.Fatalf("campos = %v para o código %q", got, code)
			}
			return
		}
		if !couponCodeRe.MatchString(c.Code) || c.Code != strings.ToUpper(strings.TrimSpace(code)) {
			t.Fatalf("código normalizado inválido: %q a partir de %q", c.Code, code)
		}
		again, err := NewCoupon(uuid.New(), uuid.New(), money.BRL, couponWith(func(in *CouponInput) { in.Code = c.Code }), testNow)
		if err != nil || again.Code != c.Code {
			t.Fatalf("a normalização deve ser estável: %q -> %q (%v)", c.Code, again.Code, err)
		}
	})
}

func TestCheckRedeemable(t *testing.T) {
	from := testNow.Add(-24 * time.Hour)
	until := testNow.Add(24 * time.Hour)
	plan, otherPlan := uuid.New(), uuid.New()
	cases := []struct {
		name   string
		coupon func(*Coupon)
		now    time.Time
		kind   apperr.Kind
		code   string
	}{
		{"ativo sem restrições", nil, testNow, 0, ""},
		{"arquivado", func(c *Coupon) { c.Status = Archived }, testNow, apperr.KindInvalid, "coupon_inactive"},
		{"antes do início da validade", func(c *Coupon) { c.ValidFrom = &from }, from.Add(-time.Nanosecond), apperr.KindInvalid, "coupon_not_yet_valid"},
		{"no início da validade", func(c *Coupon) { c.ValidFrom = &from }, from, 0, ""},
		{"pouco antes do fim da validade", func(c *Coupon) { c.ValidUntil = &until }, until.Add(-time.Nanosecond), 0, ""},
		{"no fim da validade", func(c *Coupon) { c.ValidUntil = &until }, until, apperr.KindInvalid, "coupon_expired"},
		{"após o fim da validade", func(c *Coupon) { c.ValidUntil = &until }, until.Add(time.Hour), apperr.KindInvalid, "coupon_expired"},
		{"dentro da janela", func(c *Coupon) { c.ValidFrom, c.ValidUntil = &from, &until }, testNow, 0, ""},
		{"abaixo do limite de usos", func(c *Coupon) { c.MaxRedemptions, c.RedemptionsCount = ptr(3), 2 }, testNow, 0, ""},
		{"no limite de usos", func(c *Coupon) { c.MaxRedemptions, c.RedemptionsCount = ptr(3), 3 }, testNow, apperr.KindConflict, "coupon_exhausted"},
		{"acima do limite de usos", func(c *Coupon) { c.MaxRedemptions, c.RedemptionsCount = ptr(3), 4 }, testNow, apperr.KindConflict, "coupon_exhausted"},
		{"sem limite com muitos usos", func(c *Coupon) { c.RedemptionsCount = 1_000_000 }, testNow, 0, ""},
		{"plano elegível", func(c *Coupon) { c.AppliesToPlanIDs = []uuid.UUID{otherPlan, plan} }, testNow, 0, ""},
		{"plano não elegível", func(c *Coupon) { c.AppliesToPlanIDs = []uuid.UUID{otherPlan} }, testNow, apperr.KindInvalid, "coupon_not_applicable"},
		{"arquivado prevalece sobre expirado", func(c *Coupon) { c.Status, c.ValidUntil = Archived, &until }, until, apperr.KindInvalid, "coupon_inactive"},
		{"fora da validade prevalece sobre limite", func(c *Coupon) {
			c.ValidFrom, c.MaxRedemptions, c.RedemptionsCount = &from, ptr(1), 1
		}, from.Add(-time.Hour), apperr.KindInvalid, "coupon_not_yet_valid"},
		{"expirado prevalece sobre limite", func(c *Coupon) {
			c.ValidUntil, c.MaxRedemptions, c.RedemptionsCount = &until, ptr(1), 1
		}, until, apperr.KindInvalid, "coupon_expired"},
		{"limite prevalece sobre plano", func(c *Coupon) {
			c.MaxRedemptions, c.RedemptionsCount, c.AppliesToPlanIDs = ptr(1), 1, []uuid.UUID{otherPlan}
		}, testNow, apperr.KindConflict, "coupon_exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoupon(t, nil)
			if tc.coupon != nil {
				tc.coupon(&c)
			}
			err := c.CheckRedeemable(plan, tc.now)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("cupom deveria ser aplicável: %v", err)
				}
				return
			}
			assertAppErr(t, err, tc.kind, tc.code)
		})
	}
}

func TestAppliesTo(t *testing.T) {
	plan, otherPlan := uuid.New(), uuid.New()
	cases := []struct {
		name  string
		plans []uuid.UUID
		want  bool
	}{
		{"lista nula vale para todos", nil, true},
		{"lista vazia vale para todos", []uuid.UUID{}, true},
		{"plano na lista", []uuid.UUID{otherPlan, plan}, true},
		{"plano fora da lista", []uuid.UUID{otherPlan}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Coupon{AppliesToPlanIDs: tc.plans}
			if got := c.AppliesTo(plan); got != tc.want {
				t.Fatalf("AppliesTo = %v, esperado %v", got, tc.want)
			}
		})
	}
}

func TestInitialCycles(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CouponInput)
		want   *int
	}{
		{"uso único", nil, ptr(1)},
		{"recorrente", func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(3) }, ptr(3)},
		{"sem prazo", func(in *CouponInput) { in.Duration = DurationForever }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoupon(t, tc.mutate)
			got := c.InitialCycles()
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("ciclos iniciais = %v, esperado %v", deref(got), deref(tc.want))
			}
		})
	}
}

func TestInitialCyclesReturnsCopy(t *testing.T) {
	c := newTestCoupon(t, func(in *CouponInput) { in.Duration, in.DurationCycles = DurationRepeating, ptr(4) })
	first := c.InitialCycles()
	*first = 0
	if *c.DurationCycles != 4 || *c.InitialCycles() != 4 {
		t.Fatalf("alterar o retorno não deve afetar o cupom: ciclos %d", *c.DurationCycles)
	}
}

func deref(p *int) any {
	if p == nil {
		return "nil"
	}
	return *p
}
