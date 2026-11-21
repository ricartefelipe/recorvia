//go:build integration

package httpapi_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/metering"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/reporting"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// invFiscalNotice é o aviso obrigatório do documento comercial.
const invFiscalNotice = "Este documento é uma fatura comercial e não é documento fiscal. Não substitui a nota fiscal."

// invAPIWithExportLimit monta, sobre o ambiente de newAdvancedAPI, um segundo
// servidor com o limite de faturas por arquivo exportado reduzido; as sessões
// valem nos dois servidores.
func invAPIWithExportLimit(t *testing.T, maxRows int) (full, limited *apiEnv) {
	t.Helper()
	a := newAdvancedAPI(t)
	env := a.env
	idsvc := identity.NewService(stubIDP{}, a.sessions, env.Store.Users(), env.Clock, identity.DefaultPolicy())
	cfg := &config.Config{Env: config.Test, CookieName: "recorvia_session", CookieSecure: false, RateLimitPerMinute: 10000}
	pay := env.Payments(gateways.NewResolver(time.Second), payments.Config{WebhookBaseURL: "http://api.test/webhooks/providers"})
	_, router := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Config: cfg, Clock: env.Clock,
		Store: env.Store, Sessions: a.sessions, Identity: idsvc, Tenancy: env.Tenancy, Platform: env.Platform, Customers: env.Customers,
		Catalog: env.Catalog, Subscriptions: env.Subscriptions, Billing: env.Billing, Payments: pay,
		Changes:     billing.NewChangeService(env.Store.Billing(), env.Clock),
		CreditNotes: billing.NewCreditNoteService(env.Store.Billing(), env.Clock),
		Coupons:     catalog.NewCouponService(env.Store.Catalog(), env.Clock),
		Terms:       subscriptions.NewTermsService(env.Store.Subscriptions(), env.Clock),
		Metering:    metering.NewService(env.Store.Metering(), env.Clock),
		Reporting:   reporting.NewService(env.Store.Reporting(), env.Clock), InvoiceExportMaxRows: maxRows})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	l := *a
	l.srv = srv
	return a, &l
}

// invGetText faz um GET autenticado com a sessão do cliente e devolve o corpo
// textual (CSV, HTML).
func invGetText(c *client, path string) rawResponse {
	c.a.t.Helper()
	return c.a.raw(http.MethodGet, path, nil, map[string]string{"X-Recorvia-Tenant": c.tenant},
		&http.Cookie{Name: "recorvia_session", Value: c.cookie})
}

