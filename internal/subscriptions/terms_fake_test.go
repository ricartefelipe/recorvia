package subscriptions

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// fakeTermsStore implementa TermsStore e TermsTx em memória. Cada InTermsTx é
// atômico: se a função falhar, as gravações feitas na transação são
// descartadas. Assinaturas de outro tenant não são visíveis, como no banco.
type fakeTermsStore struct {
	subs        map[uuid.UUID]Subscription
	prices      map[uuid.UUID]catalog.PriceVersion
	coupons     map[string]catalog.Coupon
	changes     map[uuid.UUID][]ScheduledChange
	discounts   []Discount
	appliedBy   map[uuid.UUID]*uuid.UUID
	adjustments []PriceAdjustment
	histories   []historyWrite
	audits      []audit.Entry
	events      []outbox.Event

	errs          map[string]error
	tenant        uuid.UUID
	tenants       []uuid.UUID
	forUpdate     []bool
	couponLookups []string
	// redeemRace simula outra transação consumindo o último uso do cupom
	// entre a leitura e o resgate.
	redeemRace bool
}

func newFakeTermsStore() *fakeTermsStore {
	return &fakeTermsStore{subs: map[uuid.UUID]Subscription{}, prices: map[uuid.UUID]catalog.PriceVersion{},
		coupons: map[string]catalog.Coupon{}, changes: map[uuid.UUID][]ScheduledChange{}, appliedBy: map[uuid.UUID]*uuid.UUID{},
		errs: map[string]error{}}
}

func (s *fakeTermsStore) InTermsTx(_ context.Context, tenantID uuid.UUID, fn func(TermsTx) error) error {
	s.tenant = tenantID
	s.tenants = append(s.tenants, tenantID)
	coupons, appliedBy := maps.Clone(s.coupons), maps.Clone(s.appliedBy)
	discounts, adjustments := slices.Clone(s.discounts), slices.Clone(s.adjustments)
	nh, na, ne := len(s.histories), len(s.audits), len(s.events)
	if err := fn(s); err != nil {
		s.coupons, s.appliedBy, s.discounts, s.adjustments = coupons, appliedBy, discounts, adjustments
		s.histories, s.audits, s.events = s.histories[:nh], s.audits[:na], s.events[:ne]
		return err
	}
	return nil
}

func (s *fakeTermsStore) fail(method string) error { return s.errs[method] }

func (s *fakeTermsStore) Get(_ context.Context, id uuid.UUID, forUpdate bool) (Subscription, error) {
	s.forUpdate = append(s.forUpdate, forUpdate)
	if err := s.fail("Get"); err != nil {
		return Subscription{}, err
	}
	sub, ok := s.subs[id]
	if !ok || sub.TenantID != s.tenant {
		return Subscription{}, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
	}
	return sub, nil
}

func (s *fakeTermsStore) PriceVersion(_ context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	if err := s.fail("PriceVersion"); err != nil {
		return catalog.PriceVersion{}, err
	}
	p, ok := s.prices[id]
	if !ok {
		return catalog.PriceVersion{}, apperr.NotFound("price_not_found", "Preço não encontrado.")
	}
	return p, nil
}

func (s *fakeTermsStore) CouponByCode(_ context.Context, code string) (catalog.Coupon, error) {
	s.couponLookups = append(s.couponLookups, code)
	if err := s.fail("CouponByCode"); err != nil {
		return catalog.Coupon{}, err
	}
	c, ok := s.coupons[code]
	if !ok {
		return catalog.Coupon{}, apperr.NotFound("coupon_not_found", "Cupom não encontrado.")
	}
	return c, nil
}

func (s *fakeTermsStore) RedeemCoupon(_ context.Context, couponID uuid.UUID) (bool, error) {
	if err := s.fail("RedeemCoupon"); err != nil {
		return false, err
	}
	for code, c := range s.coupons {
		if c.ID != couponID {
			continue
		}
		if s.redeemRace || (c.MaxRedemptions != nil && c.RedemptionsCount >= *c.MaxRedemptions) {
			return false, nil
		}
		c.RedemptionsCount++
		s.coupons[code] = c
		return true, nil
	}
	return false, apperr.NotFound("coupon_not_found", "Cupom não encontrado.")
}

func (s *fakeTermsStore) ActiveDiscount(_ context.Context, subscriptionID uuid.UUID) (*Discount, error) {
	if err := s.fail("ActiveDiscount"); err != nil {
		return nil, err
	}
	for _, d := range s.discounts {
		if d.SubscriptionID == subscriptionID && d.Status == DiscountActive {
			return &d, nil
		}
	}
	return nil, nil
}

func (s *fakeTermsStore) InsertDiscount(_ context.Context, d Discount, appliedBy *uuid.UUID) error {
	if err := s.fail("InsertDiscount"); err != nil {
		return err
	}
	s.discounts = append(s.discounts, d)
	s.appliedBy[d.ID] = appliedBy
	return nil
}

func (s *fakeTermsStore) EndActiveDiscount(_ context.Context, subscriptionID uuid.UUID, _ time.Time) (bool, error) {
	if err := s.fail("EndActiveDiscount"); err != nil {
		return false, err
	}
	for i, d := range s.discounts {
		if d.SubscriptionID == subscriptionID && d.Status == DiscountActive {
			s.discounts[i].Status = DiscountEnded
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeTermsStore) InsertAdjustment(_ context.Context, a PriceAdjustment) error {
	if err := s.fail("InsertAdjustment"); err != nil {
		return err
	}
	s.adjustments = append(s.adjustments, a)
	return nil
}

func (s *fakeTermsStore) Adjustments(_ context.Context, subscriptionID uuid.UUID) ([]PriceAdjustment, error) {
	if err := s.fail("Adjustments"); err != nil {
		return nil, err
	}
	var out []PriceAdjustment
	for _, a := range s.adjustments {
		if a.SubscriptionID == subscriptionID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeTermsStore) Adjustment(_ context.Context, subscriptionID, id uuid.UUID) (PriceAdjustment, error) {
	if err := s.fail("Adjustment"); err != nil {
		return PriceAdjustment{}, err
	}
	for _, a := range s.adjustments {
		if a.ID == id && a.SubscriptionID == subscriptionID {
			return a, nil
		}
	}
	return PriceAdjustment{}, apperr.NotFound("adjustment_not_found", "Reajuste não encontrado.")
}

func (s *fakeTermsStore) CancelAdjustment(_ context.Context, a PriceAdjustment) error {
	if err := s.fail("CancelAdjustment"); err != nil {
		return err
	}
	for i, cur := range s.adjustments {
		if cur.ID == a.ID && cur.SubscriptionID == a.SubscriptionID {
			s.adjustments[i] = a
			return nil
		}
	}
	return apperr.NotFound("adjustment_not_found", "Reajuste não encontrado.")
}

func (s *fakeTermsStore) ScheduledChanges(_ context.Context, subscriptionID uuid.UUID) ([]ScheduledChange, error) {
	return s.changes[subscriptionID], s.fail("ScheduledChanges")
}

func (s *fakeTermsStore) InsertHistory(_ context.Context, id uuid.UUID, actor audit.Actor, ev []HistoryEvent) error {
	if err := s.fail("InsertHistory"); err != nil {
		return err
	}
	s.histories = append(s.histories, historyWrite{id, actor, ev})
	return nil
}

func (s *fakeTermsStore) Audit(_ context.Context, e audit.Entry) error {
	if err := s.fail("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *fakeTermsStore) Emit(_ context.Context, e outbox.Event) error {
	if err := s.fail("Emit"); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}
