package httpapi

import (
	"encoding/csv"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

func invalidField(field, msg string) error {
	return apperr.Invalid("invalid_field", "Dados inválidos.", apperr.FieldError{Field: field, Message: msg})
}

type invoiceDTO struct {
	ID                 uuid.UUID  `json:"id"`
	Number             *string    `json:"number"`
	CustomerID         uuid.UUID  `json:"customer_id"`
	CustomerName       string     `json:"customer_name"`
	SubscriptionID     *uuid.UUID `json:"subscription_id"`
	Status             string     `json:"status"`
	Kind               string     `json:"kind"`
	Currency           string     `json:"currency"`
	BillingPeriodStart *time.Time `json:"billing_period_start"`
	BillingPeriodEnd   *time.Time `json:"billing_period_end"`
	IssuedAt           *time.Time `json:"issued_at"`
	DueAt              *time.Time `json:"due_at"`
	DueDate            *string    `json:"due_date"`
	Subtotal           int64      `json:"subtotal"`
	DiscountTotal      int64      `json:"discount_total"`
	Total              int64      `json:"total"`
	CreditApplied      int64      `json:"credit_applied"`
	AmountPaid         int64      `json:"amount_paid"`
	AmountCredited     int64      `json:"amount_credited"`
	AmountWrittenOff   int64      `json:"amount_written_off"`
	AmountDue          int64      `json:"amount_due"`
	Overdue            bool       `json:"overdue"`
	PaidAt             *time.Time `json:"paid_at"`
	VoidedAt           *time.Time `json:"voided_at"`
	VoidReason         string     `json:"void_reason"`
	CreatedAt          time.Time  `json:"created_at"`
	Version            int        `json:"version"`
}

func (a *API) invDTOFrom(inv billing.Invoice, loc *time.Location) invoiceDTO {
	d := invoiceDTO{ID: inv.ID, CustomerID: inv.CustomerID, CustomerName: inv.CustomerSnapshot.Name, SubscriptionID: inv.SubscriptionID,
		Status: string(inv.Status), Kind: string(inv.Kind), Currency: inv.Currency.Code(), IssuedAt: inv.IssuedAt, DueAt: inv.DueAt,
		Subtotal: inv.Subtotal.Amount(), DiscountTotal: inv.DiscountTotal.Amount(), Total: inv.Total.Amount(), CreditApplied: inv.CreditApplied.Amount(),
		AmountPaid: inv.AmountPaid.Amount(), AmountCredited: inv.AmountCredited.Amount(), AmountWrittenOff: inv.AmountWrittenOff.Amount(),
		AmountDue: inv.AmountDue.Amount(), Overdue: inv.IsOverdue(a.Clock.Now()), PaidAt: inv.PaidAt, VoidedAt: inv.VoidedAt,
		VoidReason: inv.VoidReason, CreatedAt: inv.CreatedAt, Version: inv.Version}
	if inv.Number != "" {
		n := inv.Number
		d.Number = &n
	}
	if inv.BillingPeriod != nil {
		d.BillingPeriodStart, d.BillingPeriodEnd = &inv.BillingPeriod.Start, &inv.BillingPeriod.End
	}
	if inv.DueAt != nil && loc != nil {
		s := billing.DueDate(*inv.DueAt, loc).String()
		d.DueDate = &s
	}
	return d
}

func (a *API) listInvoices(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	f, loc, err := a.invoiceFilter(r, p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	pg, err := a.Billing.List(r.Context(), p.TenantID, f)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]invoiceDTO, 0, len(pg.Items))
	for _, inv := range pg.Items {
		out = append(out, a.invDTOFrom(inv, loc))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, pg.NextCursor))
}

func (a *API) invoiceFilter(r *http.Request, p Principal) (billing.ListFilter, *time.Location, error) {
	loc, err := a.Billing.Location(r.Context(), p.TenantID)
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	limit, err := queryLimit(r)
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	customerID, err := queryUUID(r, "customer_id")
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	subID, err := queryUUID(r, "subscription_id")
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	from, err := queryDate(r, "issued_from", loc, false)
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	to, err := queryDate(r, "issued_to", loc, true)
	if err != nil {
		return billing.ListFilter{}, nil, err
	}
	q := r.URL.Query()
	return billing.ListFilter{Status: billing.Status(q.Get("status")), CustomerID: customerID, SubscriptionID: subID, IssuedFrom: from,
		IssuedTo: to, Overdue: q.Get("overdue") == "true", Limit: limit, Cursor: q.Get("cursor")}, loc, nil
}

