package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

type webhookSecretDTO struct {
	ID        uuid.UUID  `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type webhookEndpointDTO struct {
	ID          uuid.UUID          `json:"id"`
	URL         string             `json:"url"`
	Description string             `json:"description"`
	EventTypes  []string           `json:"event_types"`
	Status      string             `json:"status"`
	CreatedAt   time.Time          `json:"created_at"`
	Version     int                `json:"version"`
	Secrets     []webhookSecretDTO `json:"secrets"`
}

// endpointView nunca inclui o valor dos segredos: somente identificador e
// validade.
func endpointView(e webhooks.Endpoint) webhookEndpointDTO {
	types := e.EventTypes
	if types == nil {
		types = []string{}
	}
	secrets := make([]webhookSecretDTO, 0, len(e.Secrets))
	for _, s := range e.Secrets {
		secrets = append(secrets, webhookSecretDTO{ID: s.ID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt})
	}
	return webhookEndpointDTO{ID: e.ID, URL: e.URL, Description: e.Description, EventTypes: types, Status: string(e.Status),
		CreatedAt: e.CreatedAt, Version: e.Version, Secrets: secrets}
}

// endpointWithSecret é a única resposta que contém o segredo em claro
// (criação e rotação); a resposta não é armazenada em cache (Cache-Control:
// no-store em todas as rotas).
type endpointWithSecret struct {
	Endpoint webhookEndpointDTO `json:"endpoint"`
	Secret   string             `json:"secret"`
}

type webhookDeliveryDTO struct {
	ID             uuid.UUID  `json:"id"`
	EndpointID     uuid.UUID  `json:"endpoint_id"`
	EventID        uuid.UUID  `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  *time.Time `json:"next_attempt_at"`
	LastStatusCode *int       `json:"last_status_code"`
	LastError      string     `json:"last_error"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

func deliveryView(d webhooks.Delivery) webhookDeliveryDTO {
	return webhookDeliveryDTO{ID: d.ID, EndpointID: d.EndpointID, EventID: d.EventID, EventType: d.EventType, Status: string(d.Status),
		Attempts: d.Attempts, NextAttemptAt: d.NextAttemptAt, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
		DeliveredAt: d.DeliveredAt, CreatedAt: d.CreatedAt}
}

// endpointBody é o corpo WebhookEndpointInput.
type endpointBody struct {
	URL         string   `json:"url"`
	Description *string  `json:"description"`
	EventTypes  []string `json:"event_types"`
}

func (b endpointBody) input(currentDescription string) webhooks.Input {
	desc := currentDescription
	if b.Description != nil {
		desc = *b.Description
	}
	return webhooks.Input{URL: b.URL, Description: desc, EventTypes: b.EventTypes}
}

// decodeOptionalJSON lê um corpo opcional: sem corpo, v fica inalterado.
func decodeOptionalJSON(r *http.Request, v any) error {
	err := decodeJSON(r, v)
	if e, ok := apperr.As(err); ok && e.Code == "empty_body" {
		return nil
	}
	return err
}

func (a *API) listWebhookEventTypes(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, r, http.StatusOK, list(webhooks.EventTypes()))
}

func (a *API) listWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	items, err := a.Webhooks.Endpoints(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]webhookEndpointDTO, 0, len(items))
	for _, e := range items {
		out = append(out, endpointView(e))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	var body endpointBody
	if err := decodeJSON(r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	e, secret, err := a.Webhooks.CreateEndpoint(r.Context(), p.TenantID, p.Actor(), p.UserID(), body.input(""))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, e.Version, endpointWithSecret{Endpoint: endpointView(e), Secret: secret})
}

func (a *API) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
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
	var body struct {
		endpointBody
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	description := ""
	if body.Description == nil {
		// Descrição omitida mantém a atual; a versão do If-Match protege a
		// leitura (alteração concorrente resulta em 412 na gravação).
		cur, err := a.Webhooks.Endpoint(r.Context(), p.TenantID, id)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if cur.Version != version {
			a.writeError(w, r, apperr.PreconditionFailed("version_mismatch", "O endpoint foi alterado por outra pessoa. Recarregue e tente novamente."))
			return
		}
		description = cur.Description
	}
	e, err := a.Webhooks.UpdateEndpoint(r.Context(), p.TenantID, p.Actor(), id, body.input(description), body.Status, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, e.Version, endpointView(e))
}

func (a *API) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if err := a.Webhooks.DeleteEndpoint(r.Context(), p.TenantID, p.Actor(), id); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var body struct {
		PreviousSecretValidHours *int `json:"previous_secret_valid_hours"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	e, secret, err := a.Webhooks.RotateSecret(r.Context(), p.TenantID, p.Actor(), id, body.PreviousSecretValidHours)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, e.Version, endpointWithSecret{Endpoint: endpointView(e), Secret: secret})
}

func (a *API) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	page, err := a.Webhooks.Deliveries(r.Context(), mustPrincipal(r).TenantID, id, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]webhookDeliveryDTO, 0, len(page.Items))
	for _, d := range page.Items {
		out = append(out, deliveryView(d))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, page.NextCursor))
}

func (a *API) retryWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	d, err := a.Webhooks.RetryDelivery(r.Context(), p.TenantID, p.Actor(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, deliveryView(d))
}
