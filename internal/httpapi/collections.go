package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/notifications"
)

// nullableInt distingue, num corpo JSON, o campo ausente (Set falso), o
// valor nulo (Set verdadeiro e Value nulo) e um número inteiro.
type nullableInt struct {
	Set   bool
	Value *int
}

// UnmarshalJSON implementa json.Unmarshaler (chamado também para null).
func (n *nullableInt) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		n.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// --- políticas de cobrança ---

type collectionPolicyDTO struct {
	ID                  uuid.UUID `json:"id"`
	Name                string    `json:"name"`
	IsDefault           bool      `json:"is_default"`
	ReminderOffsetsDays []int     `json:"reminder_offsets_days"`
	SendWindowStartHour int       `json:"send_window_start_hour"`
	SendWindowEndHour   int       `json:"send_window_end_hour"`
	MaxMessagesPerDay   int       `json:"max_messages_per_day"`
	SuspendAfterDays    *int      `json:"suspend_after_days"`
	ReactivateOnPayment bool      `json:"reactivate_on_payment"`
	CreatedAt           time.Time `json:"created_at"`
	Version             int       `json:"version"`
}

func policyView(p collections.Policy) collectionPolicyDTO {
	offsets := p.OffsetsDays
	if offsets == nil {
		offsets = []int{}
	}
	return collectionPolicyDTO{ID: p.ID, Name: p.Name, IsDefault: p.IsDefault, ReminderOffsetsDays: offsets,
		SendWindowStartHour: p.Window.StartHour, SendWindowEndHour: p.Window.EndHour, MaxMessagesPerDay: p.MaxMessagesPerDay,
		SuspendAfterDays: p.SuspendAfterDays, ReactivateOnPayment: p.ReactivateOnPayment, CreatedAt: p.CreatedAt, Version: p.Version}
}

// policyBody é o corpo CollectionPolicyInput. Os campos obrigatórios do
// contrato são ponteiros para distinguir ausência de zero.
type policyBody struct {
	Name                *string     `json:"name"`
	IsDefault           *bool       `json:"is_default"`
	ReminderOffsetsDays []int       `json:"reminder_offsets_days"`
	SendWindowStartHour *int        `json:"send_window_start_hour"`
	SendWindowEndHour   *int        `json:"send_window_end_hour"`
	MaxMessagesPerDay   *int        `json:"max_messages_per_day"`
	SuspendAfterDays    nullableInt `json:"suspend_after_days"`
	ReactivateOnPayment *bool       `json:"reactivate_on_payment"`
}

// partial informa se algum campo opcional foi omitido (na alteração, ele
// mantém o valor atual da política).
func (b policyBody) partial() bool {
	return b.IsDefault == nil || !b.SuspendAfterDays.Set || b.ReactivateOnPayment == nil
}

// input valida a presença dos campos obrigatórios e monta os dados do caso de
// uso. Campos opcionais omitidos usam os valores de base (alteração) ou o
// valor neutro (criação: não padrão, sem suspensão, sem reativação
// automática).
func (b policyBody) input(base *collections.PolicyInput) (collections.PolicyInput, error) {
	var missing []apperr.FieldError
	required := func(field string, present bool) {
		if !present {
			missing = append(missing, apperr.FieldError{Field: field, Message: "Campo obrigatório."})
		}
	}
	required("name", b.Name != nil)
	required("reminder_offsets_days", b.ReminderOffsetsDays != nil)
	required("send_window_start_hour", b.SendWindowStartHour != nil)
	required("send_window_end_hour", b.SendWindowEndHour != nil)
	required("max_messages_per_day", b.MaxMessagesPerDay != nil)
	if len(missing) > 0 {
		return collections.PolicyInput{}, apperr.Invalid("invalid_collection_policy", "Política de cobrança inválida.", missing...)
	}
	var in collections.PolicyInput
	if base != nil {
		in = *base
	}
	in.Name, in.OffsetsDays = *b.Name, b.ReminderOffsetsDays
	in.WindowStartHour, in.WindowEndHour, in.MaxMessagesPerDay = *b.SendWindowStartHour, *b.SendWindowEndHour, *b.MaxMessagesPerDay
	if b.IsDefault != nil {
		in.IsDefault = *b.IsDefault
	}
	if b.SuspendAfterDays.Set {
		in.SuspendAfterDays = b.SuspendAfterDays.Value
	}
	if b.ReactivateOnPayment != nil {
		in.ReactivateOnPayment = *b.ReactivateOnPayment
	}
	return in, nil
}