func itemDTO(it billing.Item) map[string]any {
	d := map[string]any{"id": it.ID, "position": it.Position, "kind": it.Kind, "description": it.Description,
		"quantity": money.RatString(it.Quantity), "unit_amount_decimal": money.RatString(it.UnitAmount), "amount": it.Amount.Amount(),
		"discount_amount": it.DiscountAmount.Amount(), "price_version_id": it.PriceVersionID, "usage_aggregate_id": it.UsageAggregateID,
		"price_snapshot": it.PriceSnapshot, "period_start": nil, "period_end": nil}
	if it.Period != nil {
		d["period_start"], d["period_end"] = it.Period.Start, it.Period.End
	}
	return d
}

func (a *API) chargeDTO(c payments.Charge) map[string]any {
	refundable, err := c.Refundable()
	if err != nil {
		refundable = money.Zero(c.Amount.Currency())
	}
	return map[string]any{"id": c.ID, "invoice_id": c.InvoiceID, "customer_id": c.CustomerID, "provider": c.Provider, "attempt": c.Attempt,
		"provider_charge_id": nullable(c.ProviderChargeID), "status": c.Status, "amount": c.Amount.Amount(), "amount_paid": c.AmountPaid.Amount(),
		"fee_amount": c.Fee.Amount(), "amount_refunded": c.AmountRefunded.Amount(), "amount_charged_back": c.AmountChargedBack.Amount(),
		"refundable_amount": refundable.Amount(), "currency": c.Amount.Currency().Code(), "checkout_url": nullable(c.CheckoutURL),
		"due_date": c.DueDate.String(), "paid_at": c.PaidAt, "failure_reason": c.FailureReason, "provider_status": c.ProviderStatus,
		"created_at": c.CreatedAt, "version": c.Version}
}

func (a *API) invoiceDetails(r *http.Request, id uuid.UUID) (map[string]any, int, error) {
	p := mustPrincipal(r)
	d, err := a.Billing.Get(r.Context(), p.TenantID, id)
	if err != nil {
		return nil, 0, err
	}
	charges, err := a.Store.InvoiceCharges(r.Context(), p.TenantID, id)
	if err != nil {
		return nil, 0, err
	}
	base := a.invDTOFrom(d.Invoice, d.Location)
	items := make([]map[string]any, 0, len(d.Items))
	for _, it := range d.Items {
		items = append(items, itemDTO(it))
	}
	hist := make([]map[string]any, 0, len(d.History))
	for _, h := range d.History {
		hist = append(hist, map[string]any{"id": h.ID, "from_status": h.FromStatus, "to_status": h.ToStatus, "occurred_at": h.OccurredAt,
			"actor_type": h.ActorType, "actor_id": h.ActorID, "reason": h.Reason, "details": h.Details})
	}
	ch := make([]map[string]any, 0, len(charges))
	for _, c := range charges {
		ch = append(ch, a.chargeDTO(c))
	}
	notes, err := a.Store.Billing().CreditNotes(r.Context(), p.TenantID, id)
	if err != nil {
		return nil, 0, err
	}
	out := structToMap(base)
	out["items"], out["history"], out["charges"], out["customer_snapshot"] = items, hist, ch, d.CustomerSnapshot
	out["credit_notes"] = creditNotesView(notes)
	return out, d.Version, nil
}

func (a *API) getInvoice(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out, version, err := a.invoiceDetails(r, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, version, out)
}

func (a *API) invoiceAction(w http.ResponseWriter, r *http.Request, fn func(p Principal, id uuid.UUID, version int, reason string) error) {
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
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := fn(mustPrincipal(r), id, version, in.Reason); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.getInvoice(w, r)
}

func (a *API) voidInvoice(w http.ResponseWriter, r *http.Request) {
	a.invoiceAction(w, r, func(p Principal, id uuid.UUID, v int, reason string) error {
		_, err := a.Billing.Void(r.Context(), p.TenantID, p.Actor(), id, reason, v)
		return err
	})
}

func (a *API) markUncollectible(w http.ResponseWriter, r *http.Request) {
	a.invoiceAction(w, r, func(p Principal, id uuid.UUID, v int, reason string) error {
		_, err := a.Billing.MarkUncollectible(r.Context(), p.TenantID, p.Actor(), id, reason, v)
		return err
	})
}