// invIssuerTenant cria a organização com razão social e CNPJ, que o
// documento da fatura exibe como emitente.
func invIssuerTenant(t *testing.T, env *fixture.Env, prefix string) fixture.Tenant {
	t.Helper()
	ctx := context.Background()
	tn := env.NewTenant(t, prefix)
	org, err := env.Tenancy.Organization(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Tenancy.UpdateOrganization(ctx, tn.ID, tn.Actor, tenancy.TenantInput{Name: org.Name, LegalName: "Emitente Serviços Ltda.",
		DocumentNumber: "11444777000161", Timezone: org.Timezone, InvoicePrefix: org.InvoicePrefix}, org.Version); err != nil {
		t.Fatal(err)
	}
	return tn
}

// invEtag lê a versão atual da fatura para o If-Match.
func invEtag(c *client, invPath string) string {
	c.a.t.Helper()
	_, h := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	return h.Get("ETag")
}

// invFullInvoice emite, para o cliente, uma fatura de 200,00 com desconto
// de 20,00 (cupom), crédito aplicado de 15,00, nota de crédito de 30,00 e
// pagamento parcial de 50,00: saldo de 85,00, ainda sem baixa.
func invFullInvoice(t *testing.T, a *apiEnv, c *client, tn fixture.Tenant, cust customers.Customer) map[string]any {
	t.Helper()
	custPath := "/api/v1/customers/" + cust.ID.String()
	c.expect(http.StatusOK, http.MethodPut, custPath+"/billing-address", map[string]any{"street": "Avenida Paulista", "number": "1000",
		"complement": "Conjunto 12", "district": "Bela Vista", "city": "São Paulo", "state": "SP", "postal_code": "01310-100"})
	c.expect(http.StatusCreated, http.MethodPost, "/api/v1/coupons", map[string]any{"code": "DOC20", "name": "Desconto do documento",
		"discount_type": "amount", "amount_off": 2000, "duration": "once"})
	plan := newPlan(t, a.env, tn, "Plano Documento", flatPrice(20000))
	sub := a.env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	c.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions/"+sub.ID.String()+"/discounts", map[string]any{"coupon_code": "DOC20"}, "Idempotency-Key", idemKey())
	c.expect(http.StatusCreated, http.MethodPost, custPath+"/credits", map[string]any{"amount": 1500, "reason": "Cortesia comercial"},
		"Idempotency-Key", "credito-"+uuid.NewString())
	inv := issuedInvoice(c, sub.ID)
	if inv["subtotal"] != float64(20000) || inv["discount_total"] != float64(2000) || inv["total"] != float64(18000) ||
		inv["credit_applied"] != float64(1500) || inv["amount_due"] != float64(16500) {
		t.Fatalf("fatura emitida com desconto e crédito: %v", inv)
	}
	invPath := "/api/v1/invoices/" + inv["id"].(string)
	c.expect(http.StatusCreated, http.MethodPost, invPath+"/credit-notes", map[string]any{"amount": 3000, "reason": "Ajuste comercial negociado"},
		"Idempotency-Key", "nota-"+uuid.NewString())
	c.expect(http.StatusCreated, http.MethodPost, invPath+"/payments", map[string]any{"amount": 5000, "method": "pix",
		"note": "Pagamento parcial identificado"}, "Idempotency-Key", "pagamento-"+uuid.NewString())
	got, _ := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	if got["status"] != "partially_paid" || got["amount_credited"] != float64(3000) || got["amount_paid"] != float64(5000) ||
		got["amount_due"] != float64(8500) {
		t.Fatalf("fatura com nota de crédito e pagamento parcial: %v", got)
	}
	return got
}

var invTotalRow = regexp.MustCompile(`<tr(?: class="total")?><td>([^<]+)</td><td class="num">([^<]+)</td></tr>`)

func invDocumentTotals(t *testing.T, html string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range invTotalRow.FindAllStringSubmatch(html, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// invPeriod devolve o primeiro e o último dia incluídos no período [início,
// fim) informado pela API, no fuso da organização.
func invPeriod(t *testing.T, start, end any) (first, last calendar.Date) {
	t.Helper()
	loc := mustLocation(t)
	s, e := instant(t, start), instant(t, end)
	return calendar.DateOf(s, loc), calendar.DateOf(e.Add(-time.Nanosecond), loc)
}

func invBR(d calendar.Date) string {
	return d.String()[8:10] + "/" + d.String()[5:7] + "/" + d.String()[:4]
}

// Documento da fatura com desconto, crédito, nota de crédito, pagamento
// parcial e, depois, baixa: o quadro de totais fecha com o saldo em cada
// etapa, e o documento traz emitente, cliente com endereço, período,
// vencimento, condições e o aviso de que não é documento fiscal.
func TestInvoiceDocumentTotalsCloseOverHTTP(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := invIssuerTenant(t, a.env, "documento")
	c := a.login(tn.Owner, &tn.ID)
	cust := a.env.NewCustomer(t, tn)
	inv := invFullInvoice(t, a, c, tn, cust)
	invPath := "/api/v1/invoices/" + inv["id"].(string)

	first, last := invPeriod(t, inv["billing_period_start"], inv["billing_period_end"])
	issued := calendar.DateOf(instant(t, inv["issued_at"]), mustLocation(t))
	due, err := calendar.ParseDate(inv["due_date"].(string))
	if err != nil {
		t.Fatal(err)
	}
	doc := cust.DocumentNumber
	common := []string{invFiscalNotice,
		"<strong>Emitente:</strong> Org documento (Emitente Serviços Ltda.)",
		"<strong>CNPJ do emitente:</strong> 11.444.777/0001-61",
		"<strong>CNPJ:</strong> " + doc[0:2] + "." + doc[2:5] + "." + doc[5:8] + "/" + doc[8:12] + "-" + doc[12:],
		"<strong>Endereço de cobrança:</strong> Avenida Paulista, 1000, Conjunto 12 - Bela Vista - São Paulo/SP - CEP 01310-100",
		"<strong>Emissão:</strong> " + invBR(issued), "<strong>Vencimento:</strong> " + invBR(due),
		"<strong>Período de faturamento:</strong> " + invBR(first) + " a " + invBR(last),
		"<td>" + invBR(first) + " a " + invBR(last) + "</td>",
		"<strong>Condições de pagamento:</strong> Pagamento ",
	}
	want := map[string]string{"Subtotal": "R$ 200,00", "Descontos": "-R$ 20,00", "Total": "R$ 180,00", "Créditos aplicados": "-R$ 15,00",
		"Notas de crédito": "-R$ 30,00", "Pago": "-R$ 50,00", "Valor baixado": "R$ 0,00", "Saldo": "R$ 85,00"}
	check := func(stage string, want map[string]string, status string) {
		t.Helper()
		r := invGetText(c, invPath+"/document")
		if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: documento: %d %s", stage, r.status, r.header.Get("Content-Type"))
		}
		for _, s := range append(common, "<strong>Situação:</strong> "+status, invBR(due)) {
			if !strings.Contains(r.text, s) {
				t.Errorf("%s: documento sem %q", stage, s)
			}
		}
		if got := invDocumentTotals(t, r.text); len(got) != len(want) {
			t.Fatalf("%s: quadro de totais %v, esperado %v", stage, got, want)
		} else {
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s: %s = %q, esperado %q", stage, k, got[k], v)
				}
			}
		}
	}
	check("parcialmente paga", want, "Parcialmente paga")

	// O emitente é o da emissão: mudar a razão social e o CNPJ da organização
	// não altera o documento já emitido.
	org, err := a.env.Tenancy.Organization(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.env.Tenancy.UpdateOrganization(context.Background(), tn.ID, tn.Actor, tenancy.TenantInput{Name: "Nome Novo",
		LegalName: "Razão Social Nova S.A.", DocumentNumber: "11222333000181", Timezone: org.Timezone, InvoicePrefix: org.InvoicePrefix},
		org.Version); err != nil {
		t.Fatal(err)
	}
	if r := invGetText(c, invPath+"/document"); strings.Contains(r.text, "Razão Social Nova") || strings.Contains(r.text, "Nome Novo") ||
		strings.Contains(r.text, "11.222.333/0001-81") {
		t.Fatalf("documento emitido passou a usar os dados atuais da organização:\n%s", r.text)
	}

	got, _ := c.expect(http.StatusOK, http.MethodPost, invPath+"/mark-uncollectible", map[string]any{"reason": "Cliente sem condições de pagar"},
		"If-Match", invEtag(c, invPath))
	if got["status"] != "uncollectible" || got["amount_written_off"] != float64(8500) || got["amount_due"] != float64(0) {
		t.Fatalf("baixa como incobrável: %v", got)
	}
	want["Valor baixado"], want["Saldo"] = "-R$ 85,00", "R$ 0,00"
	check("baixada", want, "Incobrável")
}

func invParseCSV(t *testing.T, text string) [][]string {
	t.Helper()
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = ';'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("CSV inválido: %v\n%s", err, text)
	}
	return rows
}

