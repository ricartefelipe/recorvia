package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

func (a *API) listCharges(w http.ResponseWriter, r *http.Request) {
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
	status := r.URL.Query().Get("status")
	if status != "" && !knownChargeStatus(status) {
		a.writeError(w, r, invalidField("status", "Situação desconhecida."))
		return
	}
	items, next, err := a.Store.ListCharges(r.Context(), mustPrincipal(r).TenantID, status, invoiceID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, c := range items {
		out = append(out, a.chargeDTO(c))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, next))
}

func knownChargeStatus(s string) bool {
	switch payments.ChargeStatus(s) {
	case payments.ChargePending, payments.ChargeAwaitingPayment, payments.ChargePaid, payments.ChargeFailed, payments.ChargeCanceled,
		payments.ChargePartiallyRefunded, payments.ChargeRefunded, payments.ChargeDisputed, payments.ChargeChargedBack:
		return true
	}
	return false
}

func (a *API) getCharge(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	c, err := a.Payments.Charge(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, a.chargeDTO(c))
}

func (a *API) listPayments(w http.ResponseWriter, r *http.Request) {
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
	items, next, err := a.Store.ListPayments(r.Context(), mustPrincipal(r).TenantID, invoiceID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		out = append(out, paymentDTO(p))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, next))
}

func operationDTO(o payments.Operation) map[string]any {
	return map[string]any{"id": o.ID, "charge_id": o.ChargeID, "refund_id": o.RefundID, "kind": o.Kind, "status": o.Status,
		"external_reference": o.ExternalReference, "attempts": o.Attempts, "next_attempt_at": o.NextAttemptAt, "last_error": o.LastError,
		"created_at": o.CreatedAt, "updated_at": o.UpdatedAt}
}

// listOperations lista as operações externas, das criadas mais recentemente
// para as mais antigas, em páginas por cursor.
func (a *API) listOperations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch payments.OperationStatus(status) {
	case "", payments.OpPending, payments.OpInFlight, payments.OpSucceeded, payments.OpFailed, payments.OpUnknown:
	default:
		a.writeError(w, r, invalidField("status", "Situação desconhecida."))
		return
	}
	limit, cursor, err := pageParams(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	items, next, err := a.Store.ListOperations(r.Context(), mustPrincipal(r).TenantID, status, limit, cursor)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, o := range items {
		out = append(out, operationDTO(o))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, next))
}

// retryOperation reexecuta agora uma operação pendente, com resultado
// desconhecido ou com lease vencido: antecipa a próxima tentativa agendada,
// consulta o provedor pela referência externa antes de criar e respeita o
// limite de tentativas. Operação concluída, esgotada ou em execução responde
// 409 (nada é executado).
func (a *API) retryOperation(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Store.Operation(r.Context(), p.TenantID, id); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.Payments.RetryOperation(r.Context(), p.TenantID, id); err != nil {
		a.writeError(w, r, err)
		return
	}
	op, err := a.Store.Operation(r.Context(), p.TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, operationDTO(op))
}

func accountDTO(acc payments.ProviderAccount) map[string]any {
	return map[string]any{"id": acc.ID, "provider": acc.Provider, "environment": acc.Environment, "status": acc.Status,
		"is_default": acc.IsDefault, "api_key_hint": acc.APIKeyHint, "created_at": acc.CreatedAt, "version": acc.Version}
}

func (a *API) listProviders(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Payments.Accounts(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, accountDTO(acc))
	}
	available := a.Providers
	if available == nil {
		available = []string{}
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"data": out, "available": available})
}

// configureProvider grava a configuração do provedor na versão do If-Match:
// "0" para o provedor ainda não configurado.
func (a *API) configureProvider(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatchOrDefault(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Environment   string `json:"environment"`
		APIKey        string `json:"api_key"`
		WebhookSecret string `json:"webhook_secret"`
		MakeDefault   bool   `json:"make_default"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	res, err := a.Payments.ConfigureAccount(r.Context(), p.TenantID, p.Actor(), payments.AccountInput{Provider: chi.URLParam(r, "provider"),
		Environment: in.Environment, APIKey: in.APIKey, WebhookSecret: in.WebhookSecret, MakeDefault: in.MakeDefault, ExpectedVersion: version})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, res.Account.Version, map[string]any{"account": accountDTO(res.Account), "webhook_endpoint_url": res.EndpointURL})
}

// receiveProviderWebhook autentica e persiste o webhook antes de responder
// 200; o processamento é assíncrono (outbox → consumidor).
func (a *API) receiveProviderWebhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			a.observeWebhook(provider, "too_large")
		}
		a.writeError(w, r, err)
		return
	}
	if !json.Valid(body) {
		a.observeWebhook(provider, "invalid")
		a.writeError(w, r, apperr.Invalid("webhook_invalid", "Corpo do webhook inválido."))
		return
	}
	dup, err := a.Payments.ReceiveWebhook(r.Context(), provider, chi.URLParam(r, "token"), r.Header, body)
	if err != nil {
		outcome := "error"
		switch apperr.KindOf(err) {
		case apperr.KindUnauthenticated:
			outcome = "unauthenticated"
		case apperr.KindNotFound:
			outcome = "unknown_endpoint"
		case apperr.KindInvalid:
			outcome = "invalid"
		}
		a.observeWebhook(provider, outcome)
		a.writeError(w, r, err)
		return
	}
	outcome := "accepted"
	if dup {
		outcome = "duplicate"
	}
	a.observeWebhook(provider, outcome)
	a.log.InfoContext(r.Context(), "webhook recebido", slog.String("provider", provider), slog.Bool("duplicate", dup))
	a.writeJSON(w, r, http.StatusOK, map[string]any{"received": true, "duplicate": dup})
}

func (a *API) observeWebhook(provider, outcome string) {
	if a.Metrics == nil {
		return
	}
	switch provider {
	case "simulator", "asaas":
	default:
		provider = telemetry.OtherProvider // evita cardinalidade a partir da URL
	}
	a.Metrics.WebhooksReceived.WithLabelValues(provider, outcome).Inc()
}
