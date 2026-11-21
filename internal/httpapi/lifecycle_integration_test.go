//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// lcEvents conta os eventos do histórico da assinatura por nome.
func lcEvents(c *client, subPath string) (map[string]int, []map[string]any) {
	c.a.t.Helper()
	hist, _ := c.expect(http.StatusOK, http.MethodGet, subPath+"/history", nil)
	list := objects(c.a.t, hist["data"])
	out := map[string]int{}
	for _, e := range list {
		ev, _ := e["event"].(string)
		out[ev]++
	}
	return out, list
}

// lcEvent devolve a última entrada do histórico com o evento informado.
func lcEvent(t *testing.T, list []map[string]any, event string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, e := range list {
		if e["event"] == event {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("evento %s ausente do histórico: %v", event, list)
	}
	return found
}

// Cancelamento ao fim do período pela API: agenda sem encerrar, retomada da
// renovação desfaz o agendamento, e o motor encerra a assinatura no fim do
// período sem emitir novo ciclo.
func TestLifecycleCancelAtPeriodEndAndResumeRenewalOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "ciclo-fim")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Fim do Período", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	path := "/api/v1/subscriptions/" + sub.ID.String()
	issuedInvoice(c, sub.ID)

	body := map[string]any{"mode": "period_end", "reason": "Cliente pediu o encerramento"}
	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path+"/cancel", body)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path+"/cancel", body, "If-Match", `"99"`)
	etag := subscriptionETag(c, sub.ID)
	scheduled, h := c.expect(http.StatusOK, http.MethodPost, path+"/cancel", body, "If-Match", etag)
	if scheduled["status"] != "active" || scheduled["cancel_at_period_end"] != true || scheduled["canceled_at"] == nil ||
		scheduled["cancel_reason"] != "Cliente pediu o encerramento" || scheduled["ended_at"] != nil || h.Get("ETag") == etag {
		t.Fatalf("cancelamento agendado: %v (ETag %s)", scheduled, h.Get("ETag"))
	}

	resumed, _ := c.expect(http.StatusOK, http.MethodPost, path+"/resume-renewal", nil, "If-Match", h.Get("ETag"))
	if resumed["status"] != "active" || resumed["cancel_at_period_end"] != false || resumed["canceled_at"] != nil || resumed["cancel_reason"] != "" {
		t.Fatalf("renovação retomada: %v", resumed)
	}
	c.expectProblem(http.StatusConflict, "no_scheduled_cancel", http.MethodPost, path+"/resume-renewal", nil, "If-Match", subscriptionETag(c, sub.ID))

	// Agenda de novo e deixa o período terminar.
	c.expect(http.StatusOK, http.MethodPost, path+"/cancel", body, "If-Match", subscriptionETag(c, sub.ID))
	a.env.Clock.Set(sub.CurrentPeriod.End.Add(10 * time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	ended, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	if ended["status"] != "canceled" || ended["ended_at"] == nil || !instant(t, ended["ended_at"]).Equal(sub.CurrentPeriod.End) ||
		ended["next_billing_at"] != nil {
		t.Fatalf("encerramento no fim do período: %v", ended)
	}
	if list := subscriptionInvoices(c, sub.ID); len(list) != 1 {
		t.Fatalf("assinatura encerrada no fim do período não pode emitir novo ciclo: %v", list)
	}
	events, hist := lcEvents(c, path)
	if events["cancel_scheduled"] != 2 || events["cancel_unscheduled"] != 1 || events["ended_at_period_end"] != 1 {
		t.Fatalf("histórico do cancelamento agendado: %v", events)
	}
	if e := lcEvent(t, hist, "ended_at_period_end"); e["from_status"] != "active" || e["to_status"] != "canceled" {
		t.Fatalf("transição do encerramento: %v", e)
	}
	c.expectProblem(http.StatusConflict, "no_scheduled_cancel", http.MethodPost, path+"/resume-renewal", nil, "If-Match", subscriptionETag(c, sub.ID))
}

// Cancelamento imediato pela API: encerra agora, sem novo ciclo depois.
func TestLifecycleCancelImmediatelyOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "ciclo-imediato")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Imediato", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	path := "/api/v1/subscriptions/" + sub.ID.String()
	issuedInvoice(c, sub.ID)

	c.expectProblem(http.StatusUnprocessableEntity, "invalid_cancel_mode", http.MethodPost, path+"/cancel", map[string]any{"mode": "amanha"},
		"If-Match", subscriptionETag(c, sub.ID))
	now := a.env.Clock.Now()
	out, _ := c.expect(http.StatusOK, http.MethodPost, path+"/cancel", map[string]any{"mode": "immediate", "reason": "Contrato rescindido"},
		"If-Match", subscriptionETag(c, sub.ID))
	if out["status"] != "canceled" || out["cancel_at_period_end"] != false || out["cancel_reason"] != "Contrato rescindido" ||
		!instant(t, out["ended_at"]).Equal(now) || !instant(t, out["canceled_at"]).Equal(now) || out["next_billing_at"] != nil {
		t.Fatalf("cancelamento imediato: %v", out)
	}
	if n := countOutbox(t, a.env, tn.ID, outbox.EventSubscriptionCanceled, sub.ID); n != 1 {
		t.Fatalf("eventos subscription.canceled = %d, esperado 1", n)
	}
	_, hist := lcEvents(c, path)
	e := lcEvent(t, hist, "canceled")
	if details, _ := e["details"].(map[string]any); e["from_status"] != "active" || e["to_status"] != "canceled" || details["mode"] != "immediate" {
		t.Fatalf("histórico do cancelamento imediato: %v", e)
	}

	a.env.Clock.Set(sub.CurrentPeriod.End.Add(10 * time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	if list := subscriptionInvoices(c, sub.ID); len(list) != 1 {
		t.Fatalf("assinatura cancelada não pode emitir novo ciclo: %v", list)
	}
}

// Suspensão e reativação pela API: suspensa não fatura; reativada, o
// faturamento retoma no período corrente.
func TestLifecycleSuspendAndReactivateOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "ciclo-suspensao")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Suspensão", flatPrice(10000))
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	path := "/api/v1/subscriptions/" + sub.ID.String()
	issuedInvoice(c, sub.ID)

	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path+"/suspend", nil)
	// O corpo é opcional: sem motivo, a suspensão é manual.
	suspended, _ := c.expect(http.StatusOK, http.MethodPost, path+"/suspend", nil, "If-Match", subscriptionETag(c, sub.ID))
	if suspended["status"] != "suspended" || suspended["suspended_at"] == nil || suspended["next_billing_at"] != nil {
		t.Fatalf("suspensão: %v", suspended)
	}
	if n := countOutbox(t, a.env, tn.ID, outbox.EventSubscriptionSuspended, sub.ID); n != 1 {
		t.Fatalf("eventos subscription.suspended = %d, esperado 1", n)
	}

	// Período seguinte começa com a assinatura suspensa: nada é faturado.
	resumeAt := sub.CurrentPeriod.End.Add(10 * time.Hour)
	a.env.Clock.Set(resumeAt)
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	if list := subscriptionInvoices(c, sub.ID); len(list) != 1 {
		t.Fatalf("assinatura suspensa não pode ser faturada: %v", list)
	}

	reactivated, _ := c.expect(http.StatusOK, http.MethodPost, path+"/reactivate", nil, "If-Match", subscriptionETag(c, sub.ID))
	if reactivated["status"] != "active" || reactivated["suspended_at"] != nil || reactivated["next_billing_at"] == nil ||
		!instant(t, reactivated["current_period_start"]).Equal(sub.CurrentPeriod.End) {
		t.Fatalf("reativação: %v", reactivated)
	}
	runBilling(c)
	list := subscriptionInvoices(c, sub.ID)
	if len(list) != 2 {
		t.Fatalf("reativada, a assinatura deveria faturar o período corrente: %v", list)
	}
	var resumed map[string]any
	for _, inv := range list {
		if instant(t, inv["billing_period_start"]).Equal(sub.CurrentPeriod.End) {
			resumed = inv
		}
	}
	if resumed == nil || resumed["status"] != "open" || resumed["total"] != float64(10000) {
		t.Fatalf("fatura do período da reativação: %v", list)
	}
	events, hist := lcEvents(c, path)
	if events["suspended"] != 1 || events["reactivated"] != 1 {
		t.Fatalf("histórico de suspensão e reativação: %v", events)
	}
	s := lcEvent(t, hist, "suspended")
	if d, _ := s["details"].(map[string]any); s["from_status"] != "active" || s["to_status"] != "suspended" || d["reason_code"] != "manual" {
		t.Fatalf("histórico da suspensão: %v", s)
	}
	r := lcEvent(t, hist, "reactivated")
	if d, _ := r["details"].(map[string]any); r["from_status"] != "suspended" || r["to_status"] != "active" || d["skipped_periods"] != float64(0) {
		t.Fatalf("histórico da reativação: %v", r)
	}
}

