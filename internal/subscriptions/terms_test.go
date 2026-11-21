package subscriptions

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

type termsFixture struct {
	store    *fakeTermsStore
	svc      *TermsService
	clock    *clock.Manual
	tenantID uuid.UUID
	by       uuid.UUID
	actor    audit.Actor
	sub      Subscription
	price    catalog.PriceVersion
	coupon   catalog.Coupon
}

func newTermsFixture(t *testing.T) *termsFixture {
	t.Helper()
	st := newFakeTermsStore()
	f := &termsFixture{store: st, clock: clock.NewManual(fixtureNow), tenantID: uuid.New(), by: uuid.New()}
	f.actor = audit.User(f.by, "Ana Souza")
	plan := fixturePlan(calendar.Monthly)
	f.price = priceFlat(plan, 9900)
	start := local(2026, time.November, 1, 12, 0)
	f.sub = mustNew(t, NewInput{ID: uuid.New(), TenantID: f.tenantID, CustomerID: uuid.New(), Plan: plan, Price: f.price,
		Quantity: 1, StartAt: start, Location: saoPaulo, Now: start})
	billOnce(t, &f.sub, start)
	f.sub.PopEvents()
	pct, cycles, maxRedemptions := 1000, 3, 10
	f.coupon = catalog.Coupon{ID: uuid.New(), TenantID: f.tenantID, Code: "BEMVINDO10", Name: "Boas-vindas", Type: catalog.DiscountPercent,
		PercentOffBP: &pct, Currency: money.BRL, Duration: catalog.DurationRepeating, DurationCycles: &cycles,
		MaxRedemptions: &maxRedemptions, RedemptionsCount: 2, Status: catalog.Active, CreatedAt: start, Version: 1}
	st.subs[f.sub.ID] = f.sub
	st.prices[f.price.ID] = f.price
	st.coupons[f.coupon.Code] = f.coupon
	f.svc = NewTermsService(st, f.clock)
	return f
}

func (f *termsFixture) editCoupon(mod func(*catalog.Coupon)) {
	c := f.store.coupons[f.coupon.Code]
	mod(&c)
	f.store.coupons[f.coupon.Code] = c
}

func (f *termsFixture) editSubscription(mod func(*Subscription)) {
	s := f.store.subs[f.sub.ID]
	mod(&s)
	f.store.subs[f.sub.ID] = s
}

func (f *termsFixture) redemptions() int { return f.store.coupons[f.coupon.Code].RedemptionsCount }

func (f *termsFixture) assertNothingRecorded(t *testing.T) {
	t.Helper()
	if len(f.store.histories) != 0 || len(f.store.events) != 0 || len(f.store.audits) != 0 {
		t.Fatalf("nada deveria ser gravado: hist=%d eventos=%d auditoria=%d", len(f.store.histories), len(f.store.events), len(f.store.audits))
	}
}

// assertRecorded verifica histórico, evento da outbox e auditoria gravados na
// mesma transação da alteração, com o instante do relógio do serviço.
func (f *termsFixture) assertRecorded(t *testing.T, histEvent, action string, details map[string]any) {
	t.Helper()
	now := f.clock.Now()
	if len(f.store.histories) != 1 {
		t.Fatalf("esperava um registro de histórico, obtive %d", len(f.store.histories))
	}
	h := f.store.histories[0]
	if h.subscriptionID != f.sub.ID || h.actor != f.actor || len(h.events) != 1 {
		t.Fatalf("histórico: %+v", h)
	}
	ev := h.events[0]
	if ev.Event != histEvent || ev.From != nil || ev.To != f.sub.Status || !ev.At.Equal(now) {
		t.Fatalf("evento de histórico: %+v", ev)
	}
	if len(f.store.events) != 1 {
		t.Fatalf("esperava um evento na outbox, obtive %d", len(f.store.events))
	}
	e := f.store.events[0]
	if e.Type != outbox.EventSubscriptionUpdated || e.TenantID != f.tenantID || e.AggregateType != "subscription" || e.AggregateID != f.sub.ID || !e.OccurredAt.Equal(now) {
		t.Fatalf("evento da outbox: %+v", e)
	}
	payload, ok := e.Payload.(map[string]any)
	if !ok || payload["subscription_id"] != f.sub.ID || payload["status"] != f.sub.Status || payload["change"] != action {
		t.Fatalf("conteúdo do evento: %+v", e.Payload)
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("esperava uma entrada de auditoria, obtive %d", len(f.store.audits))
	}
	a := f.store.audits[0]
	if a.Action != action || a.TenantID != f.tenantID || a.Actor != f.actor || a.ResourceType != "subscription" || a.ResourceID != f.sub.ID.String() || !a.OccurredAt.Equal(now) {
		t.Fatalf("auditoria: %+v", a)
	}
	if len(a.Details) != len(details) || len(ev.Details) != len(details) {
		t.Fatalf("detalhes: auditoria=%+v histórico=%+v, esperava %+v", a.Details, ev.Details, details)
	}
	for k, v := range details {
		if a.Details[k] != v || ev.Details[k] != v {
			t.Fatalf("detalhe %q: auditoria=%v histórico=%v, esperava %v", k, a.Details[k], ev.Details[k], v)
		}
	}
}

