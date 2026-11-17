package catalog

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
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

type couponEnv struct {
	store    *fakeCouponStore
	clock    *clock.Manual
	svc      *CouponService
	tenantID uuid.UUID
	actor    audit.Actor
}

func newCouponEnv() *couponEnv {
	store := newFakeCouponStore()
	c := clock.NewManual(testNow)
	return &couponEnv{store: store, clock: c, svc: NewCouponService(store, c), tenantID: uuid.New(), actor: audit.User(uuid.New(), "Ana")}
}

func (e *couponEnv) st() *couponState { return e.store.state(e.tenantID) }

// seedPlans registra planos existentes no tenant informado.
func (e *couponEnv) seedPlans(tenantID uuid.UUID, n int) []uuid.UUID {
	st := e.store.state(tenantID)
	out := uuidsOf(n)
	for _, id := range out {
		st.plans[id] = true
	}
	return out
}

// create cadastra um cupom pelo serviço ou falha o teste.
func (e *couponEnv) create(t *testing.T, mutate func(*CouponInput)) Coupon {
	t.Helper()
	c, err := e.svc.Create(context.Background(), e.tenantID, e.actor, couponWith(mutate))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c
}

func (e *couponEnv) lastAudit(t *testing.T) audit.Entry {
	t.Helper()
	a := e.st().audits
	if len(a) == 0 {
		t.Fatal("nenhuma entrada de auditoria")
	}
	return a[len(a)-1]
}

func TestCouponServiceCreate(t *testing.T) {
	e := newCouponEnv()
	e.clock.Advance(90 * time.Minute)
	c, err := e.svc.Create(context.Background(), e.tenantID, e.actor, couponWith(func(in *CouponInput) { in.Code = " promo10 " }))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID.Version() != 7 || c.TenantID != e.tenantID || c.Code != "PROMO10" || c.Currency != money.BRL ||
		c.Status != Active || c.Version != 1 || !c.CreatedAt.Equal(e.clock.Now()) {
		t.Fatalf("cupom inesperado: %+v", c)
	}
	stored, ok := e.st().coupons[c.ID]
	if !ok || stored.Code != c.Code || stored.TenantID != e.tenantID {
		t.Fatalf("cupom não gravado no tenant: %+v", stored)
	}
	if n := e.store.calls["CountPlans"]; n != 0 {
		t.Fatalf("sem planos elegíveis, CountPlans não deve ser chamado (%d chamadas)", n)
	}
	a := e.lastAudit(t)
	if a.Action != "coupon.created" || a.ResourceType != "coupon" || a.ResourceID != c.ID.String() || a.TenantID != e.tenantID ||
		a.Actor != e.actor || !a.OccurredAt.Equal(e.clock.Now()) {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	if a.Details["code"] != "PROMO10" || a.Details["discount_type"] != DiscountPercent || a.Details["duration"] != DurationOnce {
		t.Fatalf("detalhes da auditoria inesperados: %v", a.Details)
	}
}

func TestCouponServiceCreateWithPlans(t *testing.T) {
	e := newCouponEnv()
	plans := e.seedPlans(e.tenantID, 3)
	c := e.create(t, func(in *CouponInput) { in.AppliesToPlanIDs = []uuid.UUID{plans[2], plans[0], plans[2], plans[1]} })
	want := slices.Clone(plans)
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(c.AppliesToPlanIDs, want) {
		t.Fatalf("planos = %v, esperado %v", c.AppliesToPlanIDs, want)
	}
	if !slices.Equal(e.store.countedPlans, want) {
		t.Fatalf("CountPlans recebeu %v, esperado a lista sem duplicados %v", e.store.countedPlans, want)
	}
	if !slices.Equal(e.st().coupons[c.ID].AppliesToPlanIDs, want) {
		t.Fatalf("planos gravados = %v", e.st().coupons[c.ID].AppliesToPlanIDs)
	}
}

func TestCouponServiceCreateRejected(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*testing.T, *couponEnv) CouponInput
		kind   apperr.Kind
		code   string
		fields []string
		// txCalls é o número esperado de transações abertas.
		txCalls int
	}{
		{"entrada inválida não abre transação", func(*testing.T, *couponEnv) CouponInput {
			return couponWith(func(in *CouponInput) { in.Code = "x" })
		}, apperr.KindInvalid, "invalid_coupon", []string{"code"}, 0},
		{"validade vencida pelo relógio", func(_ *testing.T, e *couponEnv) CouponInput {
			until := e.clock.Now().Add(time.Hour)
			e.clock.Advance(2 * time.Hour)
			return couponWith(func(in *CouponInput) { in.ValidUntil = &until })
		}, apperr.KindInvalid, "invalid_coupon", []string{"valid_until"}, 0},
		{"plano inexistente", func(_ *testing.T, e *couponEnv) CouponInput {
			plans := e.seedPlans(e.tenantID, 1)
			return couponWith(func(in *CouponInput) { in.AppliesToPlanIDs = []uuid.UUID{plans[0], uuid.New()} })
		}, apperr.KindInvalid, "invalid_coupon", []string{"applies_to_plan_ids"}, 1},
		{"plano de outro tenant", func(_ *testing.T, e *couponEnv) CouponInput {
			plans := e.seedPlans(uuid.New(), 1)
			return couponWith(func(in *CouponInput) { in.AppliesToPlanIDs = plans })
		}, apperr.KindInvalid, "invalid_coupon", []string{"applies_to_plan_ids"}, 1},
		{"código já usado no tenant", func(t *testing.T, e *couponEnv) CouponInput {
			e.create(t, nil)
			return couponWith(func(in *CouponInput) { in.Code = "  promo10" })
		}, apperr.KindConflict, "coupon_code_taken", nil, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCouponEnv()
			in := tc.setup(t, e)
			coupons, audits := len(e.st().coupons), len(e.st().audits)
			_, err := e.svc.Create(context.Background(), e.tenantID, e.actor, in)
			got := assertAppErr(t, err, tc.kind, tc.code)
			if !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
			if n := e.store.calls["InCouponTx"]; n != tc.txCalls {
				t.Fatalf("transações abertas = %d, esperado %d", n, tc.txCalls)
			}
			if len(e.st().coupons) != coupons || len(e.st().audits) != audits {
				t.Fatalf("nada deve ser gravado: %d cupons, %d auditorias", len(e.st().coupons), len(e.st().audits))
			}
		})
	}
}

