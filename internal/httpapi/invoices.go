package httpapi

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
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
		IssuedTo: to, Overdue: q.Get("overdue") == "true", OpenBalance: q.Get("open_balance") == "true", Limit: limit, Cursor: q.Get("cursor")}, loc, nil
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
	uid := p.UserID()
	pay, err := a.Payments.RecordManualPayment(r.Context(), p.TenantID, p.Actor(), uid, payments.ManualPaymentInput{InvoiceID: id,
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

// exportMaxRows é o máximo de faturas por arquivo exportado. Listagens maiores
// são exportadas em partes: a resposta informa o corte nos cabeçalhos
// X-Recorvia-Export-Truncated e X-Recorvia-Export-Next-Cursor, e a parte
// seguinte é pedida com o cursor informado (ADR-079).
const exportMaxRows = 50000

// exportHeader são as colunas do CSV de faturas. Valores monetários em reais
// com ponto decimal; datas civis no fuso da organização; o período de
// faturamento vai do primeiro ao último dia incluído (o fim do período
// [início, fim) menos um dia), como no documento da fatura.
var exportHeader = []string{"numero", "cliente", "situacao", "emissao", "vencimento", "periodo_inicio", "periodo_fim", "subtotal", "desconto",
	"total", "creditos_aplicados", "valor_creditado", "pago", "valor_baixado", "saldo", "moeda"}

// exportInvoices gera o CSV sob demanda (streaming), sem armazenar arquivo:
// o conteúdo respeita o escopo do tenant da requisição e a exportação é auditada.
func (a *API) exportInvoices(w http.ResponseWriter, r *http.Request) {
	maxRows := a.InvoiceExportMaxRows
	if maxRows <= 0 {
		maxRows = exportMaxRows
	}
	p := mustPrincipal(r)
	f, loc, err := a.invoiceFilter(r, p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	f.Limit = 100
	part := 1
	if v := r.URL.Query().Get("part"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100000 {
			a.writeError(w, r, invalidField("part", "Informe o número da parte, a partir de 1."))
			return
		}
		part = n
	}
	// O corte é calculado antes do primeiro byte: os cabeçalhos informam se o
	// arquivo está completo, e o cursor da parte seguinte começa logo depois
	// da última fatura incluída.
	cut, err := a.Billing.ListCutoff(r.Context(), p.TenantID, f, maxRows)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.Store.AppendAudit(r.Context(), p.TenantID, audit.New(p.TenantID, a.Clock.Now(), p.Actor(), "invoices.exported", "invoice", "",
		map[string]any{"status": f.Status, "continuation": f.Cursor != "", "part": part, "truncated": cut != nil})); err != nil {
		a.writeError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	filename := "faturas.csv"
	if cut != nil || f.Cursor != "" || part > 1 {
		filename = fmt.Sprintf("faturas-parte-%d.csv", part)
	}
	h.Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	h.Set("X-Recorvia-Export-Limit", strconv.Itoa(maxRows))
	h.Set("X-Recorvia-Export-Truncated", strconv.FormatBool(cut != nil))
	if cut != nil {
		h.Set("X-Recorvia-Export-Next-Cursor", cut.NextCursor)
		h.Set("X-Recorvia-Export-Next-Part", strconv.Itoa(part+1))
	}
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	if err := a.writeInvoiceRows(r, cw, f, loc, cut); err != nil {
		a.log.WarnContext(r.Context(), "exportação interrompida", slog.String("error", err.Error()))
		return
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		a.log.WarnContext(r.Context(), "exportação incompleta", slog.String("error", err.Error()))
	}
}

// writeInvoiceRows escreve o cabeçalho e as faturas da listagem até o corte.
func (a *API) writeInvoiceRows(r *http.Request, cw *csv.Writer, f billing.ListFilter, loc *time.Location, cut *billing.Cutoff) error {
	if err := cw.Write(exportHeader); err != nil {
		return err
	}
	dec := func(m money.Money) string { return m.DecimalString() }
	for {
		pg, err := a.Billing.List(r.Context(), mustPrincipal(r).TenantID, f)
		if err != nil {
			return err
		}
		for _, inv := range pg.Items {
			if !cut.Includes(inv) {
				return nil
			}
			issued, due, first, last := "", "", "", ""
			if inv.IssuedAt != nil {
				issued = calendar.DateOf(*inv.IssuedAt, loc).String()
			}
			if inv.DueAt != nil {
				due = billing.DueDate(*inv.DueAt, loc).String()
			}
			if inv.BillingPeriod != nil {
				fd, ld := periodDates(*inv.BillingPeriod, loc)
				first, last = fd.String(), ld.String()
			}
			if err := cw.Write([]string{csvSafe(inv.Number), csvSafe(inv.CustomerSnapshot.Name), string(inv.Status), issued, due, first, last,
				dec(inv.Subtotal), dec(inv.DiscountTotal), dec(inv.Total), dec(inv.CreditApplied), dec(inv.AmountCredited), dec(inv.AmountPaid),
				dec(inv.AmountWrittenOff), dec(inv.AmountDue), inv.Currency.Code()}); err != nil {
				return err
			}
		}
		if pg.NextCursor == "" {
			return nil
		}
		f.Cursor = pg.NextCursor
	}
}

// csvSafe neutraliza fórmulas em planilhas (injeção de CSV).
func csvSafe(s string) string {
	if s != "" && strings.ContainsAny(s[:1], "=+-@\t\r") {
		return "'" + s
	}
	return s
}

// documentFiscalNotice é o aviso obrigatório do documento comercial: a
// fatura do Recorvia não substitui a nota fiscal (RF-06).
const documentFiscalNotice = "Este documento é uma fatura comercial e não é documento fiscal. Não substitui a nota fiscal."

var documentTmpl = template.Must(template.New("doc").Funcs(template.FuncMap{
	"brl": formatBRL,
}).Parse(`<!doctype html><html lang="pt-BR"><head><meta charset="utf-8"><title>Fatura {{.Number}}</title>
<style>body{font-family:system-ui,sans-serif;margin:2rem;color:#1f2328}table{width:100%;border-collapse:collapse;margin:1rem 0}
th,td{border-bottom:1px solid #d0d7de;padding:.4rem;text-align:left}td.num,th.num{text-align:right}.muted{color:#57606a;font-size:.85rem}
.notice{border:1px solid #d0d7de;padding:.5rem;font-size:.9rem}tr.total td{font-weight:600}
@media print{.noprint{display:none}}</style></head><body>
<h1>Fatura {{.Number}}</h1>
<p class="notice" role="note"><strong>{{.FiscalNotice}}</strong></p>
<p><strong>Emitente:</strong> {{.Issuer.Name}}{{if .Issuer.LegalName}} ({{.Issuer.LegalName}}){{end}}{{if .Issuer.Document}}<br>
<strong>CNPJ do emitente:</strong> {{.Issuer.Document}}{{end}}{{if .Issuer.Address}}<br>
<strong>Endereço do emitente:</strong> {{.Issuer.Address}}{{end}}</p>
<p class="muted">Documento comercial emitido pelo Recorvia em nome de {{.Issuer.Name}}.</p>
<p><strong>Cliente:</strong> {{.Customer.Name}}{{if .Customer.LegalName}} ({{.Customer.LegalName}}){{end}}<br>
<strong>{{.CustomerDocumentLabel}}:</strong> {{.CustomerDocument}}<br>
<strong>Endereço de cobrança:</strong> {{if .CustomerAddress}}{{.CustomerAddress}}{{else}}não informado{{end}}</p>
<p><strong>Emissão:</strong> {{.IssuedOn}} · <strong>Vencimento:</strong> {{.DueDate}} · <strong>Situação:</strong> {{.Status}}{{if .Period}}<br>
<strong>Período de faturamento:</strong> {{.Period}}{{end}}<br>
<strong>Condições de pagamento:</strong> {{.Terms}}</p>
{{if .Cancellation}}<p class="notice" role="status"><strong>{{.Cancellation}}</strong></p>{{end}}
<table><thead><tr><th>Descrição</th><th>Período</th><th class="num">Quantidade</th><th class="num">Valor</th><th class="num">Desconto</th></tr></thead><tbody>
{{range .Items}}<tr><td>{{.Description}}</td><td>{{if .Period}}{{.Period}}{{else}}-{{end}}</td><td class="num">{{.Qty}}</td><td class="num">{{brl .Amount}}</td><td class="num">{{brl .Discount}}</td></tr>{{end}}
</tbody></table>
<table><tbody>
{{range .Totals}}<tr{{if .Strong}} class="total"{{end}}><td>{{.Label}}</td><td class="num">{{brl .Amount}}</td></tr>
{{end}}</tbody></table>
{{if .Memo}}<p><strong>Observações:</strong> {{.Memo}}</p>{{end}}
{{if .CheckoutURL}}<p class="noprint"><a href="{{.CheckoutURL}}" rel="noopener noreferrer">Pagar esta fatura</a></p>{{end}}
</body></html>`))

// formatBRL formata um valor em centavos como moeda brasileira.
func formatBRL(v int64) string {
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
}

type docItem struct {
	Description      string
	Period           string
	Qty              string
	Amount, Discount int64
}

// docTotal é uma linha do quadro de totais. A sequência fecha a conta:
// Subtotal - Descontos = Total; Total - Créditos aplicados - Notas de crédito
// - Pago - Valor baixado = Saldo.
type docTotal struct {
	Label  string
	Amount int64
	Strong bool
}

// DocumentIssuer identifica o emitente (a organização) no documento.
// Address é o endereço gravado na emissão, já formatado em uma linha; vazio
// quando a fatura não o tem.
type DocumentIssuer struct {
	Name      string
	LegalName string
	Document  string
	Address   string
}

// invoiceDocumentData reúne o que o documento comercial exibe. Issuer são os
// dados atuais da organização, usados somente quando a fatura não tem o
// emitente gravado na emissão.
type invoiceDocumentData struct {
	Issuer      DocumentIssuer
	Invoice     billing.Invoice
	Location    *time.Location
	CheckoutURL string
	// CustomerFacing indica o documento exibido ao próprio cliente (portal).
	CustomerFacing bool
}

// documentStatusLabels traduz a situação da fatura para o documento.
var documentStatusLabels = map[billing.Status]string{billing.Draft: "Rascunho", billing.Open: "Em aberto",
	billing.PartiallyPaid: "Parcialmente paga", billing.Paid: "Paga", billing.Void: "Cancelada", billing.Uncollectible: "Incobrável"}

// documentStatus devolve o rótulo da situação. Para o cliente, a baixa como
// incobrável (registro interno da organização) aparece como encerrada.
func documentStatus(s billing.Status, customerFacing bool) string {
	if customerFacing && s == billing.Uncollectible {
		return "Encerrada"
	}
	if l, ok := documentStatusLabels[s]; ok {
		return l
	}
	return string(s)
}

// formatDate formata a data civil como DD/MM/AAAA.
func formatDate(d calendar.Date) string {
	return fmt.Sprintf("%02d/%02d/%04d", d.Day, int(d.Month), d.Year)
}

// formatPeriod mostra o período [início, fim) pelas datas civis do primeiro
// e do último dia incluídos, no fuso da organização.
func formatPeriod(p *calendar.Period, loc *time.Location) string {
	if p == nil {
		return ""
	}
	first, last := periodDates(*p, loc)
	if last == first {
		return formatDate(first)
	}
	return formatDate(first) + " a " + formatDate(last)
}

// periodDates devolve o primeiro e o último dia civil incluídos no período
// [início, fim), no fuso da organização.
func periodDates(p calendar.Period, loc *time.Location) (first, last calendar.Date) {
	first, last = calendar.DateOf(p.Start, loc), calendar.DateOf(p.Start, loc)
	if p.End.After(p.Start) {
		last = calendar.DateOf(p.End.Add(-time.Nanosecond), loc)
	}
	return first, last
}

// formatDocument pontua CPF (11 dígitos) e CNPJ (14 caracteres); outros
// valores (por exemplo, de cliente anonimizado) são exibidos como estão.
func formatDocument(kind, doc string) string {
	switch {
	case kind == "cpf" && len(doc) == 11 && strings.Trim(doc, "0123456789") == "":
		return doc[0:3] + "." + doc[3:6] + "." + doc[6:9] + "-" + doc[9:]
	case (kind == "cnpj" || kind == "") && len(doc) == 14:
		return doc[0:2] + "." + doc[2:5] + "." + doc[5:8] + "/" + doc[8:12] + "-" + doc[12:]
	}
	return doc
}

// formatAddress monta o endereço (de cobrança do cliente ou do emitente) em
// uma linha.
func formatAddress(a *customers.Address) string {
	if a == nil {
		return ""
	}
	parts := []string{a.Street + ", " + a.Number}
	if a.Complement != "" {
		parts[0] += ", " + a.Complement
	}
	city := a.City
	if a.State != "" {
		city += "/" + a.State
	}
	parts = append(parts, a.District, city)
	if len(a.PostalCode) == 8 {
		parts = append(parts, "CEP "+a.PostalCode[:5]+"-"+a.PostalCode[5:])
	} else if a.PostalCode != "" {
		parts = append(parts, "CEP "+a.PostalCode)
	}
	if a.Country != "" && a.Country != "BR" {
		parts = append(parts, a.Country)
	}
	return strings.Join(parts, " - ")
}

// documentTerms descreve as condições de pagamento a partir das datas
// gravadas na emissão (não do prazo atual da organização, que pode mudar).
func documentTerms(issued, due *calendar.Date) string {
	if due == nil {
		return "-"
	}
	if issued == nil {
		return "Pagamento até " + formatDate(*due) + "."
	}
	switch days := calendar.DaysBetween(*issued, *due); {
	case days <= 0:
		return "Pagamento à vista, até " + formatDate(*due) + "."
	case days == 1:
		return "Pagamento até " + formatDate(*due) + " (1 dia após a emissão)."
	default:
		return fmt.Sprintf("Pagamento até %s (%d dias após a emissão).", formatDate(*due), days)
	}
}

// documentTotals monta o quadro de totais. Para o cliente, o valor baixado
// como incobrável aparece como saldo encerrado pela organização. Na fatura
// cancelada, o crédito aplicado volta ao cliente e o restante é anulado: a
// conta fecha com saldo zero.
func documentTotals(inv billing.Invoice, customerFacing bool) []docTotal {
	writtenOff := "Valor baixado"
	if customerFacing {
		writtenOff = "Saldo encerrado pela organização"
	}
	rows := []docTotal{
		{Label: "Subtotal", Amount: inv.Subtotal.Amount()},
		{Label: "Descontos", Amount: -inv.DiscountTotal.Amount()},
		{Label: "Total", Amount: inv.Total.Amount(), Strong: true},
		{Label: "Créditos aplicados", Amount: -inv.CreditApplied.Amount()},
		{Label: "Notas de crédito", Amount: -inv.AmountCredited.Amount()},
		{Label: "Pago", Amount: -inv.AmountPaid.Amount()},
		{Label: writtenOff, Amount: -inv.AmountWrittenOff.Amount()},
	}
	if inv.Status == billing.Void {
		returned := inv.CreditApplied.Amount()
		canceled := inv.Total.Amount() - inv.CreditApplied.Amount() - inv.AmountCredited.Amount() - inv.AmountPaid.Amount() -
			inv.AmountWrittenOff.Amount() + returned
		rows = append(rows, docTotal{Label: "Crédito devolvido ao cliente", Amount: returned},
			docTotal{Label: "Valor cancelado", Amount: -canceled})
	}
	return append(rows, docTotal{Label: "Saldo", Amount: inv.AmountDue.Amount(), Strong: true})
}

// documentIssuer devolve o emitente gravado na emissão; faturas emitidas
// antes da ADR-079 não o têm e usam os dados atuais da organização. O
// endereço só aparece quando gravado na emissão (ADR-084): a fatura emitida
// antes do endereço, ou sem endereço cadastrado, sai sem ele, e o endereço
// atual da organização nunca substitui o da emissão.
func documentIssuer(inv billing.Invoice, current DocumentIssuer) DocumentIssuer {
	if inv.Issuer == nil {
		current.Address = ""
		return current
	}
	return DocumentIssuer{Name: inv.Issuer.Name, LegalName: inv.Issuer.LegalName, Document: inv.Issuer.DocumentNumber,
		Address: formatAddress(inv.Issuer.Address)}
}

// documentCancellation descreve o cancelamento da fatura no documento; o
// motivo, registro interno da organização, não aparece para o cliente.
func documentCancellation(inv billing.Invoice, loc *time.Location, customerFacing bool) string {
	if inv.Status != billing.Void {
		return ""
	}
	s := "Fatura cancelada"
	if inv.VoidedAt != nil {
		s += " em " + formatDate(calendar.DateOf(*inv.VoidedAt, loc))
	}
	s += "; não há valor a pagar."
	if inv.CreditApplied.IsPositive() {
		s += " O crédito aplicado de " + formatBRL(inv.CreditApplied.Amount()) + " foi devolvido ao saldo de créditos do cliente."
	}
	if !customerFacing && inv.VoidReason != "" {
		s += " Motivo: " + inv.VoidReason
	}
	return s
}

// writeInvoiceDocument renderiza o documento comercial (HTML sem scripts,
// com CSP restritiva). Quem chama garante que a fatura foi emitida e
// pertence ao escopo da requisição.
func (a *API) writeInvoiceDocument(w http.ResponseWriter, r *http.Request, d invoiceDocumentData) {
	items := make([]docItem, 0, len(d.Invoice.Items))
	for _, it := range d.Invoice.Items {
		items = append(items, docItem{Description: it.Description, Period: formatPeriod(it.Period, d.Location), Qty: money.RatString(it.Quantity),
			Amount: it.Amount.Amount(), Discount: it.DiscountAmount.Amount()})
	}
	var issued, due *calendar.Date
	issuedOn, dueOn := "-", "-"
	if d.Invoice.IssuedAt != nil {
		dd := calendar.DateOf(*d.Invoice.IssuedAt, d.Location)
		issued, issuedOn = &dd, formatDate(dd)
	}
	if d.Invoice.DueAt != nil {
		dd := billing.DueDate(*d.Invoice.DueAt, d.Location)
		due, dueOn = &dd, formatDate(dd)
	}
	snap := d.Invoice.CustomerSnapshot
	docLabel := "Documento"
	switch snap.DocumentType {
	case "cpf":
		docLabel = "CPF"
	case "cnpj":
		docLabel = "CNPJ"
	}
	issuer := documentIssuer(d.Invoice, d.Issuer)
	issuer.Document = formatDocument("cnpj", issuer.Document)
	terms := documentTerms(issued, due)
	if d.Invoice.Status == billing.Void {
		terms = "Fatura cancelada."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if err := documentTmpl.Execute(w, map[string]any{"Number": d.Invoice.Number, "Issuer": issuer, "FiscalNotice": documentFiscalNotice,
		"Customer": snap, "CustomerDocumentLabel": docLabel, "CustomerDocument": formatDocument(snap.DocumentType, snap.DocumentNumber),
		"CustomerAddress": formatAddress(snap.Address), "IssuedOn": issuedOn, "DueDate": dueOn, "Terms": terms,
		"Cancellation": documentCancellation(d.Invoice, d.Location, d.CustomerFacing),
		"Period":       formatPeriod(d.Invoice.BillingPeriod, d.Location), "Status": documentStatus(d.Invoice.Status, d.CustomerFacing),
		"Items": items, "Totals": documentTotals(d.Invoice, d.CustomerFacing), "Memo": d.Invoice.Memo, "CheckoutURL": d.CheckoutURL}); err != nil {
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
	d, err := a.Billing.Get(r.Context(), p.TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if d.Status == billing.Draft {
		a.writeError(w, r, apperr.NotFound("invoice_not_found", "Fatura não encontrada."))
		return
	}
	charges, err := a.Store.InvoiceCharges(r.Context(), p.TenantID, id)
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
	a.writeInvoiceDocument(w, r, invoiceDocumentData{Issuer: DocumentIssuer{Name: org.Name, LegalName: org.LegalName, Document: org.DocumentNumber},
		Invoice: d.Invoice, Location: d.Location, CheckoutURL: checkout})
}
