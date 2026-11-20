package httpapi

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// --- cupons ---

type couponDTO struct {
	ID               uuid.UUID   `json:"id"`
	Code             string      `json:"code"`
	Name             string      `json:"name"`
	DiscountType     string      `json:"discount_type"`
	PercentOffBP     *int        `json:"percent_off_bp,omitempty"`
	AmountOff        *int64      `json:"amount_off,omitempty"`
	Duration         string      `json:"duration"`
	DurationCycles   *int        `json:"duration_cycles,omitempty"`
	MaxRedemptions   *int        `json:"max_redemptions,omitempty"`
	ValidFrom        *time.Time  `json:"valid_from,omitempty"`
	ValidUntil       *time.Time  `json:"valid_until,omitempty"`
	AppliesToPlanIDs []uuid.UUID `json:"applies_to_plan_ids"`
	RedemptionsCount int         `json:"redemptions_count"`
	Status           string      `json:"status"`
	Currency         string      `json:"currency"`
	CreatedAt        time.Time   `json:"created_at"`
	Version          int         `json:"version"`
}

func amountPtr(m *money.Money) *int64 {
	if m == nil {
		return nil
	}
	v := m.Amount()
	return &v
}

// centsPtr arredonda um valor exato a centavos (half-even) para exibição.
func centsPtr(r *big.Rat) *int64 {
	if r == nil {
		return nil
	}
	v := money.RoundRat(r, money.DefaultRounding).Int64()
	return &v
}

// decimalPtr formata um valor exato em centavos como texto decimal.
func decimalPtr(r *big.Rat) *string {
	if r == nil {
		return nil
	}
	v := money.RatString(r)
	return &v
}

func couponView(c catalog.Coupon) couponDTO {
	plans := c.AppliesToPlanIDs
	if plans == nil {
		plans = []uuid.UUID{}
	}
	return couponDTO{ID: c.ID, Code: c.Code, Name: c.Name, DiscountType: string(c.Type), PercentOffBP: c.PercentOffBP, AmountOff: amountPtr(c.AmountOff),
		Duration: string(c.Duration), DurationCycles: c.DurationCycles, MaxRedemptions: c.MaxRedemptions, ValidFrom: c.ValidFrom, ValidUntil: c.ValidUntil,
		AppliesToPlanIDs: plans, RedemptionsCount: c.RedemptionsCount, Status: string(c.Status), Currency: c.Currency.Code(), CreatedAt: c.CreatedAt,
		Version: c.Version}
}

func (a *API) listCoupons(w http.ResponseWriter, r *http.Request) {
	list, err := a.Coupons.List(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]couponDTO, len(list))
	for i, c := range list {
		out[i] = couponView(c)
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"data": out})
}

func (a *API) createCoupon(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code             string      `json:"code"`
		Name             string      `json:"name"`
		DiscountType     string      `json:"discount_type"`
		PercentOffBP     *int        `json:"percent_off_bp"`
		AmountOff        *int64      `json:"amount_off"`
		Duration         string      `json:"duration"`
		DurationCycles   *int        `json:"duration_cycles"`
		MaxRedemptions   *int        `json:"max_redemptions"`
		ValidFrom        *time.Time  `json:"valid_from"`
		ValidUntil       *time.Time  `json:"valid_until"`
		AppliesToPlanIDs []uuid.UUID `json:"applies_to_plan_ids"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	c, err := a.Coupons.Create(r.Context(), p.TenantID, p.Actor(), catalog.CouponInput{Code: in.Code, Name: in.Name,
		Type: catalog.DiscountType(in.DiscountType), PercentOffBP: in.PercentOffBP, AmountOff: in.AmountOff, Duration: catalog.CouponDuration(in.Duration),
		DurationCycles: in.DurationCycles, MaxRedemptions: in.MaxRedemptions, ValidFrom: in.ValidFrom, ValidUntil: in.ValidUntil,
		AppliesToPlanIDs: in.AppliesToPlanIDs})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, couponView(c))
}

func (a *API) archiveCoupon(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	c, err := a.Coupons.Archive(r.Context(), p.TenantID, p.Actor(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, couponView(c))
}

// --- mudanças de plano e quantidade ---

func (a *API) changeSubscription(w http.ResponseWriter, r *http.Request, in billing.ChangeInput) {
	a.subscriptionAction(w, r, func(p Principal, id uuid.UUID, v int) (subscriptions.Subscription, error) {
		uid := p.UserID()
		res, err := a.Changes.Change(r.Context(), p.TenantID, p.Actor(), uid, id, in, v)
		return res.Subscription, err
	})
}

func (a *API) changeSubscriptionPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlanID   uuid.UUID                  `json:"plan_id"`
		Quantity *int                       `json:"quantity"`
		Timing   subscriptions.ChangeTiming `json:"timing"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if in.PlanID == uuid.Nil {
		a.writeError(w, r, apperr.Invalid("invalid_change", "Alteração de assinatura inválida.", apperr.FieldError{Field: "plan_id", Message: "Informe o plano."}))
		return
	}
	a.changeSubscription(w, r, billing.ChangeInput{PlanID: &in.PlanID, Quantity: in.Quantity, Timing: in.Timing})
}

