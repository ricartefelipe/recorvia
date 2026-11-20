package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/reporting"
)

func (a *API) ledgerAccounts(w http.ResponseWriter, r *http.Request) {
	bal, err := a.Store.Ledger().Balances(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(bal))
	for _, b := range bal {
		out = append(out, map[string]any{"code": b.Code, "name": b.Name, "kind": b.Kind, "currency": b.Balance.Currency().Code(),
			"debits": b.Debits.Amount(), "credits": b.Credits.Amount(), "balance": b.Balance.Amount()})
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

var ledgerRefTypes = map[string]bool{"": true, "invoice": true, "payment": true, "refund": true, "dispute": true, "credit_note": true,
	"customer_credit": true, "settlement": true, "reconciliation_discrepancy": true}

func (a *API) ledgerTransactions(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	refType := r.URL.Query().Get("reference_type")
	if !ledgerRefTypes[refType] {
		a.writeError(w, r, invalidField("reference_type", "Tipo de referência não permitido."))
		return
	}
	refID, err := queryUUID(r, "reference_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	txs, next, err := a.Store.Ledger().Transactions(r.Context(), mustPrincipal(r).TenantID, refType, refID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(txs))
	for _, t := range txs {
		entries := make([]map[string]any, 0, len(t.Entries))
		for _, e := range t.Entries {
			entries = append(entries, map[string]any{"account": e.Account, "direction": e.Direction, "amount": e.Amount.Amount(),
				"currency": e.Amount.Currency().Code()})
		}
		out = append(out, map[string]any{"id": t.ID, "kind": t.Kind, "description": t.Description, "occurred_at": t.OccurredAt,
			"reference_type": t.ReferenceType, "reference_id": t.ReferenceID, "reverses_transaction_id": t.ReversesID, "entries": entries})
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, next))
}

func auditDTOs(events []pgstore.AuditEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{"id": e.ID, "occurred_at": e.OccurredAt, "actor_type": e.ActorType, "actor_id": e.ActorID,
			"actor_label": e.ActorLabel, "action": e.Action, "resource_type": e.ResourceType, "resource_id": e.ResourceID,
			"request_id": e.RequestID, "details": e.Details})
	}
	return out
}

func (a *API) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	loc, err := a.Billing.Location(r.Context(), p.TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	from, err := queryDate(r, "from", loc, false)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	to, err := queryDate(r, "to", loc, true)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	for _, k := range []string{"resource_type", "resource_id", "action", "actor_id"} {
		if len(q.Get(k)) > 100 {
			a.writeError(w, r, invalidField(k, "Valor muito longo."))
			return
		}
	}
	events, next, err := a.Store.AuditEvents(r.Context(), p.TenantID, pgstore.AuditFilter{ResourceType: q.Get("resource_type"),
		ResourceID: q.Get("resource_id"), Action: q.Get("action"), ActorID: q.Get("actor_id"), From: from, To: to, Limit: limit, Cursor: q.Get("cursor")})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, newPage(auditDTOs(events), next))
}

func deadLetterDTO(d pgstore.DeadLetter) map[string]any {
	return map[string]any{"id": d.ID, "consumer": d.Consumer, "message_id": d.MessageID, "event_type": nullable(d.EventType), "error": d.Error,
		"attempts": d.Attempts, "status": d.Status, "failed_at": d.FailedAt, "resolved_at": d.ResolvedAt, "resolution_note": nullable(d.ResolutionNote)}
}

func (a *API) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", "open", "replayed", "discarded":
	default:
		a.writeError(w, r, invalidField("status", "Situação desconhecida."))
		return
	}
	items, err := a.Store.DeadLetters(r.Context(), mustPrincipal(r).TenantID, status)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, d := range items {
		out = append(out, deadLetterDTO(d))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

// resolveDeadLetter reprocessa (reentrega direcionada à fila do consumidor
// que falhou, preservando o id original) ou descarta, sempre com
// justificativa e auditoria.
func (a *API) resolveDeadLetter(replay bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathUUID(r, "id")
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		var in struct {
			Reason string `json:"reason"`
		}
		if err := decodeJSON(r, &in); err != nil {
			a.writeError(w, r, err)
			return
		}
		if l := len([]rune(in.Reason)); l < 3 || l > 500 {
			a.writeError(w, r, invalidField("reason", "Informe de 3 a 500 caracteres."))
			return
		}
		p := mustPrincipal(r)
		res := pgstore.DeadLetterResolution{Status: pgstore.DeadLetterDiscarded, Actor: p.Actor(), By: p.UserID(), Note: in.Reason}
		var fn func(context.Context, pgstore.ReplayEmitter, pgstore.DeadLetter) error
		if replay {
			res.Status = pgstore.DeadLetterReplayed
			fn = func(_ context.Context, emit pgstore.ReplayEmitter, d pgstore.DeadLetter) error {
				if d.EventType == "" || len(d.Payload) == 0 {
					return apperr.Conflict("dead_letter_not_replayable", "A mensagem não possui conteúdo para reprocessamento.")
				}
				return emit(rabbit.QueueName(d.Consumer), d.MessageID, d.EventType, uuid.Nil, d.Payload)
			}
		}
		d, err := a.Store.ResolveDeadLetter(r.Context(), p.TenantID, id, res, fn)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		a.writeJSON(w, r, http.StatusOK, deadLetterDTO(d))
	}
}

func (a *API) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := a.Reporting.Dashboard(r.Context(), mustPrincipal(r).TenantID, r.URL.Query().Get("month"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, dashboardDTO(d))
}

func dashboardDTO(d reporting.Dashboard) map[string]any {
	return map[string]any{"month": d.Month, "currency": d.Currency, "mrr": d.MRR, "arr": d.ARR, "billed": d.Billed, "billed_recurring": d.BilledRecurring,
		"billed_usage": d.BilledUsage, "received": d.Received, "open_receivables": d.OpenReceivables, "overdue_receivables": d.OverdueReceivables,
		"delinquency_rate_bp": d.DelinquencyRateBP, "churn_rate_bp": d.ChurnRateBP, "active_subscriptions": d.ActiveSubscriptions,
		"new_subscriptions": d.NewSubscriptions, "canceled_subscriptions": d.CanceledSubscriptions, "refunds": d.Refunds, "chargebacks": d.Chargebacks,
		"open_discrepancies": d.OpenDiscrepancies, "unknown_operations": d.UnknownOperations, "definitions": d.Definitions}
}

// structToMap converte um DTO em mapa para compor respostas.
func structToMap(v any) map[string]any {
	b, err := jsonMarshal(v)
	if err != nil {
		return map[string]any{}
	}
	m := map[string]any{}
	if err := jsonUnmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}
