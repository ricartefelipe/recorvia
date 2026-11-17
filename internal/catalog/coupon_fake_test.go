package catalog

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

var (
	_ CouponStore = (*fakeCouponStore)(nil)
	_ CouponTx    = (*fakeCouponTx)(nil)
)

// couponState são os cupons e os planos existentes de um tenant em memória.
type couponState struct {
	coupons map[uuid.UUID]Coupon
	plans   map[uuid.UUID]bool
	audits  []audit.Entry
}

func newCouponState() *couponState {
	return &couponState{coupons: map[uuid.UUID]Coupon{}, plans: map[uuid.UUID]bool{}}
}

func (c *couponState) clone() *couponState {
	return &couponState{coupons: maps.Clone(c.coupons), plans: maps.Clone(c.plans), audits: slices.Clone(c.audits)}
}

// fakeCouponStore implementa CouponStore e CouponTx em memória. Cada
// transação trabalha sobre uma cópia do estado do tenant, confirmada somente
// quando fn retorna nil; em caso de erro, as alterações são descartadas.
type fakeCouponStore struct {
	tenants map[uuid.UUID]*couponState
	fail    map[string]error
	calls   map[string]int
	// countedPlans guarda a última lista recebida por CountPlans.
	countedPlans []uuid.UUID
	// lockedReads conta leituras de cupom com bloqueio da linha.
	lockedReads int
}

func newFakeCouponStore() *fakeCouponStore {
	return &fakeCouponStore{tenants: map[uuid.UUID]*couponState{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (s *fakeCouponStore) state(tenantID uuid.UUID) *couponState {
	st, ok := s.tenants[tenantID]
	if !ok {
		st = newCouponState()
		s.tenants[tenantID] = st
	}
	return st
}

func (s *fakeCouponStore) hit(op string) error {
	s.calls[op]++
	return s.fail[op]
}

func (s *fakeCouponStore) InCouponTx(_ context.Context, tenantID uuid.UUID, fn func(CouponTx) error) error {
	if err := s.hit("InCouponTx"); err != nil {
		return err
	}
	work := s.state(tenantID).clone()
	if err := fn(&fakeCouponTx{s: s, st: work}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

type fakeCouponTx struct {
	s  *fakeCouponStore
	st *couponState
}

// InsertCoupon reproduz a unicidade de (tenant_id, code) do banco.
func (t *fakeCouponTx) InsertCoupon(_ context.Context, c Coupon) error {
	if err := t.s.hit("InsertCoupon"); err != nil {
		return err
	}
	if _, ok := t.st.coupons[c.ID]; ok {
		return fmt.Errorf("cupom %s já gravado", c.ID)
	}
	for _, other := range t.st.coupons {
		if other.Code == c.Code {
			return apperr.Conflict("coupon_code_taken", "Já existe um cupom com este código.")
		}
	}
	c.AppliesToPlanIDs = slices.Clone(c.AppliesToPlanIDs)
	t.st.coupons[c.ID] = c
	return nil
}

func (t *fakeCouponTx) Coupon(_ context.Context, id uuid.UUID, forUpdate bool) (Coupon, error) {
	if err := t.s.hit("Coupon"); err != nil {
		return Coupon{}, err
	}
	if forUpdate {
		t.s.lockedReads++
	}
	c, ok := t.st.coupons[id]
	if !ok {
		return Coupon{}, apperr.NotFound("coupon_not_found", "Cupom não encontrado.")
	}
	return c, nil
}

// Coupons segue a ordenação da consulta: mais recentes primeiro.
func (t *fakeCouponTx) Coupons(context.Context) ([]Coupon, error) {
	if err := t.s.hit("Coupons"); err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Values(t.st.coupons))
	slices.SortFunc(out, func(a, b Coupon) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID.String(), a.ID.String())
	})
	return out, nil
}

// ArchiveCoupon reproduz o UPDATE: arquiva e incrementa a versão.
func (t *fakeCouponTx) ArchiveCoupon(_ context.Context, id uuid.UUID) error {
	if err := t.s.hit("ArchiveCoupon"); err != nil {
		return err
	}
	c, ok := t.st.coupons[id]
	if !ok {
		return apperr.PreconditionFailed("version_mismatch", "O cupom foi alterado por outra pessoa.")
	}
	c.Status = Archived
	c.Version++
	t.st.coupons[id] = c
	return nil
}

// CountPlans conta apenas os planos do próprio tenant.
func (t *fakeCouponTx) CountPlans(_ context.Context, ids []uuid.UUID) (int, error) {
	if err := t.s.hit("CountPlans"); err != nil {
		return 0, err
	}
	t.s.countedPlans = slices.Clone(ids)
	n := 0
	for _, id := range ids {
		if t.st.plans[id] {
			n++
		}
	}
	return n, nil
}

func (t *fakeCouponTx) Audit(_ context.Context, e audit.Entry) error {
	if err := t.s.hit("Audit"); err != nil {
		return err
	}
	t.st.audits = append(t.st.audits, e)
	return nil
}