func (a *API) listCollectionPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := a.Collections.Policies(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]collectionPolicyDTO, 0, len(items))
	for _, p := range items {
		out = append(out, policyView(p))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createCollectionPolicy(w http.ResponseWriter, r *http.Request) {
	var body policyBody
	if err := decodeJSON(r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	in, err := body.input(nil)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	uid := p.User.ID
	pol, err := a.Collections.CreatePolicy(r.Context(), p.TenantID, p.Actor(), &uid, in)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, pol.Version, policyView(pol))
}

func (a *API) updateCollectionPolicy(w http.ResponseWriter, r *http.Request) {
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
	var body policyBody
	if err := decodeJSON(r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	var base *collections.PolicyInput
	if body.partial() {
		// A leitura da base é protegida pela mesma versão do If-Match: se a
		// política mudar depois dela, a gravação falha com 412.
		cur, err := a.Collections.Policy(r.Context(), p.TenantID, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if cur.Version != version {
			a.writeError(w, r, apperr.PreconditionFailed("version_mismatch", "A política foi alterada por outra pessoa. Recarregue e tente novamente."))
			return
		}
		in := cur.Input()
		base = &in
	}
	in, err := body.input(base)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	pol, err := a.Collections.UpdatePolicy(r.Context(), p.TenantID, p.Actor(), id, in, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, pol.Version, policyView(pol))
}

// --- templates ---

type notificationTemplateDTO struct {
	Key       string     `json:"key"`
	Channel   string     `json:"channel"`
	Subject   string     `json:"subject"`
	Body      string     `json:"body"`
	IsDefault bool       `json:"is_default"`
	Variables []string   `json:"variables"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func templateView(t notifications.Template) notificationTemplateDTO {
	vars := t.Key.Variables()
	if vars == nil {
		vars = []string{}
	}
	return notificationTemplateDTO{Key: string(t.Key), Channel: notifications.ChannelEmail, Subject: t.Subject, Body: t.Body,
		IsDefault: t.IsDefault, Variables: vars, UpdatedAt: t.UpdatedAt}
}

func (a *API) listNotificationTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := a.Notifications.Templates(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]notificationTemplateDTO, 0, len(items))
	for _, t := range items {
		out = append(out, templateView(t))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) updateNotificationTemplate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	uid := p.User.ID
	t, err := a.Notifications.UpdateTemplate(r.Context(), p.TenantID, p.Actor(), &uid, chi.URLParam(r, "key"), in.Subject, in.Body)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, templateView(t))
}

// --- histórico de notificações ---

type notificationDTO struct {
	ID          uuid.UUID  `json:"id"`
	BusinessKey string     `json:"business_key"`
	CustomerID  *uuid.UUID `json:"customer_id"`
	InvoiceID   *uuid.UUID `json:"invoice_id"`
	TemplateKey string     `json:"template_key"`
	Channel     string     `json:"channel"`
	Recipient   string     `json:"recipient"`
	Subject     string     `json:"subject"`
	Status      string     `json:"status"`
	SendAfter   time.Time  `json:"send_after"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	SentAt      *time.Time `json:"sent_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

func notificationView(n notifications.Notification) notificationDTO {
	channel := n.Channel
	if channel == "" {
		channel = notifications.ChannelEmail
	}
	return notificationDTO{ID: n.ID, BusinessKey: n.BusinessKey, CustomerID: n.CustomerID, InvoiceID: n.InvoiceID,
		TemplateKey: string(n.TemplateKey), Channel: channel, Recipient: strings.Join(n.Recipients, ", "), Subject: n.Subject,
		Status: string(n.Status), SendAfter: n.SendAfter, Attempts: n.Attempts, LastError: n.LastError, SentAt: n.SentAt, CreatedAt: n.CreatedAt}
}

func (a *API) listNotifications(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	invoiceID, err := queryUUID(r, "invoice_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	page, err := a.Notifications.List(r.Context(), mustPrincipal(r).TenantID, notifications.ListFilter{Status: notifications.Status(q.Get("status")),
		InvoiceID: invoiceID, Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]notificationDTO, 0, len(page.Items))
	for _, n := range page.Items {
		out = append(out, notificationView(n))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, page.NextCursor))
}