func paymentDTO(p payments.Payment) map[string]any {
	return map[string]any{"id": p.ID, "invoice_id": p.InvoiceID, "charge_id": p.ChargeID, "customer_id": p.CustomerID, "amount": p.Amount.Amount(),
		"fee_amount": p.Fee.Amount(), "currency": p.Amount.Currency().Code(), "method": p.Method, "source": p.Source, "paid_at": p.PaidAt,
		"note": p.Note, "created_at": p.CreatedAt}
}

func (a *API) recordManualPayment(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Amount int64      `json:"amount"`
		PaidAt *time.Time `json:"paid_at"`
		Method string     `json:"method"`
		Note   string     `json:"note"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	paidAt := time.Time{}
	if in.PaidAt != nil {
		paidAt = in.PaidAt.UTC()
	}
	p := mustPrincipal(r)
	uid := p.User.ID
	pay, err := a.Payments.RecordManualPayment(r.Context(), p.TenantID, p.Actor(), &uid, payments.ManualPaymentInput{InvoiceID: id,
		Amount: money.New(in.Amount, money.BRL), PaidAt: paidAt, Method: in.Method, Note: in.Note, RequestKey: idempotencyKeyFrom(r.Context())})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, paymentDTO(pay))
}

func (a *API) retryCharge(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	c, err := a.Payments.RetryCharge(r.Context(), p.TenantID, p.Actor(), id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, a.chargeDTO(*c))
}

func (a *API) runBilling(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	res, err := a.Billing.RunNow(r.Context(), p.TenantID, p.Actor())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if res.InvoiceIDs == nil {
		res.InvoiceIDs = []uuid.UUID{}
	}
	a.writeJSON(w, r, http.StatusOK, map[string]any{"subscriptions_processed": res.Subscriptions, "invoices_created": res.Invoices,
		"invoice_ids": res.InvoiceIDs})
}

// exportInvoices gera o CSV sob demanda (streaming), sem armazenar arquivo:
// o conteúdo respeita o escopo do tenant da requisição e a exportação é auditada.
func (a *API) exportInvoices(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	f, loc, err := a.invoiceFilter(r, p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	f.Limit = 100
	if err := a.Store.AppendAudit(r.Context(), p.TenantID, audit.New(p.TenantID, a.Clock.Now(), p.Actor(), "invoices.exported", "invoice", "",
		map[string]any{"status": f.Status})); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="faturas.csv"`)
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	write := func(rec []string) bool {
		if err := cw.Write(rec); err != nil {
			a.log.WarnContext(r.Context(), "exportação interrompida", slog.String("error", err.Error()))
			return false
		}
		return true
	}
	if !write([]string{"numero", "cliente", "situacao", "emissao", "vencimento", "total", "pago", "saldo", "moeda"}) {
		return
	}
	for pages := 0; pages < 500; pages++ {
		pg, err := a.Billing.List(r.Context(), p.TenantID, f)
		if err != nil {
			a.log.ErrorContext(r.Context(), "exportação falhou", slog.String("error", err.Error()))
			return
		}
		for _, inv := range pg.Items {
			d := a.invDTOFrom(inv, loc)
			issued, due := "", ""
			if inv.IssuedAt != nil {
				issued = calendar.DateOf(*inv.IssuedAt, loc).String()
			}
			if d.DueDate != nil {
				due = *d.DueDate
			}
			if !write([]string{csvSafe(inv.Number), csvSafe(inv.CustomerSnapshot.Name), string(inv.Status), issued, due,
				inv.Total.DecimalString(), inv.AmountPaid.DecimalString(), inv.AmountDue.DecimalString(), inv.Currency.Code()}) {
				return
			}
		}
		if pg.NextCursor == "" {
			break
		}
		f.Cursor = pg.NextCursor
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		a.log.WarnContext(r.Context(), "exportação incompleta", slog.String("error", err.Error()))
	}
}

// csvSafe neutraliza fórmulas em planilhas (injeção de CSV).
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