func TestTermsServiceApplyCoupon(t *testing.T) {
	f := newTermsFixture(t)
	f.clock.Advance(time.Hour)
	now := f.clock.Now()

	d, err := f.svc.ApplyCoupon(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, "  bemvindo10 ")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.store.couponLookups, []string{"BEMVINDO10"}) {
		t.Fatalf("o código deve ser normalizado: %v", f.store.couponLookups)
	}
	if !slices.Equal(f.store.tenants, []uuid.UUID{f.tenantID}) || !slices.Equal(f.store.forUpdate, []bool{true}) {
		t.Fatalf("transação do tenant com bloqueio da assinatura: tenants=%v bloqueio=%v", f.store.tenants, f.store.forUpdate)
	}
	if d.ID == uuid.Nil || d.SubscriptionID != f.sub.ID || d.CouponID != f.coupon.ID || d.CouponCode != "BEMVINDO10" ||
		d.Status != DiscountActive || !d.CreatedAt.Equal(now) || d.RemainingCycles == nil || *d.RemainingCycles != 3 {
		t.Fatalf("desconto: %+v", d)
	}
	if len(f.store.discounts) != 1 || f.store.discounts[0].ID != d.ID {
		t.Fatalf("desconto gravado: %+v", f.store.discounts)
	}
	if by := f.store.appliedBy[d.ID]; by == nil || *by != f.by {
		t.Fatalf("responsável pela aplicação: %v", by)
	}
	if f.redemptions() != 3 {
		t.Fatalf("o uso do cupom deve ser contado: %d", f.redemptions())
	}
	f.assertRecorded(t, "discount_applied", "subscription.discount_applied",
		map[string]any{"coupon_id": f.coupon.ID, "coupon_code": "BEMVINDO10", "discount_id": d.ID})
}

func TestTermsServiceApplyCouponCycles(t *testing.T) {
	cases := []struct {
		name     string
		duration catalog.CouponDuration
		cycles   *int
		want     *int
	}{
		{"uma fatura", catalog.DurationOnce, nil, intPtr(1)},
		{"ciclos limitados", catalog.DurationRepeating, intPtr(6), intPtr(6)},
		{"sem limite de ciclos", catalog.DurationForever, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			f.editCoupon(func(cp *catalog.Coupon) { cp.Duration, cp.DurationCycles = c.duration, c.cycles })
			d, err := f.svc.ApplyCoupon(context.Background(), f.tenantID, f.actor, nil, f.sub.ID, "BEMVINDO10")
			if err != nil {
				t.Fatal(err)
			}
			if (d.RemainingCycles == nil) != (c.want == nil) || (c.want != nil && *d.RemainingCycles != *c.want) {
				t.Fatalf("ciclos restantes: obtive %v, esperava %v", d.RemainingCycles, c.want)
			}
			if f.store.appliedBy[d.ID] != nil {
				t.Fatal("aplicação sem usuário responsável deve ficar sem autor")
			}
		})
	}
}

