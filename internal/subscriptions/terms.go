package subscriptions

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// DiscountStatus é a situação de um desconto aplicado.
type DiscountStatus string

const (
	DiscountActive DiscountStatus = "active"
	DiscountEnded  DiscountStatus = "ended"
)

// Discount é um cupom aplicado à assinatura. RemainingCycles nulo indica
// desconto sem limite de ciclos.
type Discount struct {
	ID              uuid.UUID
	SubscriptionID  uuid.UUID
	CouponID        uuid.UUID
	CouponCode      string
	RemainingCycles *int
	Status          DiscountStatus
	CreatedAt       time.Time
}

// TermsTx são as operações das condições comerciais na transação do tenant.
type TermsTx interface {
	Get(ctx context.Context, id uuid.UUID, forUpdate bool) (Subscription, error)
	PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error)
	CouponByCode(ctx context.Context, code string) (catalog.Coupon, error)
	RedeemCoupon(ctx context.Context, couponID uuid.UUID) (bool, error)
	ActiveDiscount(ctx context.Context, subscriptionID uuid.UUID) (*Discount, error)
	InsertDiscount(ctx context.Context, d Discount, appliedBy *uuid.UUID) error
	EndActiveDiscount(ctx context.Context, subscriptionID uuid.UUID, at time.Time) (bool, error)
	InsertAdjustment(ctx context.Context, a PriceAdjustment) error
	// Adjustments devolve uma página dos reajustes da assinatura, pela
	// vigência (limite e cursor opaco).
	Adjustments(ctx context.Context, subscriptionID uuid.UUID, limit int, cursor string) (AdjustmentPage, error)
	Adjustment(ctx context.Context, subscriptionID, id uuid.UUID) (PriceAdjustment, error)
	CancelAdjustment(ctx context.Context, a PriceAdjustment) error
	ScheduledChanges(ctx context.Context, subscriptionID uuid.UUID) ([]ScheduledChange, error)
	InsertHistory(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, ev []HistoryEvent) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// TermsStore abre transações do tenant.
type TermsStore interface {
	InTermsTx(ctx context.Context, tenantID uuid.UUID, fn func(TermsTx) error) error
}

// TermsService implementa cupons aplicados, reajustes programados e a
// consulta de mudanças agendadas.
type TermsService struct {
	store TermsStore
	clock clock.Clock
}

// NewTermsService cria o serviço.
func NewTermsService(store TermsStore, c clock.Clock) *TermsService {
	return &TermsService{store: store, clock: c}
}

// Terms reúne as condições vigentes exibidas com a assinatura.
type Terms struct {
	ScheduledChanges []ScheduledChange
	Discount         *Discount
}

// Terms retorna mudanças agendadas e desconto ativo.
func (s *TermsService) Terms(ctx context.Context, tenantID, subscriptionID uuid.UUID) (Terms, error) {
	var t Terms
	err := s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		var err error
		if t.ScheduledChanges, err = tx.ScheduledChanges(ctx, subscriptionID); err != nil {
			return err
		}
		t.Discount, err = tx.ActiveDiscount(ctx, subscriptionID)
		return err
	})
	return t, err
}

func (s *TermsService) finish(ctx context.Context, tx TermsTx, sub *Subscription, actor audit.Actor, action string, details map[string]any, now time.Time) error {
	if err := tx.InsertHistory(ctx, sub.ID, actor, sub.PopEvents()); err != nil {
		return err
	}
	if err := tx.Emit(ctx, outbox.New(sub.TenantID, outbox.EventSubscriptionUpdated, "subscription", sub.ID,
		map[string]any{"subscription_id": sub.ID, "status": sub.Status, "change": action}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(sub.TenantID, now, actor, action, "subscription", sub.ID.String(), details))
}

// ApplyCoupon aplica um cupom à assinatura. O cupom é bloqueado durante a
// aplicação; o limite de usos é garantido também por constraint do banco.
func (s *TermsService) ApplyCoupon(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, by *uuid.UUID, subscriptionID uuid.UUID, code string) (Discount, error) {
	var d Discount
	err := s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		sub, err := tx.Get(ctx, subscriptionID, true)
		if err != nil {
			return err
		}
		if sub.Status == Canceled {
			return apperr.Conflict("invalid_transition", "A assinatura está cancelada.")
		}
		now := s.clock.Now()
		coupon, err := tx.CouponByCode(ctx, strings.ToUpper(strings.TrimSpace(code)))
		if apperr.IsKind(err, apperr.KindNotFound) {
			return apperr.Invalid("coupon_not_found", "Cupom não encontrado.", apperr.FieldError{Field: "coupon_code", Message: "Código inexistente."})
		}
		if err != nil {
			return err
		}
		if err := coupon.CheckRedeemable(sub.PlanID, now); err != nil {
			return err
		}
		active, err := tx.ActiveDiscount(ctx, sub.ID)
		if err != nil {
			return err
		}
		if active != nil {
			return apperr.Conflict("discount_already_active", "A assinatura já tem um desconto ativo; encerre-o antes de aplicar outro.")
		}
		ok, err := tx.RedeemCoupon(ctx, coupon.ID)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.Conflict("coupon_exhausted", "O cupom atingiu o limite de usos.")
		}
		d = Discount{ID: ids.New(), SubscriptionID: sub.ID, CouponID: coupon.ID, CouponCode: coupon.Code,
			RemainingCycles: coupon.InitialCycles(), Status: DiscountActive, CreatedAt: now}
		if err := tx.InsertDiscount(ctx, d, by); err != nil {
			return err
		}
		details := map[string]any{"coupon_id": coupon.ID, "coupon_code": coupon.Code, "discount_id": d.ID}
		sub.record("discount_applied", nil, now, details)
		return s.finish(ctx, tx, &sub, actor, "subscription.discount_applied", details, now)
	})
	return d, err
}