func TestCouponServiceCreateSameCodeInAnotherTenant(t *testing.T) {
	e := newCouponEnv()
	first := e.create(t, nil)
	other := uuid.New()
	second, err := e.svc.Create(context.Background(), other, e.actor, validCouponInput())
	if err != nil {
		t.Fatalf("o mesmo código deve ser aceito em outro tenant: %v", err)
	}
	if second.Code != first.Code || second.TenantID != other || second.ID == first.ID {
		t.Fatalf("cupom inesperado: %+v", second)
	}
	if len(e.st().coupons) != 1 || len(e.store.state(other).coupons) != 1 {
		t.Fatal("cada tenant deve guardar somente o próprio cupom")
	}
}

func TestCouponServiceCreateStoreErrors(t *testing.T) {
	for _, op := range []string{"InCouponTx", "CountPlans", "InsertCoupon", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newCouponEnv()
			plans := e.seedPlans(e.tenantID, 2)
			e.store.fail[op] = errBoom
			_, err := e.svc.Create(context.Background(), e.tenantID, e.actor, couponWith(func(in *CouponInput) { in.AppliesToPlanIDs = plans }))
			if !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperado a falha simulada", err)
			}
			if len(e.st().coupons) != 0 || len(e.st().audits) != 0 {
				t.Fatal("a transação deve ser desfeita")
			}
		})
	}
}

func TestCouponServiceList(t *testing.T) {
	e := newCouponEnv()
	older := e.create(t, func(in *CouponInput) { in.Code = "PRIMEIRO" })
	e.clock.Advance(time.Hour)
	newer := e.create(t, func(in *CouponInput) { in.Code = "SEGUNDO" })
	if _, err := e.svc.Create(context.Background(), uuid.New(), e.actor, couponWith(func(in *CouponInput) { in.Code = "ALHEIO" })); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.List(context.Background(), e.tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("lista inesperada: %+v", list)
	}
}

