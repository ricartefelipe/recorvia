package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

type subscriptionDTO struct {
	ID                 uuid.UUID  `json:"id"`
	CustomerID         uuid.UUID  `json:"customer_id"`
	CustomerName       string     `json:"customer_name,omitempty"`
	PlanID             uuid.UUID  `json:"plan_id"`
	PlanName           string     `json:"plan_name,omitempty"`
	PriceVersionID     uuid.UUID  `json:"price_version_id"`
	Status             string     `json:"status"`
	Quantity           int        `json:"quantity"`
	BillingInterval    string     `json:"billing_interval"`
	Timezone           string     `json:"timezone"`
	AnchorDate         string     `json:"anchor_date"`
	TrialEndsAt        *time.Time `json:"trial_ends_at"`
	StartedAt          time.Time  `json:"started_at"`
	CurrentPeriodStart time.Time  `json:"current_period_start"`
	CurrentPeriodEnd   time.Time  `json:"current_period_end"`
	NextBillingAt      *time.Time `json:"next_billing_at"`
	CancelAtPeriodEnd  bool       `json:"cancel_at_period_end"`
	CanceledAt         *time.Time `json:"canceled_at"`
	CancelReason       string     `json:"cancel_reason"`
	EndedAt            *time.Time `json:"ended_at"`
	SuspendedAt        *time.Time `json:"suspended_at"`
	PaymentTermsDays   *int       `json:"payment_terms_days"`
	ScheduledChanges   []any      `json:"scheduled_changes"`
	Discount           any        `json:"discount"`
	CreatedAt          time.Time  `json:"created_at"`
	Version            int        `json:"version"`
}

func subDTO(s subscriptions.Subscription) subscriptionDTO {
	return subscriptionDTO{ID: s.ID, CustomerID: s.CustomerID, PlanID: s.PlanID, PriceVersionID: s.PriceVersionID, Status: string(s.Status),
		Quantity: s.Quantity, BillingInterval: string(s.Interval), Timezone: s.Timezone, AnchorDate: s.Anchor.String(), TrialEndsAt: s.TrialEndsAt,
		StartedAt: s.StartedAt, CurrentPeriodStart: s.CurrentPeriod.Start, CurrentPeriodEnd: s.CurrentPeriod.End, NextBillingAt: s.NextBillingAt,
		CancelAtPeriodEnd: s.CancelAtPeriodEnd, CanceledAt: s.CanceledAt, CancelReason: s.CancelReason, EndedAt: s.EndedAt,
		SuspendedAt: s.SuspendedAt, PaymentTermsDays: s.PaymentTermsDays, ScheduledChanges: []any{}, CreatedAt: s.CreatedAt, Version: s.Version}
}

func (a *API) subscriptionView(r *http.Request, s subscriptions.Subscription) subscriptionDTO {
	d := subDTO(s)
	if c, p, err := a.Store.Subscriptions().Names(r.Context(), s.TenantID, s.ID); err == nil {
		d.CustomerName, d.PlanName = c, p
	}
	return d
}

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	customerID, err := queryUUID(r, "customer_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	pg, err := a.Subscriptions.List(r.Context(), mustPrincipal(r).TenantID, subscriptions.ListFilter{Status: subscriptions.Status(r.URL.Query().Get("status")),
		CustomerID: customerID, Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]subscriptionDTO, 0, len(pg.Items))
	for _, s := range pg.Items {
		out = append(out, a.subscriptionView(r, s))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, pg.NextCursor))
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID       uuid.UUID `json:"customer_id"`
		PlanID           uuid.UUID `json:"plan_id"`
		Quantity         int       `json:"quantity"`
		StartDate        string    `json:"start_date"`
		TrialDays        *int      `json:"trial_days"`
		PaymentTermsDays *int      `json:"payment_terms_days"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	var start *calendar.Date
	if in.StartDate != "" {
		d, err := calendar.ParseDate(in.StartDate)
		if err != nil {
			a.writeError(w, r, invalidField("start_date", "Use AAAA-MM-DD."))
			return
		}
		start = &d
	}
	p := mustPrincipal(r)
	s, err := a.Subscriptions.Create(r.Context(), p.TenantID, p.Actor(), subscriptions.CreateInput{CustomerID: in.CustomerID, PlanID: in.PlanID,
		Quantity: in.Quantity, StartDate: start, TrialDays: in.TrialDays, PaymentTermsDays: in.PaymentTermsDays,
		RequestKey: idempotencyKeyFrom(r.Context())})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, s.Version, a.subscriptionView(r, s))
}

func (a *API) getSubscription(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	s, err := a.Subscriptions.Get(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	d := a.subscriptionView(r, s)
	if err := a.subscriptionTerms(r, &d); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, s.Version, d)
}

func (a *API) subscriptionHistory(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	h, err := a.Subscriptions.History(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(h))
	for _, e := range h {
		out = append(out, map[string]any{"id": e.ID, "event": e.Event, "from_status": e.FromStatus, "to_status": e.ToStatus,
			"occurred_at": e.OccurredAt, "actor_type": e.ActorType, "actor_id": e.ActorID, "details": e.Details})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

// subscriptionAction executa uma transição com If-Match e devolve a assinatura.
func (a *API) subscriptionAction(w http.ResponseWriter, r *http.Request, fn func(p Principal, id uuid.UUID, version int) (subscriptions.Subscription, error)) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	s, err := fn(mustPrincipal(r), id, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	d := a.subscriptionView(r, s)
	if err := a.subscriptionTerms(r, &d); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, s.Version, d)
}

func (a *API) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode   subscriptions.CancelMode `json:"mode"`
		Reason string                   `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.subscriptionAction(w, r, func(p Principal, id uuid.UUID, v int) (subscriptions.Subscription, error) {
		return a.Subscriptions.Cancel(r.Context(), p.TenantID, p.Actor(), id, in.Mode, in.Reason, v)
	})
}

func (a *API) resumeRenewal(w http.ResponseWriter, r *http.Request) {
	a.subscriptionAction(w, r, func(p Principal, id uuid.UUID, v int) (subscriptions.Subscription, error) {
		return a.Subscriptions.ResumeRenewal(r.Context(), p.TenantID, p.Actor(), id, v)
	})
}

func (a *API) suspendSubscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &in); err != nil {
			a.writeError(w, r, err)
			return
		}
	}
	a.subscriptionAction(w, r, func(p Principal, id uuid.UUID, v int) (subscriptions.Subscription, error) {
		return a.Subscriptions.Suspend(r.Context(), p.TenantID, p.Actor(), id, in.Reason, v)
	})
}

func (a *API) reactivateSubscription(w http.ResponseWriter, r *http.Request) {
	a.subscriptionAction(w, r, func(p Principal, id uuid.UUID, v int) (subscriptions.Subscription, error) {
		return a.Subscriptions.Reactivate(r.Context(), p.TenantID, p.Actor(), id, v)
	})
}