var documentTmpl = template.Must(template.New("doc").Funcs(template.FuncMap{
	"brl": func(v int64) string {
		neg := v < 0
		if neg {
			v = -v
		}
		ints := strconv.FormatInt(v/100, 10)
		var b strings.Builder
		for i, c := range ints {
			if i > 0 && (len(ints)-i)%3 == 0 {
				b.WriteByte('.')
			}
			b.WriteRune(c)
		}
		s := "R$ " + b.String() + "," + strconv.FormatInt(100+v%100, 10)[1:]
		if neg {
			s = "-" + s
		}
		return s
	},
	"date": func(t *time.Time, loc *time.Location) string {
		if t == nil {
			return "-"
		}
		return t.In(loc).Format("02/01/2006")
	},
}).Parse(`<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><title>Fatura {{.Number}}</title>
<style>body{font-family:system-ui,sans-serif;margin:2rem;color:#1f2328}table{width:100%;border-collapse:collapse;margin:1rem 0}
th,td{border-bottom:1px solid #d0d7de;padding:.4rem;text-align:left}td.num,th.num{text-align:right}.muted{color:#57606a;font-size:.85rem}
@media print{.noprint{display:none}}</style></head><body>
<h1>Fatura {{.Number}}</h1>
<p class="muted">Documento comercial emitido pelo Recorvia em nome de {{.Org}}. Não é documento fiscal.</p>
<p><strong>Cliente:</strong> {{.Customer.Name}}{{if .Customer.LegalName}} ({{.Customer.LegalName}}){{end}}<br>
<strong>Documento:</strong> {{.Customer.DocumentNumber}}<br>
<strong>Emissão:</strong> {{date .Invoice.IssuedAt .Loc}} · <strong>Vencimento:</strong> {{.DueDate}} · <strong>Situação:</strong> {{.Status}}</p>
<table><thead><tr><th>Descrição</th><th class="num">Quantidade</th><th class="num">Valor</th><th class="num">Desconto</th></tr></thead><tbody>
{{range .Items}}<tr><td>{{.Description}}</td><td class="num">{{.Qty}}</td><td class="num">{{brl .Amount}}</td><td class="num">{{brl .Discount}}</td></tr>{{end}}
</tbody></table>
<table><tbody>
<tr><td>Subtotal</td><td class="num">{{brl .Invoice.Subtotal.Amount}}</td></tr>
<tr><td>Descontos</td><td class="num">{{brl .Invoice.DiscountTotal.Amount}}</td></tr>
<tr><td>Créditos aplicados</td><td class="num">{{brl .Invoice.CreditApplied.Amount}}</td></tr>
<tr><td>Pago</td><td class="num">{{brl .Invoice.AmountPaid.Amount}}</td></tr>
<tr><td><strong>Saldo</strong></td><td class="num"><strong>{{brl .Invoice.AmountDue.Amount}}</strong></td></tr>
</tbody></table>
{{if .CheckoutURL}}<p class="noprint"><a href="{{.CheckoutURL}}" rel="noopener noreferrer">Pagar esta fatura</a></p>{{end}}
</body></html>`))

type docItem struct {
	Description      string
	Qty              string
	Amount, Discount int64
}

func (a *API) renderInvoiceDocument(w http.ResponseWriter, r *http.Request, tenantID, id uuid.UUID, orgName string, customerFilter *uuid.UUID) {
	d, err := a.Billing.Get(r.Context(), tenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if customerFilter != nil && d.CustomerID != *customerFilter {
		a.writeError(w, r, apperr.NotFound("invoice_not_found", "Fatura não encontrada."))
		return
	}
	if d.Status == billing.Draft {
		a.writeError(w, r, apperr.NotFound("invoice_not_found", "Fatura não encontrada."))
		return
	}
	charges, err := a.Store.InvoiceCharges(r.Context(), tenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	checkout := ""
	for _, c := range charges {
		if c.Status == payments.ChargeAwaitingPayment && c.CheckoutURL != "" {
			checkout = c.CheckoutURL
		}
	}
	items := make([]docItem, 0, len(d.Items))
	for _, it := range d.Items {
		items = append(items, docItem{Description: it.Description, Qty: money.RatString(it.Quantity), Amount: it.Amount.Amount(), Discount: it.DiscountAmount.Amount()})
	}
	due := "-"
	if d.DueAt != nil {
		due = billing.DueDate(*d.DueAt, d.Location).String()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if err := documentTmpl.Execute(w, map[string]any{"Number": d.Number, "Org": orgName, "Customer": d.CustomerSnapshot, "Invoice": d.Invoice,
		"Loc": d.Location, "DueDate": due, "Status": d.Status, "Items": items, "CheckoutURL": checkout}); err != nil {
		a.log.WarnContext(r.Context(), "documento não renderizado", slog.String("error", err.Error()))
	}
}

func (a *API) invoiceDocument(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	org, err := a.Tenancy.Organization(r.Context(), p.TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.renderInvoiceDocument(w, r, p.TenantID, id, org.Name, nil)
}