func TestCouponServiceListEmptyAndErrors(t *testing.T) {
	e := newCouponEnv()
	list, err := e.svc.List(context.Background(), e.tenantID)
	if err != nil || len(list) != 0 {
		t.Fatalf("lista = %v, erro = %v", list, err)
	}
	for _, op := range []string{"InCouponTx", "Coupons"} {
		t.Run(op, func(t *testing.T) {
			e := newCouponEnv()
			e.store.fail[op] = errBoom
			if _, err := e.svc.List(context.Background(), e.tenantID); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperado a falha simulada", err)
			}
		})
	}
}

func TestCouponServiceArchive(t *testing.T) {
	e := newCouponEnv()
	plan := e.seedPlans(e.tenantID, 1)[0]
	created := e.create(t, nil)
	e.clock.Advance(24 * time.Hour)
	c, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != created.ID || c.Status != Archived || c.Version != 2 {
		t.Fatalf("cupom inesperado: %+v", c)
	}
	stored := e.st().coupons[c.ID]
	if stored.Status != Archived || stored.Version != c.Version {
		t.Fatalf("cupom gravado = %+v, devolvido versão %d", stored, c.Version)
	}
	if e.store.lockedReads != 1 {
		t.Fatalf("o cupom deve ser lido com bloqueio (%d leituras bloqueadas)", e.store.lockedReads)
	}
	a := e.lastAudit(t)
	if a.Action != "coupon.archived" || a.ResourceType != "coupon" || a.ResourceID != c.ID.String() ||
		a.Actor != e.actor || !a.OccurredAt.Equal(e.clock.Now()) || a.Details["code"] != c.Code {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	assertAppErr(t, c.CheckRedeemable(plan, e.clock.Now()), apperr.KindInvalid, "coupon_inactive")
}

func TestCouponServiceArchiveIsIdempotent(t *testing.T) {
	e := newCouponEnv()
	created := e.create(t, nil)
	first, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	audits := len(e.st().audits)
	second, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, created.ID)
	if err != nil {
		t.Fatalf("arquivar de novo deve ser aceito: %v", err)
	}
	if second.Status != Archived || second.Version != first.Version {
		t.Fatalf("cupom inesperado: %+v", second)
	}
	if n := e.store.calls["ArchiveCoupon"]; n != 1 {
		t.Fatalf("ArchiveCoupon chamado %d vezes, esperado 1", n)
	}
	if len(e.st().audits) != audits {
		t.Fatal("arquivar um cupom já arquivado não deve gerar auditoria")
	}
}

func TestCouponServiceArchiveRejected(t *testing.T) {
	cases := []struct {
		name  string
		id    func(*testing.T, *couponEnv) uuid.UUID
		fail  string
		check func(*testing.T, error)
	}{
		{"cupom inexistente", func(*testing.T, *couponEnv) uuid.UUID { return uuid.New() }, "", func(t *testing.T, err error) {
			assertAppErr(t, err, apperr.KindNotFound, "coupon_not_found")
		}},
		{"cupom de outro tenant", func(t *testing.T, e *couponEnv) uuid.UUID {
			c, err := e.svc.Create(context.Background(), uuid.New(), e.actor, couponWith(func(in *CouponInput) { in.Code = "ALHEIO" }))
			if err != nil {
				t.Fatal(err)
			}
			return c.ID
		}, "", func(t *testing.T, err error) {
			assertAppErr(t, err, apperr.KindNotFound, "coupon_not_found")
		}},
		{"falha ao abrir a transação", nil, "InCouponTx", nil},
		{"falha na leitura", nil, "Coupon", nil},
		{"falha ao arquivar", nil, "ArchiveCoupon", nil},
		{"falha na auditoria", nil, "Audit", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCouponEnv()
			own := e.create(t, nil)
			id := own.ID
			if tc.id != nil {
				id = tc.id(t, e)
			}
			audits := len(e.st().audits)
			if tc.fail != "" {
				e.store.fail[tc.fail] = errBoom
			}
			_, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, id)
			if tc.check != nil {
				tc.check(t, err)
			} else if !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperado a falha simulada", err)
			}
			stored := e.st().coupons[own.ID]
			if stored.Status != Active || stored.Version != 1 || len(e.st().audits) != audits {
				t.Fatalf("o cupom do tenant não deve ser alterado: %+v, %d auditorias", stored, len(e.st().audits))
			}
		})
	}
}
