package httpapi

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// --- utilitários de parâmetros ---

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apperr.NotFound("not_found", "Recurso não encontrado.")
	}
	return id, nil
}

func queryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, apperr.Invalid("invalid_filter", "Filtro inválido.", apperr.FieldError{Field: name, Message: "Identificador inválido."})
	}
	return &id, nil
}

func queryLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 25, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 {
		return 0, apperr.Invalid("invalid_limit", "Limite inválido.", apperr.FieldError{Field: "limit", Message: "Use de 1 a 100."})
	}
	return n, nil
}

func queryDate(r *http.Request, name string, loc *time.Location, endExclusive bool) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	d, err := calendar.ParseDate(v)
	if err != nil {
		return nil, apperr.Invalid("invalid_filter", "Data inválida.", apperr.FieldError{Field: name, Message: "Use AAAA-MM-DD."})
	}
	if endExclusive {
		d = d.AddDays(1)
	}
	t := d.StartIn(loc)
	return &t, nil
}

type page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

func newPage[T any](items []T, next string) page[T] {
	if items == nil {
		items = []T{}
	}
	p := page[T]{Data: items}
	if next != "" {
		p.NextCursor = &next
	}
	return p
}

func list[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"data": items}
}

// --- organização ---

type organizationDTO struct {
	ID               uuid.UUID `json:"id"`
	Slug             string    `json:"slug"`
	Name             string    `json:"name"`
	LegalName        string    `json:"legal_name"`
	DocumentNumber   string    `json:"document_number"`
	Timezone         string    `json:"timezone"`
	Currency         string    `json:"currency"`
	InvoicePrefix    string    `json:"invoice_prefix"`
	PaymentTermsDays int       `json:"payment_terms_days"`
	// Address é nulo enquanto o endereço não for cadastrado.
	Address   *address.Address `json:"address"`
	Status    string           `json:"status"`
	CreatedAt time.Time        `json:"created_at"`
	Version   int              `json:"version"`
}

func orgDTO(t tenancy.Tenant) organizationDTO {
	return organizationDTO{ID: t.ID, Slug: t.Slug, Name: t.Name, LegalName: t.LegalName, DocumentNumber: t.DocumentNumber, Timezone: t.Timezone,
		Currency: t.Currency, InvoicePrefix: t.InvoicePrefix, PaymentTermsDays: t.PaymentTermsDays, Address: t.Address, Status: string(t.Status),
		CreatedAt: t.CreatedAt, Version: t.Version}
}

func (a *API) getOrganization(w http.ResponseWriter, r *http.Request) {
	t, err := a.Tenancy.Organization(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, t.Version, orgDTO(t))
}