func TestTermsServiceApplyCouponRules(t *testing.T) {
	cases := []struct {
		name   string
		prep   func(*termsFixture)
		code   string
		kind   apperr.Kind
		errID  string
		fields []string
	}{
		{"assinatura inexistente", func(f *termsFixture) { delete(f.store.subs, f.sub.ID) }, "BEMVINDO10",
			apperr.KindNotFound, "subscription_not_found", nil},
		{"assinatura de outro tenant", func(f *termsFixture) { f.editSubscription(func(s *Subscription) { s.TenantID = uuid.New() }) }, "BEMVINDO10",
			apperr.KindNotFound, "subscription_not_found", nil},
		{"assinatura cancelada", func(f *termsFixture) { f.editSubscription(func(s *Subscription) { s.Status = Canceled }) }, "BEMVINDO10",
			apperr.KindConflict, "invalid_transition", nil},
		{"cupom inexistente", nil, "NAOEXISTE", apperr.KindInvalid, "coupon_not_found", []string{"coupon_code"}},
		{"cupom arquivado", func(f *termsFixture) { f.editCoupon(func(c *catalog.Coupon) { c.Status = catalog.Archived }) }, "BEMVINDO10",
			apperr.KindInvalid, "coupon_inactive", nil},
		{"cupom ainda não válido", func(f *termsFixture) {
			from := f.clock.Now().Add(24 * time.Hour)
			f.editCoupon(func(c *catalog.Coupon) { c.ValidFrom = &from })
		}, "BEMVINDO10", apperr.KindInvalid, "coupon_not_yet_valid", nil},
		{"cupom expirado no instante da aplicação", func(f *termsFixture) {
			until := f.clock.Now()
			f.editCoupon(func(c *catalog.Coupon) { c.ValidUntil = &until })
		}, "BEMVINDO10", apperr.KindInvalid, "coupon_expired", nil},
		{"cupom esgotado", func(f *termsFixture) { f.editCoupon(func(c *catalog.Coupon) { c.RedemptionsCount = 10 }) }, "BEMVINDO10",
			apperr.KindConflict, "coupon_exhausted", nil},
		{"cupom de outro plano", func(f *termsFixture) {
			f.editCoupon(func(c *catalog.Coupon) { c.AppliesToPlanIDs = []uuid.UUID{uuid.New()} })
		}, "BEMVINDO10", apperr.KindInvalid, "coupon_not_applicable", nil},
		{"desconto já ativo", func(f *termsFixture) {
			f.store.discounts = []Discount{{ID: uuid.New(), SubscriptionID: f.sub.ID, CouponID: uuid.New(), CouponCode: "ANTIGO", Status: DiscountActive}}
		}, "BEMVINDO10", apperr.KindConflict, "discount_already_active", nil},
		{"último uso consumido por outra transação", func(f *termsFixture) { f.store.redeemRace = true }, "BEMVINDO10",
			apperr.KindConflict, "coupon_exhausted", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			if c.prep != nil {
				c.prep(f)
			}
			redemptions, discounts := f.redemptions(), len(f.store.discounts)
			_, err := f.svc.ApplyCoupon(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, c.code)
			wantKind(t, err, c.kind, c.errID)
			if got := fieldNames(err); !slices.Equal(got, c.fields) {
				t.Fatalf("campos: obtive %v, esperava %v", got, c.fields)
			}
			if f.redemptions() != redemptions || len(f.store.discounts) != discounts || len(f.store.appliedBy) != 0 {
				t.Fatalf("regra violada não pode consumir o cupom: usos=%d descontos=%d", f.redemptions(), len(f.store.discounts))
			}
			f.assertNothingRecorded(t)
		})
	}
}

func TestTermsServiceApplyCouponRollsBackOnStoreFailure(t *testing.T) {
	for _, method := range []string{"Get", "CouponByCode", "ActiveDiscount", "RedeemCoupon", "InsertDiscount", "InsertHistory", "Emit", "Audit"} {
		t.Run(method, func(t *testing.T) {
			f := newTermsFixture(t)
			injected := errors.New("falha de armazenamento em " + method)
			f.store.errs[method] = injected
			_, err := f.svc.ApplyCoupon(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, "BEMVINDO10")
			if !errors.Is(err, injected) {
				t.Fatalf("esperava a falha injetada, obtive %v", err)
			}
			if f.redemptions() != 2 || len(f.store.discounts) != 0 || len(f.store.appliedBy) != 0 {
				t.Fatalf("falha deve desfazer o resgate e o desconto: usos=%d descontos=%d", f.redemptions(), len(f.store.discounts))
			}
			f.assertNothingRecorded(t)
		})
	}
}