// Assinatura com avaliação criada pela API fica em trialing, sem fatura, até
// o fim da avaliação; o motor então emite a primeira fatura e a ativa.
func TestLifecycleTrialUntilFirstInvoice(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "ciclo-trial")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano com Avaliação", flatPrice(10000))
	cust := a.env.NewCustomer(t, tn)
	today := tenantToday(t, a.env)

	created, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions", map[string]any{"customer_id": cust.ID, "plan_id": plan.ID,
		"trial_days": 14})
	loc, err := time.LoadLocation(tenantTimezone)
	if err != nil {
		t.Fatal(err)
	}
	trialEnd := today.AddDays(14).StartIn(loc)
	if created["status"] != "trialing" || !instant(t, created["trial_ends_at"]).Equal(trialEnd) ||
		!instant(t, created["next_billing_at"]).Equal(trialEnd) || !instant(t, created["current_period_end"]).Equal(trialEnd) ||
		created["anchor_date"] != today.AddDays(14).String() {
		t.Fatalf("assinatura em avaliação: %v (fim esperado %s)", created, trialEnd)
	}
	subID := uuid.MustParse(created["id"].(string))
	path := "/api/v1/subscriptions/" + subID.String()

	// Durante a avaliação o motor não fatura.
	runBilling(c)
	a.env.Clock.Set(trialEnd.Add(-time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	if list := subscriptionInvoices(c, subID); len(list) != 0 {
		t.Fatalf("assinatura em avaliação não pode ser faturada: %v", list)
	}
	if got, _ := c.expect(http.StatusOK, http.MethodGet, path, nil); got["status"] != "trialing" {
		t.Fatalf("antes do fim da avaliação: %v", got)
	}

	a.env.Clock.Set(trialEnd.Add(time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	list := subscriptionInvoices(c, subID)
	if len(list) != 1 || list[0]["status"] != "open" || list[0]["total"] != float64(10000) ||
		!instant(t, list[0]["billing_period_start"]).Equal(trialEnd) {
		t.Fatalf("primeira fatura ao fim da avaliação: %v", list)
	}
	got, _ := c.expect(http.StatusOK, http.MethodGet, path, nil)
	if got["status"] != "active" || !instant(t, got["current_period_start"]).Equal(trialEnd) {
		t.Fatalf("assinatura após a avaliação: %v", got)
	}
	_, hist := lcEvents(c, path)
	if e := lcEvent(t, hist, "trial_ended"); e["from_status"] != "trialing" || e["to_status"] != "active" {
		t.Fatalf("histórico do fim da avaliação: %v", e)
	}
}

// Cancelamento (void) da fatura pela API: exige If-Match atual e motivo,
// recusa fatura com nota de crédito ou pagamento e registra o histórico.
func TestLifecycleInvoiceVoidOverHTTP(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := a.env.NewTenant(t, "ciclo-void")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Void", flatPrice(10000))

	inv := issuedInvoice(c, newSubscription(t, a.env, tn, plan.ID, 1, nil).ID)
	invID := uuid.MustParse(inv["id"].(string))
	path := "/api/v1/invoices/" + invID.String()
	reason := map[string]any{"reason": "Emitida por engano"}
	_, h := c.expect(http.StatusOK, http.MethodGet, path, nil)
	etag := h.Get("ETag")

	c.expectProblem(http.StatusPreconditionRequired, "if_match_required", http.MethodPost, path+"/void", reason)
	c.expectProblem(http.StatusPreconditionFailed, "version_mismatch", http.MethodPost, path+"/void", reason, "If-Match", `"99"`)
	c.expectProblem(http.StatusUnprocessableEntity, "invalid_reason", http.MethodPost, path+"/void", map[string]any{"reason": "ok"}, "If-Match", etag)
	if got, gh := c.expect(http.StatusOK, http.MethodGet, path, nil); got["status"] != "open" || gh.Get("ETag") != etag {
		t.Fatalf("tentativas recusadas não podem alterar a fatura: %v", got)
	}

	voided, vh := c.expect(http.StatusOK, http.MethodPost, path+"/void", reason, "If-Match", etag)
	if voided["status"] != "void" || voided["void_reason"] != "Emitida por engano" || voided["voided_at"] == nil || vh.Get("ETag") == etag {
		t.Fatalf("fatura cancelada: %v (ETag %s)", voided, vh.Get("ETag"))
	}
	hist := objects(t, voided["history"])
	last := hist[len(hist)-1]
	if last["from_status"] != "open" || last["to_status"] != "void" || last["reason"] != "Emitida por engano" || last["actor_type"] != "user" ||
		last["actor_id"] != tn.Owner.ID.String() {
		t.Fatalf("histórico do cancelamento da fatura: %v", hist)
	}
	if n := countOutbox(t, a.env, tn.ID, outbox.EventInvoiceVoided, invID); n != 1 {
		t.Fatalf("eventos invoice.voided = %d, esperado 1", n)
	}
	c.expectProblem(http.StatusConflict, "invoice_not_voidable", http.MethodPost, path+"/void", reason, "If-Match", vh.Get("ETag"))

	// Fatura em aberto com nota de crédito parcial.
	credited := issuedInvoice(c, newSubscription(t, a.env, tn, plan.ID, 1, nil).ID)
	creditedPath := "/api/v1/invoices/" + credited["id"].(string)
	note, nh := c.expect(http.StatusCreated, http.MethodPost, creditedPath+"/credit-notes", map[string]any{"amount": 1000, "reason": "Desconto negociado"},
		"Idempotency-Key", "nota-"+uuid.NewString())
	if note["status"] != "open" || note["amount_credited"] != float64(1000) {
		t.Fatalf("nota de crédito parcial: %v", note)
	}
	c.expectProblem(http.StatusConflict, "invoice_has_payments", http.MethodPost, creditedPath+"/void", reason, "If-Match", nh.Get("ETag"))

	// Fatura parcialmente paga.
	paid := issuedInvoice(c, newSubscription(t, a.env, tn, plan.ID, 1, nil).ID)
	paidPath := "/api/v1/invoices/" + paid["id"].(string)
	c.expect(http.StatusCreated, http.MethodPost, paidPath+"/payments", map[string]any{"amount": 2500, "method": "pix", "note": "Pagamento parcial"},
		"Idempotency-Key", "pagamento-"+uuid.NewString())
	got, ph := c.expect(http.StatusOK, http.MethodGet, paidPath, nil)
	if got["status"] != "partially_paid" {
		t.Fatalf("pagamento parcial: %v", got)
	}
	c.expectProblem(http.StatusConflict, "invoice_not_voidable", http.MethodPost, paidPath+"/void", reason, "If-Match", ph.Get("ETag"))
	for _, p := range []string{creditedPath, paidPath} {
		if after, _ := c.expect(http.StatusOK, http.MethodGet, p, nil); after["status"] == "void" || after["voided_at"] != nil {
			t.Fatalf("fatura com pagamento ou crédito foi cancelada: %v", after)
		}
	}
}