var invExportHeader = []string{"numero", "cliente", "situacao", "emissao", "vencimento", "periodo_inicio", "periodo_fim", "subtotal", "desconto",
	"total", "creditos_aplicados", "valor_creditado", "pago", "valor_baixado", "saldo", "moeda"}

// Exportação CSV com conteúdo real: colunas de desconto, créditos, nota de
// crédito, baixa e período; neutralização de fórmulas nos textos; corte
// informado nos cabeçalhos e continuação em partes sem lacunas nem
// repetições; auditoria de cada parte; e escopo do tenant.
func TestInvoiceExportOverHTTP(t *testing.T) {
	full, limited := invAPIWithExportLimit(t, 2)
	tn := invIssuerTenant(t, full.env, "exportacao")
	c := full.login(tn.Owner, &tn.ID)
	ctx := context.Background()
	inv := invFullInvoice(t, full, c, tn, full.env.NewCustomer(t, tn))
	invPath := "/api/v1/invoices/" + inv["id"].(string)
	c.expect(http.StatusOK, http.MethodPost, invPath+"/mark-uncollectible", map[string]any{"reason": "Cliente sem condições de pagar"},
		"If-Match", invEtag(c, invPath))

	// Nomes que planilhas interpretariam como fórmula.
	formulas := []string{"=HIPERLINK(\"http://x.test\")", "+55 Telecom", "-Menos Serviços", "@Arroba Comércio"}
	plan := newPlan(t, full.env, tn, "Plano Simples", flatPrice(10000))
	for _, name := range formulas {
		cust := full.env.NewCustomer(t, tn)
		if _, err := full.env.Customers.Update(ctx, tn.ID, tn.Actor, cust.ID, customers.Input{Kind: cust.Kind, Name: name,
			DocumentNumber: cust.DocumentNumber, Email: cust.Email}, cust.Version); err != nil {
			t.Fatal(err)
		}
		full.env.NewSubscription(t, tn, cust.ID, plan.ID, nil)
	}
	runBilling(c)

	// Outro tenant com fatura própria, que não pode aparecer na exportação.
	other := full.env.NewTenant(t, "exportacao-outro")
	issueInvoice(t, full, other, full.env.NewCustomer(t, other).ID, 7700)

	whole := invGetText(c, "/api/v1/invoices/export")
	if whole.status != http.StatusOK || !strings.HasPrefix(whole.header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(whole.header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("exportação: %d %v", whole.status, whole.header)
	}
	if whole.header.Get("X-Recorvia-Export-Truncated") != "false" || whole.header.Get("X-Recorvia-Export-Next-Cursor") != "" ||
		whole.header.Get("Content-Disposition") != `attachment; filename="faturas.csv"` ||
		whole.header.Get("X-Recorvia-Export-Limit") != "50000" {
		t.Fatalf("exportação completa deveria informar que não houve corte: %v", whole.header)
	}
	rows := invParseCSV(t, whole.text)
	if !slices.Equal(rows[0], invExportHeader) || len(rows) != 6 {
		t.Fatalf("cabeçalho e linhas: %v", rows)
	}
	byNumber := map[string][]string{}
	names := map[string]bool{}
	for _, r := range rows[1:] {
		if len(r) != len(invExportHeader) {
			t.Fatalf("linha com colunas a mais ou a menos: %v", r)
		}
		byNumber[r[0]] = r
		names[r[1]] = true
	}
	for _, name := range formulas {
		if !names["'"+name] || names[name] {
			t.Errorf("nome %q deveria sair neutralizado com apóstrofo: %v", name, names)
		}
	}
	row, ok := byNumber[inv["number"].(string)]
	if !ok {
		t.Fatalf("fatura completa ausente da exportação: %v", byNumber)
	}
	first, last := invPeriod(t, inv["billing_period_start"], inv["billing_period_end"])
	issued := calendar.DateOf(instant(t, inv["issued_at"]), mustLocation(t)).String()
	wantRow := []string{inv["number"].(string), inv["customer_name"].(string), "uncollectible", issued, inv["due_date"].(string), first.String(),
		last.String(), "200.00", "20.00", "180.00", "15.00", "30.00", "50.00", "85.00", "0.00", "BRL"}
	if !slices.Equal(row, wantRow) {
		t.Fatalf("linha da fatura completa:\n%v\nesperada:\n%v", row, wantRow)
	}
	for number, r := range byNumber {
		if number != inv["number"] && (r[2] != "open" || r[9] != "100.00" || r[14] != "100.00" || r[8] != "0.00" || r[13] != "0.00") {
			t.Errorf("linha da fatura simples: %v", r)
		}
	}

	// Com o limite de 2 faturas por arquivo, a exportação sai em 3 partes; o
	// corte é informado e a parte seguinte continua de onde a anterior parou.
	lc := limited.login(tn.Owner, &tn.ID)
	var parts [][][]string
	var seen []string
	cursor, nextPart := "", ""
	for i := 0; ; i++ {
		path := "/api/v1/invoices/export"
		if cursor != "" {
			path += "?cursor=" + url.QueryEscape(cursor) + "&part=" + nextPart
		}
		r := invGetText(lc, path)
		if r.status != http.StatusOK || r.header.Get("X-Recorvia-Export-Limit") != "2" {
			t.Fatalf("parte %d: %d %v", i+1, r.status, r.header)
		}
		if want := fmt.Sprintf(`attachment; filename="faturas-parte-%d.csv"`, i+1); r.header.Get("Content-Disposition") != want {
			t.Fatalf("parte %d: nome do arquivo %q, esperado %q", i+1, r.header.Get("Content-Disposition"), want)
		}
		nextPart = r.header.Get("X-Recorvia-Export-Next-Part")
		part := invParseCSV(t, r.text)
		if !slices.Equal(part[0], invExportHeader) {
			t.Fatalf("parte %d sem cabeçalho: %v", i+1, part)
		}
		for _, row := range part[1:] {
			seen = append(seen, row[0])
		}
		parts = append(parts, part[1:])
		cursor = r.header.Get("X-Recorvia-Export-Next-Cursor")
		truncated := r.header.Get("X-Recorvia-Export-Truncated")
		if (truncated == "true") != (cursor != "") || (truncated != "true" && truncated != "false") ||
			(cursor != "" && nextPart != strconv.Itoa(i+2)) || (cursor == "" && nextPart != "") {
			t.Fatalf("parte %d: corte %q com cursor %q", i+1, truncated, cursor)
		}
		if cursor == "" {
			break
		}
		if i > 5 {
			t.Fatal("a exportação em partes não terminou")
		}
	}
	if len(parts) != 3 || len(parts[0]) != 2 || len(parts[1]) != 2 || len(parts[2]) != 1 {
		t.Fatalf("partes: %v", parts)
	}
	wantOrder := make([]string, 0, 5)
	for _, r := range rows[1:] {
		wantOrder = append(wantOrder, r[0])
	}
	if !slices.Equal(seen, wantOrder) {
		t.Fatalf("as partes deveriam reproduzir a exportação completa, na mesma ordem:\n%v\n%v", seen, wantOrder)
	}

	// Cada parte é auditada, com o corte e a continuação; somente nomes de
	// filtros e indicadores, sem dados das faturas.
	audits, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/audit-events?action=invoices.exported", nil)
	events := objects(t, audits["data"])
	if len(events) != 4 {
		t.Fatalf("auditoria da exportação: %v", events)
	}
	wantDetails := []struct {
		continuation, truncated bool
		part                    float64
	}{{true, false, 3}, {true, true, 2}, {false, true, 1}, {false, false, 1}}
	for i, e := range events {
		d, _ := e["details"].(map[string]any)
		if e["actor_id"] != tn.Owner.ID.String() || e["resource_type"] != "invoice" || d["continuation"] != wantDetails[i].continuation ||
			d["truncated"] != wantDetails[i].truncated || d["part"] != wantDetails[i].part {
			t.Errorf("evento %d da auditoria: %v", i, e)
		}
	}

	// Filtro aplicado à exportação e cursor inválido.
	open := invParseCSV(t, invGetText(c, "/api/v1/invoices/export?status=uncollectible").text)
	if len(open) != 2 || open[1][0] != inv["number"] {
		t.Fatalf("exportação filtrada por situação: %v", open)
	}
	if r := invGetText(c, "/api/v1/invoices/export?part=0"); r.status != http.StatusUnprocessableEntity || r.body["code"] != "invalid_field" {
		t.Fatalf("parte inválida: %d %v", r.status, r.body)
	}
	if r := invGetText(c, "/api/v1/invoices/export?cursor=invalido"); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("cursor inválido: %d %s", r.status, r.text)
	}
	if r := invGetText(c, "/api/v1/invoices/export?status=atrasada"); r.status != http.StatusUnprocessableEntity || r.body["code"] != "invalid_filter" {
		t.Fatalf("filtro inválido: %d %v", r.status, r.body)
	}
}