func (a *API) changeSubscriptionQuantity(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Quantity *int                       `json:"quantity"`
		Timing   subscriptions.ChangeTiming `json:"timing"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if in.Quantity == nil {
		a.writeError(w, r, apperr.Invalid("invalid_change", "Alteração de assinatura inválida.", apperr.FieldError{Field: "quantity", Message: "Informe a quantidade."}))
		return
	}
	a.changeSubscription(w, r, billing.ChangeInput{Quantity: in.Quantity, Timing: in.Timing})
}

// --- reajustes ---

// adjustmentDTO expõe os valores anterior e resultante arredondados a
// centavos (exibição) e exatos em texto (*_decimal): no preço por unidade o
// valor reajustado pode ter fração de centavo.
type adjustmentDTO struct {
	ID                     uuid.UUID  `json:"id"`
	EffectiveDate          string     `json:"effective_date"`
	Kind                   string     `json:"kind"`
	PercentBP              *int       `json:"percent_bp,omitempty"`
	NewAmount              *int64     `json:"new_amount,omitempty"`
	IndexName              string     `json:"index_name,omitempty"`
	Reason                 string     `json:"reason"`
	Status                 string     `json:"status"`
	AppliedAt              *time.Time `json:"applied_at"`
	PreviousAmount         *int64     `json:"previous_amount"`
	ResultingAmount        *int64     `json:"resulting_amount"`
	PreviousAmountDecimal  *string    `json:"previous_amount_decimal"`
	ResultingAmountDecimal *string    `json:"resulting_amount_decimal"`
	CanceledAt             *time.Time `json:"canceled_at"`
	CancelReason           *string    `json:"cancel_reason"`
	CreatedAt              time.Time  `json:"created_at"`
}

func adjustmentView(x subscriptions.PriceAdjustment) adjustmentDTO {
	var reason *string
	if x.CancelReason != "" {
		r := string(x.CancelReason)
		reason = &r
	}
	return adjustmentDTO{ID: x.ID, EffectiveDate: x.EffectiveDate.String(), Kind: string(x.Kind), PercentBP: x.PercentBP, NewAmount: amountPtr(x.NewAmount),
		IndexName: x.IndexName, Reason: x.Reason, Status: string(x.Status), AppliedAt: x.AppliedAt, PreviousAmount: centsPtr(x.PreviousAmount),
		ResultingAmount: centsPtr(x.ResultingAmount), PreviousAmountDecimal: decimalPtr(x.PreviousAmount),
		ResultingAmountDecimal: decimalPtr(x.ResultingAmount), CanceledAt: x.CanceledAt, CancelReason: reason, CreatedAt: x.CreatedAt}
}

func (a *API) listPriceAdjustments(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	list, err := a.Terms.Adjustments(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]adjustmentDTO, len(list))
	for i, x := range list {
		out[i] = adjustmentView(x)
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"data": out})
}

func (a *API) schedulePriceAdjustment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		EffectiveDate string `json:"effective_date"`
		Kind          string `json:"kind"`
		PercentBP     *int   `json:"percent_bp"`
		NewAmount     *int64 `json:"new_amount"`
		IndexName     string `json:"index_name"`
		Reason        string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	date, err := calendar.ParseDate(in.EffectiveDate)
	if err != nil {
		a.writeError(w, r, apperr.Invalid("invalid_adjustment", "Reajuste inválido.",
			apperr.FieldError{Field: "effective_date", Message: "Use o formato AAAA-MM-DD."}))
		return
	}
	p := mustPrincipal(r)
	uid := p.UserID()
	adj, err := a.Terms.ScheduleAdjustment(r.Context(), p.TenantID, p.Actor(), uid, id, subscriptions.AdjustmentInput{EffectiveDate: date,
		Kind: subscriptions.AdjustmentKind(in.Kind), PercentBP: in.PercentBP, NewAmount: in.NewAmount, IndexName: in.IndexName, Reason: in.Reason})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, adjustmentView(adj))
}

func (a *API) cancelPriceAdjustment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	adjID, err := pathUUID(r, "adjustment_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Terms.CancelAdjustment(r.Context(), p.TenantID, p.Actor(), id, adjID); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- descontos ---

type discountDTO struct {
	ID              uuid.UUID `json:"id"`
	CouponID        uuid.UUID `json:"coupon_id"`
	CouponCode      string    `json:"coupon_code"`
	RemainingCycles *int      `json:"remaining_cycles"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

func discountView(d subscriptions.Discount) discountDTO {
	return discountDTO{ID: d.ID, CouponID: d.CouponID, CouponCode: d.CouponCode, RemainingCycles: d.RemainingCycles, Status: string(d.Status),
		CreatedAt: d.CreatedAt}
}

func (a *API) applySubscriptionCoupon(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		CouponCode string `json:"coupon_code"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	uid := p.UserID()
	d, err := a.Terms.ApplyCoupon(r.Context(), p.TenantID, p.Actor(), uid, id, in.CouponCode)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, discountView(d))
}

func (a *API) removeSubscriptionDiscount(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Terms.RemoveDiscount(r.Context(), p.TenantID, p.Actor(), id); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// subscriptionTerms completa a visão da assinatura com mudanças agendadas e
// desconto ativo.
func (a *API) subscriptionTerms(r *http.Request, d *subscriptionDTO) error {
	if a.Terms == nil {
		return nil
	}
	t, err := a.Terms.Terms(r.Context(), mustPrincipal(r).TenantID, d.ID)
	if err != nil {
		return err
	}
	changes := make([]any, 0, len(t.ScheduledChanges))
	for _, c := range t.ScheduledChanges {
		changes = append(changes, map[string]any{"id": c.ID, "kind": c.Kind, "new_plan_id": c.NewPlanID, "new_quantity": c.NewQuantity,
			"effective_at": c.EffectiveAt, "status": c.Status})
	}
	d.ScheduledChanges = changes
	if t.Discount != nil {
		d.Discount = discountView(*t.Discount)
	}
	return nil
}

// --- consumo ---

type usageEventDTO struct {
	ID             uuid.UUID  `json:"id"`
	ExternalID     string     `json:"external_id"`
	SubscriptionID uuid.UUID  `json:"subscription_id"`
	MetricID       uuid.UUID  `json:"metric_id"`
	Quantity       string     `json:"quantity"`
	UniqueKey      *string    `json:"unique_key"`
	OccurredAt     time.Time  `json:"occurred_at"`
	ReceivedAt     time.Time  `json:"received_at"`
	PeriodStart    time.Time  `json:"period_start"`
	Status         string     `json:"status"`
	AggregateID    *uuid.UUID `json:"aggregate_id"`
	InvoiceItemID  *uuid.UUID `json:"invoice_item_id"`
}

func usageEventView(e metering.Event) usageEventDTO {
	d := usageEventDTO{ID: e.ID, ExternalID: e.ExternalID, SubscriptionID: e.SubscriptionID, MetricID: e.MetricID, Quantity: money.RatString(e.Quantity),
		OccurredAt: e.OccurredAt, ReceivedAt: e.ReceivedAt, PeriodStart: e.PeriodStart, Status: string(e.Status), AggregateID: e.AggregateID,
		InvoiceItemID: e.InvoiceItemID}
	if e.UniqueKey != "" {
		k := e.UniqueKey
		d.UniqueKey = &k
	}
	return d
}

// decimalText aceita quantidade como texto ("12.5") ou número JSON (12.5),
// preservando a representação decimal recebida.
type decimalText string

func (d *decimalText) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = decimalText(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*d = decimalText(n.String())
	return nil
}

func (a *API) ingestUsageEvents(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []struct {
			ExternalID     string         `json:"external_id"`
			SubscriptionID uuid.UUID      `json:"subscription_id"`
			MetricCode     string         `json:"metric_code"`
			Unit           string         `json:"unit"`
			Quantity       decimalText    `json:"quantity"`
			UniqueKey      string         `json:"unique_key"`
			OccurredAt     time.Time      `json:"occurred_at"`
			Properties     map[string]any `json:"properties"`
		} `json:"events"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	batch := make([]metering.EventInput, len(in.Events))
	for i, e := range in.Events {
		batch[i] = metering.EventInput{ExternalID: e.ExternalID, SubscriptionID: e.SubscriptionID, MetricCode: e.MetricCode, Unit: e.Unit,
			Quantity: string(e.Quantity), UniqueKey: e.UniqueKey, OccurredAt: e.OccurredAt, Properties: e.Properties}
	}
	results, err := a.Metering.Ingest(r.Context(), mustPrincipal(r).TenantID, batch)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, len(results))
	for i, res := range results {
		m := map[string]any{"external_id": res.ExternalID, "outcome": res.Outcome, "event_id": res.EventID}
		if res.Error != "" {
			m["error"] = res.Error
		}
		out[i] = m
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"results": out})
}