func TestTermsServiceRemoveDiscount(t *testing.T) {
	active := func(f *termsFixture) Discount {
		d := Discount{ID: uuid.New(), SubscriptionID: f.sub.ID, CouponID: f.coupon.ID, CouponCode: f.coupon.Code, Status: DiscountActive}
		f.store.discounts = []Discount{d}
		return d
	}

	t.Run("encerra o desconto ativo", func(t *testing.T) {
		f := newTermsFixture(t)
		active(f)
		f.clock.Advance(2 * time.Hour)
		if err := f.svc.RemoveDiscount(context.Background(), f.tenantID, f.actor, f.sub.ID); err != nil {
			t.Fatal(err)
		}
		if f.store.discounts[0].Status != DiscountEnded || !slices.Equal(f.store.forUpdate, []bool{true}) {
			t.Fatalf("desconto deve ser encerrado com a assinatura bloqueada: %+v", f.store.discounts)
		}
		f.assertRecorded(t, "discount_removed", "subscription.discount_removed", map[string]any{})
	})

	t.Run("sem desconto ativo não grava nada", func(t *testing.T) {
		f := newTermsFixture(t)
		f.store.discounts = []Discount{{ID: uuid.New(), SubscriptionID: f.sub.ID, Status: DiscountEnded}}
		if err := f.svc.RemoveDiscount(context.Background(), f.tenantID, f.actor, f.sub.ID); err != nil {
			t.Fatal(err)
		}
		f.assertNothingRecorded(t)
	})

	t.Run("desconto de outra assinatura não é encerrado", func(t *testing.T) {
		f := newTermsFixture(t)
		f.store.discounts = []Discount{{ID: uuid.New(), SubscriptionID: uuid.New(), Status: DiscountActive}}
		if err := f.svc.RemoveDiscount(context.Background(), f.tenantID, f.actor, f.sub.ID); err != nil {
			t.Fatal(err)
		}
		if f.store.discounts[0].Status != DiscountActive {
			t.Fatal("o desconto de outra assinatura deve continuar ativo")
		}
		f.assertNothingRecorded(t)
	})

	t.Run("assinatura inexistente", func(t *testing.T) {
		f := newTermsFixture(t)
		active(f)
		err := f.svc.RemoveDiscount(context.Background(), f.tenantID, f.actor, uuid.New())
		wantKind(t, err, apperr.KindNotFound, "subscription_not_found")
		if f.store.discounts[0].Status != DiscountActive {
			t.Fatal("o desconto não pode ser encerrado")
		}
	})

	for _, method := range []string{"EndActiveDiscount", "InsertHistory", "Emit", "Audit"} {
		t.Run("falha em "+method+" desfaz o encerramento", func(t *testing.T) {
			f := newTermsFixture(t)
			active(f)
			injected := errors.New("falha de armazenamento em " + method)
			f.store.errs[method] = injected
			err := f.svc.RemoveDiscount(context.Background(), f.tenantID, f.actor, f.sub.ID)
			if !errors.Is(err, injected) {
				t.Fatalf("esperava a falha injetada, obtive %v", err)
			}
			if f.store.discounts[0].Status != DiscountActive {
				t.Fatal("falha deve manter o desconto ativo")
			}
			f.assertNothingRecorded(t)
		})
	}
}

