package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

// Reembolsos, disputas e conciliação com o provedor (ADR-034 a ADR-038).

// listLimit é o número de itens das listas sem paginação por cursor
// (disputas, execuções e divergências): as mais recentes primeiro.
const listLimit = 100

func refundDTO(r payments.Refund) map[string]any {
	return map[string]any{"id": r.ID, "charge_id": r.ChargeID, "amount": r.Amount.Amount(), "currency": r.Amount.Currency().Code(),
		"status": r.Status, "provider_refund_id": nullable(r.ProviderRefundID), "reason": r.Reason, "created_at": r.CreatedAt}
}

// refundCharge solicita o reembolso total ou parcial de uma cobrança paga.
// A chave de negócio derivada do Idempotency-Key torna o pedido idempotente
// também depois da expiração do registro HTTP; pedidos concorrentes nunca
// somam mais que o saldo elegível (reserva sob bloqueio da cobrança). Com
// credencial de API, a autoria fica na auditoria (requested_by nulo).
func (a *API) refundCharge(w http.ResponseWriter, r *http.Request) {
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
	p := mustPrincipal(r)
	ref, err := a.Payments.RequestRefund(r.Context(), p.TenantID, p.Actor(), p.UserID(), payments.RefundInput{ChargeID: id,
		Amount: money.New(in.Amount, money.BRL), Reason: in.Reason, RequestKey: idempotencyKeyFrom(r.Context())})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, refundDTO(ref))
}

func (a *API) listRefunds(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	items, next, err := a.Store.ListRefunds(r.Context(), mustPrincipal(r).TenantID, nil, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, x := range items {
		out = append(out, refundDTO(x))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, next))
}

func disputeDTO(d pgstore.DisputeWithHistory) map[string]any {
	return map[string]any{"id": d.ID, "charge_id": d.ChargeID, "status": d.Status, "amount": d.Amount.Amount(),
		"currency": d.Amount.Currency().Code(), "opened_at": d.OpenedAt, "closed_at": d.ClosedAt}
}

func (a *API) listDisputes(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.ListDisputes(r.Context(), mustPrincipal(r).TenantID, nil, listLimit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, d := range items {
		out = append(out, disputeDTO(d))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func runDTO(r payments.ReconciliationRun) map[string]any {
	return map[string]any{"id": r.ID, "provider": r.Provider, "status": r.Status, "window_start": r.WindowStart, "window_end": r.WindowEnd,
		"checked_count": r.CheckedCount, "discrepancy_count": r.DiscrepancyCount, "error": r.Error, "started_at": r.StartedAt,
		"finished_at": r.FinishedAt}
}

func (a *API) listReconciliationRuns(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.ListReconciliationRuns(r.Context(), mustPrincipal(r).TenantID, listLimit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, x := range items {
		out = append(out, runDTO(x))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

// runReconciliation executa a conciliação manual do provedor padrão sobre a
// janela padrão e devolve a execução encerrada. A coleta tem prazo próprio,
// independente do prazo da requisição (esgotado, a execução é concluída com
// aviso de execução parcial). Falha do provedor encerra a execução como
// failed (resposta 200 com a causa em error); a repetição com o mesmo
// Idempotency-Key devolve a mesma execução. Com credencial de API, a autoria
// fica na auditoria (requested_by nulo).
func (a *API) runReconciliation(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	run, err := a.Payments.RunReconciliation(r.Context(), p.TenantID, p.Actor(), p.UserID(),
		payments.RunInput{Trigger: payments.TriggerManual, RequestKey: idempotencyKeyFrom(r.Context())})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, runDTO(run))
}

func discrepancyDTO(d payments.Discrepancy) map[string]any {
	local, provider := d.LocalValue, d.ProviderValue
	if local == nil {
		local = map[string]any{}
	}
	if provider == nil {
		provider = map[string]any{}
	}
	return map[string]any{"id": d.ID, "run_id": d.RunID, "kind": d.Kind, "status": d.Status, "charge_id": d.ChargeID,
		"provider_charge_id": nullable(d.ProviderChargeID), "local_value": local, "provider_value": provider,
		"resolution": nullable(string(d.Resolution)), "justification": nullable(d.Justification), "resolved_by": d.ResolvedBy,
		"resolved_at": d.ResolvedAt, "created_at": d.CreatedAt}
}

func knownDiscrepancyKind(kind string) bool {
	for _, k := range payments.DiscrepancyKinds {
		if string(k) == kind {
			return true
		}
	}
	return false
}

func (a *API) listDiscrepancies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status, kind := q.Get("status"), q.Get("kind")
	switch payments.DiscrepancyStatus(status) {
	case "", payments.DiscrepancyOpen, payments.DiscrepancyResolved, payments.DiscrepancyDismissed:
	default:
		a.writeError(w, r, invalidField("status", "Situação desconhecida."))
		return
	}
	if kind != "" && !knownDiscrepancyKind(kind) {
		a.writeError(w, r, invalidField("kind", "Tipo de divergência desconhecido."))
		return
	}
	items, err := a.Store.ListDiscrepancies(r.Context(), mustPrincipal(r).TenantID, status, kind, listLimit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, d := range items {
		out = append(out, discrepancyDTO(d))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

// discrepancyWithHistory acrescenta o histórico de auditoria (detecção,
// reabertura e resolução) à divergência.
func (a *API) discrepancyWithHistory(r *http.Request, tenantID uuid.UUID, d payments.Discrepancy) (map[string]any, error) {
	events, _, err := a.Store.AuditEvents(r.Context(), tenantID, pgstore.AuditFilter{ResourceType: "reconciliation_discrepancy",
		ResourceID: d.ID.String(), Limit: listLimit})
	if err != nil {
		return nil, err
	}
	out := discrepancyDTO(d)
	out["history"] = auditDTOs(events)
	return out, nil
}

func (a *API) getDiscrepancy(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	d, err := a.Payments.Discrepancy(r.Context(), p.TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out, err := a.discrepancyWithHistory(r, p.TenantID, d)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, out)
}

// resolveDiscrepancy resolve a divergência aberta com responsável,
// justificativa e ação; a resolução fica no histórico e, quando houver
// ajuste, no ledger. O responsável é o usuário da sessão (resolved_by) ou a
// credencial de API, registrada na auditoria (resolved_by nulo).
func (a *API) resolveDiscrepancy(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Resolution    string `json:"resolution"`
		Justification string `json:"justification"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	d, err := a.Payments.ResolveDiscrepancy(r.Context(), p.TenantID, p.Actor(), p.UserID(), payments.ResolveInput{ID: id,
		Resolution: payments.Resolution(in.Resolution), Justification: in.Justification})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out, err := a.discrepancyWithHistory(r, p.TenantID, d)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, out)
}