func (a *API) listUsageEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	subID, err := queryUUID(r, "subscription_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	metricID, err := queryUUID(r, "metric_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	page, err := a.Metering.List(r.Context(), mustPrincipal(r).TenantID, metering.ListFilter{SubscriptionID: subID, MetricID: metricID, Limit: limit,
		Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]usageEventDTO, len(page.Items))
	for i, e := range page.Items {
		out[i] = usageEventView(e)
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"data": out, "next_cursor": next})
}

func (a *API) getSubscriptionUsage(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	u, err := a.Metering.CurrentUsage(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	metrics := make([]map[string]any, len(u.Metrics))
	for i, m := range u.Metrics {
		metrics[i] = map[string]any{"metric_id": m.Metric.ID, "code": m.Metric.Code, "name": m.Metric.Name, "unit": m.Metric.Unit,
			"aggregation": m.Metric.Aggregation, "quantity": money.RatString(m.Quantity), "event_count": m.EventCount,
			"estimated_amount": m.EstimatedAmount.Amount()}
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"period_start": u.Period.Start, "period_end": u.Period.End, "metrics": metrics})
}

// --- notas de crédito ---

func (a *API) createCreditNote(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if in.Amount < 1 || in.Amount > money.MaxAmount {
		a.writeError(w, r, apperr.Invalid("invalid_credit_note", "Nota de crédito inválida.",
			apperr.FieldError{Field: "amount", Message: "Informe um valor positivo em centavos."}))
		return
	}
	p := mustPrincipal(r)
	uid := p.UserID()
	if _, err := a.CreditNotes.Issue(r.Context(), p.TenantID, p.Actor(), uid, id, money.New(in.Amount, money.BRL), in.Reason,
		idempotencyKeyFrom(r.Context())); err != nil {
		a.writeError(w, r, err)
		return
	}
	out, version, err := a.invoiceDetails(r, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, version, out)
}

func creditNotesView(list []billing.CreditNote) []map[string]any {
	out := make([]map[string]any, len(list))
	for i, c := range list {
		out[i] = map[string]any{"id": c.ID, "number": c.Number, "amount": c.Amount.Amount(), "reason": c.Reason, "created_at": c.CreatedAt}
	}
	return out
}