func TestTermsServiceScheduleAdjustment(t *testing.T) {
	t.Run("percentual", func(t *testing.T) {
		f := newTermsFixture(t)
		f.clock.Advance(time.Hour)
		a, err := f.svc.ScheduleAdjustment(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, percentInput(t, 450))
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == uuid.Nil || a.SubscriptionID != f.sub.ID || a.TenantID != f.tenantID || a.Status != AdjustmentScheduled ||
			!a.CreatedAt.Equal(f.clock.Now()) || a.CreatedBy == nil || *a.CreatedBy != f.by {
			t.Fatalf("reajuste: %+v", a)
		}
		if len(f.store.adjustments) != 1 || f.store.adjustments[0].ID != a.ID {
			t.Fatalf("reajuste gravado: %+v", f.store.adjustments)
		}
		if !slices.Equal(f.store.forUpdate, []bool{true}) {
			t.Fatal("a assinatura deve ser bloqueada")
		}
		f.assertRecorded(t, "price_adjustment_scheduled", "subscription.price_adjustment_scheduled",
			map[string]any{"adjustment_id": a.ID, "effective_date": "2026-12-01", "kind": AdjustPercent, "percent_bp": 450})
	})

	t.Run("valor fixo", func(t *testing.T) {
		f := newTermsFixture(t)
		a, err := f.svc.ScheduleAdjustment(context.Background(), f.tenantID, f.actor, nil, f.sub.ID, fixedInput(t, 12900))
		if err != nil {
			t.Fatal(err)
		}
		f.assertRecorded(t, "price_adjustment_scheduled", "subscription.price_adjustment_scheduled",
			map[string]any{"adjustment_id": a.ID, "effective_date": "2026-12-01", "kind": AdjustFixedAmount, "new_amount": int64(12900)})
	})

	cases := []struct {
		name string
		prep func(*termsFixture)
		in   func(*testing.T) AdjustmentInput
		kind apperr.Kind
		code string
	}{
		{"entrada inválida", nil, func(t *testing.T) AdjustmentInput {
			in := percentInput(t, 450)
			in.Reason = ""
			return in
		}, apperr.KindInvalid, "invalid_adjustment"},
		{"data efetiva passada pelo relógio do serviço", func(f *termsFixture) { f.clock.Set(local(2026, time.December, 2, 9, 0)) },
			func(t *testing.T) AdjustmentInput { return percentInput(t, 450) }, apperr.KindInvalid, "invalid_adjustment"},
		{"assinatura com cancelamento agendado", func(f *termsFixture) { f.editSubscription(func(s *Subscription) { s.CancelAtPeriodEnd = true }) },
			func(t *testing.T) AdjustmentInput { return percentInput(t, 450) }, apperr.KindConflict, "subscription_ending"},
		{"assinatura inexistente", func(f *termsFixture) { delete(f.store.subs, f.sub.ID) },
			func(t *testing.T) AdjustmentInput { return percentInput(t, 450) }, apperr.KindNotFound, "subscription_not_found"},
		{"preço inexistente", func(f *termsFixture) { delete(f.store.prices, f.price.ID) },
			func(t *testing.T) AdjustmentInput { return percentInput(t, 450) }, apperr.KindNotFound, "price_not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			if c.prep != nil {
				c.prep(f)
			}
			_, err := f.svc.ScheduleAdjustment(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, c.in(t))
			wantKind(t, err, c.kind, c.code)
			if len(f.store.adjustments) != 0 {
				t.Fatal("reajuste rejeitado não pode ser gravado")
			}
			f.assertNothingRecorded(t)
		})
	}

	for _, method := range []string{"Get", "PriceVersion", "InsertAdjustment", "InsertHistory", "Emit", "Audit"} {
		t.Run("falha em "+method+" desfaz o agendamento", func(t *testing.T) {
			f := newTermsFixture(t)
			injected := errors.New("falha de armazenamento em " + method)
			f.store.errs[method] = injected
			_, err := f.svc.ScheduleAdjustment(context.Background(), f.tenantID, f.actor, &f.by, f.sub.ID, percentInput(t, 450))
			if !errors.Is(err, injected) {
				t.Fatalf("esperava a falha injetada, obtive %v", err)
			}
			if len(f.store.adjustments) != 0 {
				t.Fatal("falha deve desfazer o reajuste")
			}
			f.assertNothingRecorded(t)
		})
	}
}