// Fatura de 180,00 com 15,00 de crédito aplicado, cancelada: o crédito volta
// ao cliente, o saldo é zero na API, no documento e no CSV, e o documento
// informa o cancelamento e a devolução do crédito.
func TestInvoiceVoidHasNoBalanceOverHTTP(t *testing.T) {
	a := newAdvancedAPI(t)
	tn := invIssuerTenant(t, a.env, "cancelada")
	c := a.login(tn.Owner, &tn.ID)
	cust := a.env.NewCustomer(t, tn)
	custPath := "/api/v1/customers/" + cust.ID.String()
	c.expect(http.StatusCreated, http.MethodPost, custPath+"/credits", map[string]any{"amount": 1500, "reason": "Cortesia comercial"},
		"Idempotency-Key", "credito-"+uuid.NewString())
	sub := a.env.NewSubscription(t, tn, cust.ID, newPlan(t, a.env, tn, "Plano Cancelado", flatPrice(18000)).ID, nil)
	inv := issuedInvoice(c, sub.ID)
	if inv["credit_applied"] != float64(1500) || inv["amount_due"] != float64(16500) {
		t.Fatalf("fatura emitida com crédito: %v", inv)
	}
	invPath := "/api/v1/invoices/" + inv["id"].(string)
	voided, _ := c.expect(http.StatusOK, http.MethodPost, invPath+"/void", map[string]any{"reason": "Emitida em duplicidade"},
		"If-Match", invEtag(c, invPath))
	if voided["status"] != "void" || voided["amount_due"] != float64(0) || voided["credit_applied"] != float64(1500) || voided["total"] != float64(18000) {
		t.Fatalf("fatura cancelada: %v", voided)
	}
	bal, _ := c.expect(http.StatusOK, http.MethodGet, custPath+"/credits", nil)
	if bal["balance"] != float64(1500) {
		t.Fatalf("o crédito aplicado deveria voltar ao cliente: %v", bal)
	}

	r := invGetText(c, invPath+"/document")
	if r.status != http.StatusOK {
		t.Fatalf("documento: %d", r.status)
	}
	got := invDocumentTotals(t, r.text)
	want := map[string]string{"Total": "R$ 180,00", "Créditos aplicados": "-R$ 15,00", "Crédito devolvido ao cliente": "R$ 15,00",
		"Valor cancelado": "-R$ 180,00", "Saldo": "R$ 0,00"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, esperado %q (%v)", k, got[k], v, got)
		}
	}
	for _, s := range []string{"<strong>Situação:</strong> Cancelada", "O crédito aplicado de R$ 15,00 foi devolvido ao saldo de créditos do cliente.",
		"Motivo: Emitida em duplicidade"} {
		if !strings.Contains(r.text, s) {
			t.Errorf("documento sem %q", s)
		}
	}
	if strings.Contains(r.text, "R$ 165,00") {
		t.Errorf("documento da cancelada com saldo devedor:\n%s", r.text)
	}

	rows := invParseCSV(t, invGetText(c, "/api/v1/invoices/export?status=void").text)
	if len(rows) != 2 || rows[1][0] != inv["number"] || rows[1][2] != "void" || rows[1][10] != "15.00" || rows[1][14] != "0.00" {
		t.Fatalf("CSV da fatura cancelada: %v", rows)
	}
	// O painel não soma a cancelada nos recebíveis em aberto.
	dash, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/dashboard", nil)
	if dash["open_receivables"] != float64(0) {
		t.Fatalf("recebíveis em aberto com a fatura cancelada: %v", dash)
	}
}
