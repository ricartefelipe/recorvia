package paysim

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Handler retorna o roteador HTTP do simulador.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// API autenticada por chave (Bearer).
	mux.HandleFunc("PUT /v1/webhook", s.auth(s.putWebhook))
	mux.HandleFunc("POST /v1/customers", s.auth(s.postCustomer))
	mux.HandleFunc("POST /v1/charges", s.auth(s.postCharge))
	mux.HandleFunc("GET /v1/charges", s.auth(s.listCharges))
	mux.HandleFunc("GET /v1/charges/{id}", s.auth(s.getCharge))
	mux.HandleFunc("DELETE /v1/charges/{id}", s.auth(s.deleteCharge))
	mux.HandleFunc("POST /v1/charges/{id}/refunds", s.auth(s.postRefund))
	// Checkout hospedado e controles de simulação (apenas ambiente local).
	mux.HandleFunc("GET /checkout/{id}", s.checkoutPage)
	mux.HandleFunc("POST /checkout/{id}/{action}", s.checkoutAction)
	mux.HandleFunc("POST /sim/scenarios", s.postScenario)
	mux.HandleFunc("POST /sim/charges/{id}/actions", s.postAction)
	mux.HandleFunc("GET /sim/charges", s.simCharges)
	mux.HandleFunc("GET /{$}", s.dashboard)
	return http.MaxBytesHandler(mux, 64<<10)
}

func (s *Sim) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || len(key) < 8 {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r, key)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("resposta não enviada", slog.String("error", err.Error()))
	}
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Sim) putWebhook(w http.ResponseWriter, r *http.Request, key string) {
	var in struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.ConfigureWebhook(key, in.URL, in.Secret); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": in.URL})
}

func (s *Sim) postCustomer(w http.ResponseWriter, r *http.Request, key string) {
	var in Customer
	if err := decode(r, &in); err != nil || in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	c, err := s.CreateCustomer(key, in)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Sim) postCharge(w http.ResponseWriter, r *http.Request, key string) {
	var in CreateChargeInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	c, scenario, err := s.CreateCharge(key, in)
	switch {
	case errors.Is(err, ErrDeclined):
		writeErr(w, http.StatusUnprocessableEntity, "charge_declined")
		return
	case errors.Is(err, ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	case err != nil:
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	switch scenario {
	case "timeout_after_success":
		// A cobrança já foi criada; a resposta demora além do timeout do
		// cliente, que fica sem confirmação (resultado desconhecido).
		wait(r.Context(), s.TimeoutDelay)
	case "error_after_success":
		writeErr(w, http.StatusBadGateway, "upstream_error")
		return
	case "slow":
		wait(r.Context(), s.SlowDelay)
	}
	writeJSON(w, http.StatusCreated, c)
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *Sim) getCharge(w http.ResponseWriter, r *http.Request, key string) {
	c, err := s.View(key, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Sim) deleteCharge(w http.ResponseWriter, r *http.Request, key string) {
	c, err := s.Cancel(key, r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrNotCancelable):
		writeErr(w, http.StatusUnprocessableEntity, "charge_not_cancelable")
	case errors.Is(err, ErrCancelUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "temporarily_unavailable")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal_error")
	default:
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Sim) listCharges(w http.ResponseWriter, r *http.Request, key string) {
	q := r.URL.Query()
	var from, to time.Time
	var err error
	if v := q.Get("updated_from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_updated_from")
			return
		}
	}
	if v := q.Get("updated_to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_updated_to")
			return
		}
	}
	offset, _ := strconv.Atoi(q.Get("offset")) // ausente ou inválido equivale a zero
	limit, _ := strconv.Atoi(q.Get("limit"))   // ausente ou inválido usa o padrão
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	items, more := s.ListCharges(key, q.Get("external_reference"), from, to, max(offset, 0), limit)
	if items == nil {
		items = []ChargeView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items, "has_more": more})
}

func (s *Sim) postRefund(w http.ResponseWriter, r *http.Request, key string) {
	var in RefundInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ref, err := s.Refund(key, r.PathValue("id"), in)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, ErrRefundUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	case errors.Is(err, ErrRefundResponseLost):
		writeErr(w, http.StatusBadGateway, "upstream_error")
		return
	}
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Sim) postScenario(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey   string `json:"api_key"`
		Scenario string `json:"scenario"`
	}
	if err := decode(r, &in); err != nil || in.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.EnqueueScenario(in.APIKey, in.Scenario); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"scheduled": in.Scenario})
}