func TestTermsServiceAdjustments(t *testing.T) {
	f := newTermsFixture(t)
	mine := []PriceAdjustment{
		{ID: uuid.New(), SubscriptionID: f.sub.ID, Status: AdjustmentApplied},
		{ID: uuid.New(), SubscriptionID: f.sub.ID, Status: AdjustmentScheduled},
	}
	f.store.adjustments = append(slices.Clone(mine), PriceAdjustment{ID: uuid.New(), SubscriptionID: uuid.New(), Status: AdjustmentScheduled})

	f.store.adjustmentNext = "seguinte"
	got, err := f.svc.Adjustments(context.Background(), f.tenantID, f.sub.ID, 10, "atual")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].ID != mine[0].ID || got.Items[1].ID != mine[1].ID {
		t.Fatalf("somente os reajustes da assinatura: %+v", got)
	}
	// O limite e o cursor chegam ao armazenamento, e o cursor seguinte volta.
	if f.store.adjustmentPage != "10|atual" || got.NextCursor != "seguinte" {
		t.Fatalf("página pedida %q, cursor devolvido %q", f.store.adjustmentPage, got.NextCursor)
	}
	if !slices.Equal(f.store.forUpdate, []bool{false}) || !slices.Equal(f.store.tenants, []uuid.UUID{f.tenantID}) {
		t.Fatalf("consulta sem bloqueio na transação do tenant: bloqueio=%v tenants=%v", f.store.forUpdate, f.store.tenants)
	}

	t.Run("assinatura inexistente", func(t *testing.T) {
		got, err := f.svc.Adjustments(context.Background(), f.tenantID, uuid.New(), 25, "")
		wantKind(t, err, apperr.KindNotFound, "subscription_not_found")
		if len(got.Items) != 0 {
			t.Fatalf("nada deveria ser listado: %+v", got)
		}
	})

	t.Run("assinatura de outro tenant", func(t *testing.T) {
		_, err := f.svc.Adjustments(context.Background(), uuid.New(), f.sub.ID, 25, "")
		wantKind(t, err, apperr.KindNotFound, "subscription_not_found")
	})

	t.Run("falha na consulta", func(t *testing.T) {
		injected := errors.New("consulta indisponível")
		f.store.errs["Adjustments"] = injected
		t.Cleanup(func() { delete(f.store.errs, "Adjustments") })
		if _, err := f.svc.Adjustments(context.Background(), f.tenantID, f.sub.ID, 25, ""); !errors.Is(err, injected) {
			t.Fatalf("esperava a falha injetada, obtive %v", err)
		}
	})
}