func (a *API) updateOrganization(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Name             string `json:"name"`
		LegalName        string `json:"legal_name"`
		DocumentNumber   string `json:"document_number"`
		Timezone         string `json:"timezone"`
		InvoicePrefix    string `json:"invoice_prefix"`
		PaymentTermsDays *int   `json:"payment_terms_days"`
		// Address substitui o endereço da organização; ausente, mantém o atual.
		Address *address.Address `json:"address"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	t, err := a.Tenancy.UpdateOrganization(r.Context(), p.TenantID, p.Actor(), tenancy.TenantInput{Name: in.Name, LegalName: in.LegalName,
		DocumentNumber: in.DocumentNumber, Timezone: in.Timezone, InvoicePrefix: in.InvoicePrefix, PaymentTermsDays: in.PaymentTermsDays,
		Address: in.Address}, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, t.Version, orgDTO(t))
}

func (a *API) listRoles(w http.ResponseWriter, r *http.Request) {
	type roleDTO struct {
		Role        tenancy.Role         `json:"role"`
		Permissions []tenancy.Permission `json:"permissions"`
	}
	out := make([]roleDTO, 0, 5)
	for _, role := range tenancy.Roles() {
		out = append(out, roleDTO{Role: role, Permissions: role.Permissions()})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

type memberDTO struct {
	UserID      uuid.UUID    `json:"user_id"`
	Email       string       `json:"email"`
	DisplayName string       `json:"display_name"`
	Role        tenancy.Role `json:"role"`
	Status      string       `json:"status"`
	CreatedAt   time.Time    `json:"created_at"`
	Version     int          `json:"version"`
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request) {
	members, err := a.Tenancy.Members(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]memberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, memberDTO{UserID: m.UserID, Email: m.Email, DisplayName: m.DisplayName, Role: m.Role, Status: string(m.Status),
			CreatedAt: m.CreatedAt, Version: m.Version})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) access(p Principal) tenancy.Access {
	return tenancy.Access{TenantID: p.TenantID, UserID: p.User.ID, Role: p.Role}
}

func (a *API) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	target, err := pathUUID(r, "user_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Role tenancy.Role `json:"role"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	m, err := a.Tenancy.ChangeRole(r.Context(), p.TenantID, a.access(p), p.UserRef(), target, in.Role)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	members, err := a.Tenancy.Members(r.Context(), p.TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	for _, mm := range members {
		if mm.UserID == m.UserID {
			a.writeJSON(w, r, http.StatusOK, memberDTO{UserID: mm.UserID, Email: mm.Email, DisplayName: mm.DisplayName, Role: mm.Role,
				Status: string(mm.Status), CreatedAt: mm.CreatedAt, Version: mm.Version})
			return
		}
	}
	a.writeError(w, r, apperr.NotFound("member_not_found", "Membro não encontrado."))
}

func (a *API) removeMember(w http.ResponseWriter, r *http.Request) {
	target, err := pathUUID(r, "user_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Tenancy.RemoveMember(r.Context(), p.TenantID, a.access(p), p.UserRef(), target); err != nil {
		a.writeError(w, r, err)
		return
	}
	// As sessões do membro removido com esta organização ativa são
	// encerradas. A remoção já foi confirmada: se o armazenamento de sessões
	// falhar, o acesso continua negado em toda requisição (ResolveAccess).
	if _, err := a.Identity.RevokeTenantSessions(r.Context(), target, p.TenantID); err != nil {
		a.log.WarnContext(r.Context(), "sessões do membro removido não encerradas", slog.String("error", err.Error()))
	}
	w.WriteHeader(http.StatusNoContent)
}

type invitationDTO struct {
	ID        uuid.UUID    `json:"id"`
	Email     string       `json:"email"`
	Role      tenancy.Role `json:"role"`
	State     string       `json:"state"`
	ExpiresAt time.Time    `json:"expires_at"`
	CreatedAt time.Time    `json:"created_at"`
}

func (a *API) invDTO(i tenancy.Invitation) invitationDTO {
	return invitationDTO{ID: i.ID, Email: i.Email, Role: i.Role, State: string(i.State(a.Clock.Now())), ExpiresAt: i.ExpiresAt, CreatedAt: i.CreatedAt}
}

func (a *API) listInvitations(w http.ResponseWriter, r *http.Request) {
	invs, err := a.Tenancy.Invitations(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]invitationDTO, 0, len(invs))
	for _, i := range invs {
		out = append(out, a.invDTO(i))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string       `json:"email"`
		Role  tenancy.Role `json:"role"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	inv, token, err := a.Tenancy.Invite(r.Context(), p.TenantID, a.access(p), p.UserRef(), in.Email, in.Role)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, map[string]any{"invitation": a.invDTO(inv), "accept_url": a.acceptURL(token)})
}

func (a *API) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Tenancy.RevokeInvitation(r.Context(), p.TenantID, p.UserRef(), id); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- plataforma (assinatura do tenant ao Recorvia) ---

type platformPlanDTO struct {
	Code                   string   `json:"code"`
	Name                   string   `json:"name"`
	Description            string   `json:"description"`
	MonthlyPrice           int64    `json:"monthly_price"`
	Currency               string   `json:"currency"`
	MaxUsers               *int     `json:"max_users"`
	MaxActiveSubscriptions *int     `json:"max_active_subscriptions"`
	Features               []string `json:"features"`
	TrialDays              int      `json:"trial_days"`
	IsDefault              bool     `json:"is_default"`
}

func platformPlanDTOFrom(p platformbilling.Plan) platformPlanDTO {
	f := make([]string, len(p.Features))
	for i, x := range p.Features {
		f[i] = string(x)
	}
	return platformPlanDTO{Code: p.Code, Name: p.Name, Description: p.Description, MonthlyPrice: p.MonthlyPrice.Amount(),
		Currency: p.MonthlyPrice.Currency().Code(), MaxUsers: p.MaxUsers, MaxActiveSubscriptions: p.MaxActiveSubscriptions,
		Features: f, TrialDays: p.TrialDays, IsDefault: p.IsDefault}
}

func overviewDTO(o platformbilling.Overview) map[string]any {
	return map[string]any{
		"plan": platformPlanDTOFrom(o.Plan),
		"subscription": map[string]any{"plan_code": o.Subscription.PlanCode, "status": o.Subscription.Status, "trial_ends_at": o.Subscription.TrialEndsAt,
			"current_period_start": o.Subscription.PeriodStart, "current_period_end": o.Subscription.PeriodEnd},
		"usage": map[string]any{"active_users": o.Usage.ActiveUsers, "pending_invitations": o.Usage.PendingInvitations,
			"active_subscriptions": o.Usage.ActiveSubscriptions},
	}
}

func (a *API) platformOverview(w http.ResponseWriter, r *http.Request) {
	o, err := a.Platform.Overview(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, overviewDTO(o))
}

func (a *API) platformPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := a.Platform.Plans(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]platformPlanDTO, 0, len(plans))
	for _, p := range plans {
		out = append(out, platformPlanDTOFrom(p))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) changePlatformPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PlanCode string `json:"plan_code"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Platform.ChangePlan(r.Context(), p.TenantID, p.Actor(), in.PlanCode); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.platformOverview(w, r)
}

func (a *API) platformAddons(w http.ResponseWriter, r *http.Request) {
	addons, err := a.Platform.Addons(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(addons))
	for _, x := range addons {
		out = append(out, map[string]any{"code": x.Code, "name": x.Name, "description": x.Description, "price": x.Price.Amount(),
			"currency": x.Price.Currency().Code(), "kind": x.Kind})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func addonOrderDTO(o platformbilling.AddonOrder) map[string]any {
	return map[string]any{"id": o.ID, "addon_code": o.AddonCode, "status": o.Status, "notes": o.Notes, "price": o.Price.Amount(),
		"currency": o.Price.Currency().Code(), "created_at": o.CreatedAt, "scheduled_for": o.ScheduledFor, "completed_at": o.CompletedAt,
		"canceled_at": o.CanceledAt, "cancel_reason": nullable(o.CancelReason), "billed": o.StatementID != nil, "version": o.Version}
}

func (a *API) listAddonOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := a.Store.Platform().AddonOrders(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		out = append(out, addonOrderDTO(o))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createAddonOrder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AddonCode string `json:"addon_code"`
		Notes     string `json:"notes"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	o, err := a.Platform.RequestAddon(r.Context(), p.TenantID, p.Actor(), p.User.ID, in.AddonCode, in.Notes)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, addonOrderDTO(o))
}

func (a *API) listPlatformStatements(w http.ResponseWriter, r *http.Request) {
	sts, err := a.Store.Platform().Statements(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(sts))
	for _, s := range sts {
		out = append(out, map[string]any{"id": s.ID, "period_start": s.PeriodStart, "period_end": s.PeriodEnd, "plan_code": s.PlanCode,
			"subscription_amount": s.SubscriptionAmount.Amount(), "addons_amount": s.AddonsAmount.Amount(), "total": s.Total.Amount(),
			"currency": s.Total.Currency().Code(), "status": s.Status})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}