func (s *Sim) postAction(w http.ResponseWriter, r *http.Request) {
	var a Action
	if err := decode(r, &a); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	c, err := s.Apply(r.PathValue("id"), a)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Sim) simCharges(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.st.ChargesSorted()})
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"brl": func(v int64) string {
		neg := v < 0
		if neg {
			v = -v
		}
		s := "R$ " + strconv.FormatInt(v/100, 10) + "," + strconv.FormatInt(100+v%100, 10)[1:]
		if neg {
			s = "-" + s
		}
		return s
	},
}).Parse(`<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>
body{font-family:system-ui,sans-serif;margin:0;background:#f5f6f8;color:#1f2328}main{max-width:880px;margin:2rem auto;padding:0 1rem}
.card{background:#fff;border:1px solid #d0d7de;border-radius:8px;padding:1.5rem;margin-bottom:1rem}
table{width:100%;border-collapse:collapse}td,th{padding:.4rem;border-bottom:1px solid #eaeef2;text-align:left;font-size:.9rem}
button{font-size:1rem;padding:.6rem 1.2rem;border-radius:6px;border:1px solid #1f6feb;background:#1f6feb;color:#fff;cursor:pointer;margin-right:.5rem}
button.secondary{background:#fff;color:#cf222e;border-color:#cf222e}.muted{color:#57606a;font-size:.85rem}
</style></head><body><main>
<p class="muted">Simulador local de pagamentos: ambiente exclusivo de desenvolvimento e testes.</p>
{{if .Charge}}{{with .Charge}}<div class="card"><h1>Pagamento da cobrança</h1>
<p>{{.Description}}</p><p><strong>{{brl .Amount}}</strong>: vencimento {{.DueDate}}</p><p>Situação: <strong>{{.Status}}</strong></p>
{{if eq .Status "pending"}}<form method="post" action="/checkout/{{.ID}}/pay"><button type="submit">Confirmar pagamento</button></form>
<form method="post" action="/checkout/{{.ID}}/fail" style="margin-top:.5rem"><button type="submit" class="secondary">Recusar pagamento</button></form>{{end}}
</div>{{end}}{{else}}<div class="card"><h1>Cobranças simuladas</h1><table><thead><tr><th>ID</th><th>Referência</th><th>Valor</th><th>Situação</th><th>Atualização</th></tr></thead><tbody>
{{range .Charges}}<tr><td><a href="/checkout/{{.ID}}">{{.ID}}</a></td><td>{{.ExternalReference}}</td><td>{{brl .Amount}}</td><td>{{.Status}}</td><td>{{.UpdatedAt.Format "02/01/2006 15:04:05"}}</td></tr>{{else}}<tr><td colspan="5">Nenhuma cobrança.</td></tr>{{end}}
</tbody></table><p class="muted">Controles: POST /sim/scenarios e POST /sim/charges/{id}/actions (pay, pay_silently, fail, refund, chargeback, diverge, replay); DELETE /v1/charges/{id} cancela a cobrança pendente.</p></div>{{end}}
</main></body></html>`))

func (s *Sim) render(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	if err := pageTmpl.Execute(w, data); err != nil {
		slog.Default().Warn("renderização falhou", slog.String("error", err.Error()))
	}
}

func (s *Sim) checkoutPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.Charge("", r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, map[string]any{"Title": "Pagamento", "Charge": c})
}

func (s *Sim) checkoutAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "pay" && action != "fail" {
		http.NotFound(w, r)
		return
	}
	if _, err := s.Apply(r.PathValue("id"), Action{Name: action}); err != nil && !errors.Is(err, ErrNotFound) {
		slog.Default().Info("ação de checkout ignorada", slog.String("error", err.Error()))
	}
	http.Redirect(w, r, "/checkout/"+url.PathEscape(r.PathValue("id")), http.StatusSeeOther) //nolint:gosec // caminho relativo do próprio simulador
}

func (s *Sim) dashboard(w http.ResponseWriter, _ *http.Request) {
	s.render(w, map[string]any{"Title": "Simulador de pagamentos", "Charges": s.st.ChargesSorted()})
}

// RunDispatcher entrega webhooks pendentes até ctx terminar.
func (s *Sim) RunDispatcher(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.DispatchDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
