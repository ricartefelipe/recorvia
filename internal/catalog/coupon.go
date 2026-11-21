package catalog

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// DiscountType é o tipo de desconto do cupom.
type DiscountType string

const (
	// DiscountPercent aplica percentual (pontos-base) ao subtotal da fatura.
	DiscountPercent DiscountType = "percent"
	// DiscountAmount abate um valor fixo por fatura, limitado ao subtotal.
	DiscountAmount DiscountType = "amount"
)

// CouponDuration define por quantos ciclos o desconto vale.
type CouponDuration string

const (
	DurationOnce      CouponDuration = "once"
	DurationRepeating CouponDuration = "repeating"
	DurationForever   CouponDuration = "forever"
)

// MaxCouponPlans limita a lista de planos elegíveis.
const MaxCouponPlans = 100

// MaxCouponRedemptions é o maior limite de usos aceito. Fica abaixo do
// máximo da coluna integer (2.147.483.647), de modo que o valor validado é
// sempre persistido sem truncamento.
const MaxCouponRedemptions = 1_000_000_000

// Coupon é um cupom de desconto recorrente do tenant.
type Coupon struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	Code             string
	Name             string
	Type             DiscountType
	PercentOffBP     *int
	AmountOff        *money.Money
	Currency         money.Currency
	Duration         CouponDuration
	DurationCycles   *int
	MaxRedemptions   *int
	RedemptionsCount int
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	AppliesToPlanIDs []uuid.UUID
	Status           Status
	CreatedAt        time.Time
	Version          int
}

// CouponInput são os dados de criação de um cupom.
type CouponInput struct {
	Code             string
	Name             string
	Type             DiscountType
	PercentOffBP     *int
	AmountOff        *int64
	Duration         CouponDuration
	DurationCycles   *int
	MaxRedemptions   *int
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	AppliesToPlanIDs []uuid.UUID
}

var couponCodeRe = regexp.MustCompile(`^[A-Z0-9_-]{3,40}$`)