// RemoveDiscount encerra o desconto ativo. Sem desconto ativo, não faz nada.
func (s *TermsService) RemoveDiscount(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, subscriptionID uuid.UUID) error {
	return s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		sub, err := tx.Get(ctx, subscriptionID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		ended, err := tx.EndActiveDiscount(ctx, sub.ID, now)
		if err != nil || !ended {
			return err
		}
		sub.record("discount_removed", nil, now, nil)
		return s.finish(ctx, tx, &sub, actor, "subscription.discount_removed", nil, now)
	})
}

// ScheduleAdjustment programa um reajuste do componente fixo.
func (s *TermsService) ScheduleAdjustment(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, by *uuid.UUID, subscriptionID uuid.UUID, in AdjustmentInput) (PriceAdjustment, error) {
	var a PriceAdjustment
	err := s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		sub, err := tx.Get(ctx, subscriptionID, true)
		if err != nil {
			return err
		}
		price, err := tx.PriceVersion(ctx, sub.PriceVersionID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if a, err = NewPriceAdjustment(ids.New(), sub, price, in, by, now); err != nil {
			return err
		}
		if err := tx.InsertAdjustment(ctx, a); err != nil {
			return err
		}
		details := map[string]any{"adjustment_id": a.ID, "effective_date": a.EffectiveDate.String(), "kind": a.Kind}
		if a.PercentBP != nil {
			details["percent_bp"] = *a.PercentBP
		}
		if a.NewAmount != nil {
			details["new_amount"] = a.NewAmount.Amount()
		}
		sub.record("price_adjustment_scheduled", nil, now, details)
		return s.finish(ctx, tx, &sub, actor, "subscription.price_adjustment_scheduled", details, now)
	})
	return a, err
}

// AdjustmentPage é uma página dos reajustes da assinatura, da vigência mais
// antiga para a mais recente; NextCursor vazio indica a última página.
type AdjustmentPage struct {
	Items      []PriceAdjustment
	NextCursor string
}

// Adjustments lista os reajustes da assinatura pela vigência, em páginas por
// cursor.
func (s *TermsService) Adjustments(ctx context.Context, tenantID, subscriptionID uuid.UUID, limit int, cursor string) (AdjustmentPage, error) {
	var out AdjustmentPage
	err := s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		if _, err := tx.Get(ctx, subscriptionID, false); err != nil {
			return err
		}
		var err error
		out, err = tx.Adjustments(ctx, subscriptionID, limit, cursor)
		return err
	})
	return out, err
}

// CancelAdjustment cancela um reajuste ainda não aplicado.
func (s *TermsService) CancelAdjustment(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, subscriptionID, adjustmentID uuid.UUID) error {
	return s.store.InTermsTx(ctx, tenantID, func(tx TermsTx) error {
		sub, err := tx.Get(ctx, subscriptionID, true)
		if err != nil {
			return err
		}
		a, err := tx.Adjustment(ctx, subscriptionID, adjustmentID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if err := a.Cancel(AdjustmentCanceledManually, now); err != nil {
			return err
		}
		if err := tx.CancelAdjustment(ctx, a); err != nil {
			return err
		}
		details := map[string]any{"adjustment_id": a.ID, "reason": a.CancelReason}
		sub.record("price_adjustment_canceled", nil, now, details)
		return s.finish(ctx, tx, &sub, actor, "subscription.price_adjustment_canceled", details, now)
	})
}