func TestTermsServiceCancelAdjustment(t *testing.T) {
	scheduled := func(t *testing.T, f *termsFixture) PriceAdjustment {
		t.Helper()
		a := scheduledAdjustment(t, f.sub, f.price, percentInput(t, 450))
		f.store.adjustments = []PriceAdjustment{a}
		return a
	}

	t.Run("cancela o reajuste programado", func(t *testing.T) {
		f := newTermsFixture(t)
		a := scheduled(t, f)
		f.clock.Advance(3 * time.Hour)
		if err := f.svc.CancelAdjustment(context.Background(), f.tenantID, f.actor, f.sub.ID, a.ID); err != nil {
			t.Fatal(err)
		}
		stored := f.store.adjustments[0]
		if stored.Status != AdjustmentCanceled || stored.CanceledAt == nil || !stored.CanceledAt.Equal(f.clock.Now()) ||
			stored.CancelReason != AdjustmentCanceledManually {
			t.Fatalf("reajuste cancelado: %+v", stored)
		}
		if !slices.Equal(f.store.forUpdate, []bool{true}) {
			t.Fatal("a assinatura deve ser bloqueada")
		}
		f.assertRecorded(t, "price_adjustment_canceled", "subscription.price_adjustment_canceled",
			map[string]any{"adjustment_id": a.ID, "reason": AdjustmentCanceledManually})
	})

	cases := []struct {
		name string
		prep func(*termsFixture, *PriceAdjustment) (subID, adjID uuid.UUID)
		kind apperr.Kind
		code string
	}{
		{"reajuste já aplicado", func(f *termsFixture, a *PriceAdjustment) (uuid.UUID, uuid.UUID) {
			f.store.adjustments[0].Status = AdjustmentApplied
			return f.sub.ID, a.ID
		}, apperr.KindConflict, "adjustment_not_scheduled"},
		{"reajuste inexistente", func(f *termsFixture, _ *PriceAdjustment) (uuid.UUID, uuid.UUID) {
			return f.sub.ID, uuid.New()
		}, apperr.KindNotFound, "adjustment_not_found"},
		{"reajuste de outra assinatura", func(f *termsFixture, a *PriceAdjustment) (uuid.UUID, uuid.UUID) {
			other := f.sub
			other.ID = uuid.New()
			f.store.subs[other.ID] = other
			return other.ID, a.ID
		}, apperr.KindNotFound, "adjustment_not_found"},
		{"assinatura inexistente", func(_ *termsFixture, a *PriceAdjustment) (uuid.UUID, uuid.UUID) {
			return uuid.New(), a.ID
		}, apperr.KindNotFound, "subscription_not_found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTermsFixture(t)
			a := scheduled(t, f)
			subID, adjID := c.prep(f, &a)
			before := f.store.adjustments[0]
			err := f.svc.CancelAdjustment(context.Background(), f.tenantID, f.actor, subID, adjID)
			wantKind(t, err, c.kind, c.code)
			if f.store.adjustments[0].Status != before.Status || f.store.adjustments[0].CanceledAt != nil {
				t.Fatalf("reajuste não pode mudar: %+v", f.store.adjustments[0])
			}
			f.assertNothingRecorded(t)
		})
	}

	for _, method := range []string{"Get", "Adjustment", "CancelAdjustment", "InsertHistory", "Emit", "Audit"} {
		t.Run("falha em "+method+" desfaz o cancelamento", func(t *testing.T) {
			f := newTermsFixture(t)
			a := scheduled(t, f)
			injected := errors.New("falha de armazenamento em " + method)
			f.store.errs[method] = injected
			err := f.svc.CancelAdjustment(context.Background(), f.tenantID, f.actor, f.sub.ID, a.ID)
			if !errors.Is(err, injected) {
				t.Fatalf("esperava a falha injetada, obtive %v", err)
			}
			if stored := f.store.adjustments[0]; stored.Status != AdjustmentScheduled || stored.CanceledAt != nil {
				t.Fatalf("falha deve manter o reajuste programado: %+v", stored)
			}
			f.assertNothingRecorded(t)
		})
	}
}

func TestTermsServiceTerms(t *testing.T) {
	f := newTermsFixture(t)
	qty := 5
	change := ScheduledChange{ID: uuid.New(), TenantID: f.tenantID, SubscriptionID: f.sub.ID, Kind: QuantityChange, NewQuantity: &qty,
		EffectiveAt: f.sub.NextChangeAt(), Status: ChangePending}
	f.store.changes[f.sub.ID] = []ScheduledChange{change}
	discount := Discount{ID: uuid.New(), SubscriptionID: f.sub.ID, CouponID: f.coupon.ID, CouponCode: f.coupon.Code, Status: DiscountActive}
	f.store.discounts = []Discount{discount}

	got, err := f.svc.Terms(context.Background(), f.tenantID, f.sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ScheduledChanges) != 1 || got.ScheduledChanges[0].ID != change.ID || got.Discount == nil || got.Discount.ID != discount.ID {
		t.Fatalf("condições vigentes: %+v", got)
	}
	if !slices.Equal(f.store.tenants, []uuid.UUID{f.tenantID}) {
		t.Fatalf("consulta na transação do tenant: %v", f.store.tenants)
	}

	t.Run("sem mudanças nem desconto", func(t *testing.T) {
		got, err := f.svc.Terms(context.Background(), f.tenantID, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		if got.ScheduledChanges != nil || got.Discount != nil {
			t.Fatalf("condições vazias esperadas: %+v", got)
		}
	})

	for _, method := range []string{"ScheduledChanges", "ActiveDiscount"} {
		t.Run("falha em "+method, func(t *testing.T) {
			injected := errors.New("consulta indisponível em " + method)
			f.store.errs[method] = injected
			t.Cleanup(func() { delete(f.store.errs, method) })
			if _, err := f.svc.Terms(context.Background(), f.tenantID, f.sub.ID); !errors.Is(err, injected) {
				t.Fatalf("esperava a falha injetada, obtive %v", err)
			}
		})
	}
}