// NewCoupon valida e cria o cupom. O código é normalizado em maiúsculas.
func NewCoupon(id, tenantID uuid.UUID, cur money.Currency, in CouponInput, now time.Time) (Coupon, error) {
	var f []apperr.FieldError
	add := func(field, msg string) { f = append(f, apperr.FieldError{Field: field, Message: msg}) }
	c := Coupon{ID: id, TenantID: tenantID, Code: strings.ToUpper(strings.TrimSpace(in.Code)), Type: in.Type, Currency: cur,
		Duration: in.Duration, MaxRedemptions: in.MaxRedemptions, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil,
		Status: Active, CreatedAt: now, Version: 1}
	if !couponCodeRe.MatchString(c.Code) {
		add("code", "Use de 3 a 40 letras maiúsculas, dígitos, hífen ou sublinhado.")
	}
	c.Name = textField(&f, "name", in.Name, 2, 200, "Informe o nome (2 a 200 caracteres).")
	switch in.Type {
	case DiscountPercent:
		if in.PercentOffBP == nil || *in.PercentOffBP < 1 || *in.PercentOffBP > 10000 {
			add("percent_off_bp", "Informe o percentual entre 1 e 10.000 pontos-base.")
		} else {
			bp := *in.PercentOffBP
			c.PercentOffBP = &bp
		}
		if in.AmountOff != nil {
			add("amount_off", "Cupom percentual não usa valor fixo.")
		}
	case DiscountAmount:
		if in.AmountOff == nil || *in.AmountOff < 1 || *in.AmountOff > money.MaxAmount {
			add("amount_off", "Informe o valor do desconto em centavos.")
		} else {
			m := money.New(*in.AmountOff, cur)
			c.AmountOff = &m
		}
		if in.PercentOffBP != nil {
			add("percent_off_bp", "Cupom de valor fixo não usa percentual.")
		}
	default:
		add("discount_type", "Use percent ou amount.")
	}
	switch in.Duration {
	case DurationRepeating:
		if in.DurationCycles == nil || *in.DurationCycles < 1 || *in.DurationCycles > 120 {
			add("duration_cycles", "Informe de 1 a 120 ciclos.")
		} else {
			n := *in.DurationCycles
			c.DurationCycles = &n
		}
	case DurationOnce, DurationForever:
		if in.DurationCycles != nil {
			add("duration_cycles", "Somente cupons repeating usam quantidade de ciclos.")
		}
	default:
		add("duration", "Use once, repeating ou forever.")
	}
	if in.MaxRedemptions != nil && (*in.MaxRedemptions < 1 || *in.MaxRedemptions > MaxCouponRedemptions) {
		add("max_redemptions", "Informe um limite de usos de 1 a 1.000.000.000.")
	}
	if in.ValidFrom != nil && in.ValidUntil != nil && !in.ValidFrom.Before(*in.ValidUntil) {
		add("valid_until", "O fim da validade deve ser posterior ao início.")
	}
	if in.ValidUntil != nil && !in.ValidUntil.After(now) {
		add("valid_until", "O fim da validade deve estar no futuro.")
	}
	if len(in.AppliesToPlanIDs) > MaxCouponPlans {
		add("applies_to_plan_ids", "Informe no máximo 100 planos.")
	}
	plans := slices.Clone(in.AppliesToPlanIDs)
	slices.SortFunc(plans, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	c.AppliesToPlanIDs = slices.Compact(plans)
	if len(f) > 0 {
		return Coupon{}, apperr.Invalid("invalid_coupon", "Cupom inválido.", f...)
	}
	if c.AppliesToPlanIDs == nil {
		c.AppliesToPlanIDs = []uuid.UUID{}
	}
	return c, nil
}

// CheckRedeemable verifica se o cupom pode ser aplicado a uma assinatura do
// plano informado agora.
func (c Coupon) CheckRedeemable(planID uuid.UUID, now time.Time) error {
	switch {
	case c.Status != Active:
		return apperr.Invalid("coupon_inactive", "O cupom está arquivado.")
	case c.ValidFrom != nil && now.Before(*c.ValidFrom):
		return apperr.Invalid("coupon_not_yet_valid", "O cupom ainda não está válido.")
	case c.ValidUntil != nil && !now.Before(*c.ValidUntil):
		return apperr.Invalid("coupon_expired", "O cupom expirou.")
	case c.MaxRedemptions != nil && c.RedemptionsCount >= *c.MaxRedemptions:
		return apperr.Conflict("coupon_exhausted", "O cupom atingiu o limite de usos.")
	case len(c.AppliesToPlanIDs) > 0 && !slices.Contains(c.AppliesToPlanIDs, planID):
		return apperr.Invalid("coupon_not_applicable", "O cupom não se aplica ao plano desta assinatura.")
	}
	return nil
}

// AppliesTo informa se o cupom vale para o plano.
func (c Coupon) AppliesTo(planID uuid.UUID) bool {
	return len(c.AppliesToPlanIDs) == 0 || slices.Contains(c.AppliesToPlanIDs, planID)
}

// InitialCycles é o número de faturas com desconto (nil = sem limite).
func (c Coupon) InitialCycles() *int {
	switch c.Duration {
	case DurationOnce:
		one := 1
		return &one
	case DurationRepeating:
		n := *c.DurationCycles
		return &n
	}
	return nil
}

// CouponTx são as operações de cupons na transação do tenant.
type CouponTx interface {
	InsertCoupon(ctx context.Context, c Coupon) error
	Coupon(ctx context.Context, id uuid.UUID, forUpdate bool) (Coupon, error)
	Coupons(ctx context.Context) ([]Coupon, error)
	// ArchiveCoupon arquiva o cupom na versão informada (412
	// version_mismatch se a linha tiver outra versão).
	ArchiveCoupon(ctx context.Context, id uuid.UUID, version int) error
	CountPlans(ctx context.Context, ids []uuid.UUID) (int, error)
	Audit(ctx context.Context, e audit.Entry) error
}

// CouponStore abre transações do tenant para cupons.
type CouponStore interface {
	InCouponTx(ctx context.Context, tenantID uuid.UUID, fn func(CouponTx) error) error
}

// CouponService implementa os casos de uso de cupons.
type CouponService struct {
	store CouponStore
	clock clock.Clock
}

// NewCouponService cria o serviço.
func NewCouponService(store CouponStore, c clock.Clock) *CouponService {
	return &CouponService{store: store, clock: c}
}

// Create cadastra um cupom. Planos elegíveis devem pertencer ao tenant.
func (s *CouponService) Create(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in CouponInput) (Coupon, error) {
	now := s.clock.Now()
	c, err := NewCoupon(ids.New(), tenantID, money.BRL, in, now)
	if err != nil {
		return Coupon{}, err
	}
	err = s.store.InCouponTx(ctx, tenantID, func(tx CouponTx) error {
		if len(c.AppliesToPlanIDs) > 0 {
			n, err := tx.CountPlans(ctx, c.AppliesToPlanIDs)
			if err != nil {
				return err
			}
			if n != len(c.AppliesToPlanIDs) {
				return apperr.Invalid("invalid_coupon", "Cupom inválido.",
					apperr.FieldError{Field: "applies_to_plan_ids", Message: "Há planos inexistentes na lista."})
			}
		}
		if err := tx.InsertCoupon(ctx, c); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "coupon.created", "coupon", c.ID.String(),
			map[string]any{"code": c.Code, "discount_type": c.Type, "duration": c.Duration}))
	})
	return c, err
}

// List lista os cupons do tenant.
func (s *CouponService) List(ctx context.Context, tenantID uuid.UUID) ([]Coupon, error) {
	var out []Coupon
	err := s.store.InCouponTx(ctx, tenantID, func(tx CouponTx) error {
		var err error
		out, err = tx.Coupons(ctx)
		return err
	})
	return out, err
}

// errCouponVersion recusa o arquivamento com versão diferente da informada
// no If-Match.
var errCouponVersion = apperr.PreconditionFailed("version_mismatch", "O cupom foi alterado por outra pessoa. Recarregue e tente novamente.")

// Archive arquiva o cupom na versão esperada (If-Match): novas aplicações são
// recusadas; descontos já aplicados seguem até o fim da duração contratada.
// Arquivar um cupom já arquivado, na versão informada, não tem efeito.
func (s *CouponService) Archive(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, expectedVersion int) (Coupon, error) {
	var c Coupon
	err := s.store.InCouponTx(ctx, tenantID, func(tx CouponTx) error {
		var err error
		if c, err = tx.Coupon(ctx, id, true); err != nil {
			return err
		}
		if c.Status == Archived {
			if c.Version != expectedVersion {
				return errCouponVersion
			}
			return nil
		}
		if err := tx.ArchiveCoupon(ctx, id, expectedVersion); err != nil {
			return err
		}
		c.Status = Archived
		c.Version++
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "coupon.archived", "coupon", id.String(), map[string]any{"code": c.Code}))
	})
	return c, err
}
