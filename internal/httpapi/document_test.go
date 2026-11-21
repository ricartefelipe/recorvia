package httpapi

import (
	"io"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// totalRows extrai as linhas do quadro de totais do documento.
var totalRows = regexp.MustCompile(`<tr(?: class="total")?><td>([^<]+)</td><td class="num">([^<]+)</td></tr>`)

func TestInvoiceDocumentRendering(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Deps: Deps{Clock: clock.System{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	issued := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	// O vencimento é o fim do dia civil: 03:00 UTC de 10/11 ainda é 09/11 em São Paulo.
	due := time.Date(2026, time.November, 10, 3, 0, 0, 0, time.UTC)
	// Período [01/11, 01/12) no fuso da organização: o último dia incluído é 30/11.
	period := &calendar.Period{Start: time.Date(2026, time.November, 1, 3, 0, 0, 0, time.UTC), End: time.Date(2026, time.December, 1, 3, 0, 0, 0, time.UTC)}
	brl := func(v int64) money.Money { return money.New(v, money.BRL) }
	// Desconto, crédito aplicado, nota de crédito, pagamento parcial e baixa:
	// 200,00 - 20,00 = 180,00; 180,00 - 15,00 - 30,00 - 50,00 - 85,00 = 0,00.
	inv := billing.Invoice{Number: "ACME-2026-000123", Status: billing.Uncollectible, Currency: money.BRL, IssuedAt: &issued, DueAt: &due,
		BillingPeriod: period, Memo: "Pedido de compra 4521",
		CustomerSnapshot: customers.Snapshot{Name: "<script>alert(1)</script>", DocumentType: "cnpj", DocumentNumber: "11222333000181",
			Address: &customers.Address{Street: "Avenida Paulista", Number: "1000", Complement: "Conjunto 12", District: "Bela Vista",
				City: "São Paulo", State: "SP", PostalCode: "01310100", Country: "BR"}},
		Subtotal: brl(20000), DiscountTotal: brl(2000), Total: brl(18000), CreditApplied: brl(1500), AmountCredited: brl(3000),
		AmountPaid: brl(5000), AmountWrittenOff: brl(8500), AmountDue: brl(0),
		Items: []billing.Item{{Description: "Plano Profissional", Quantity: big.NewRat(1, 1), Amount: brl(20000), DiscountAmount: brl(2000),
			Period: period}}}
	issuer := DocumentIssuer{Name: "Acme Serviços", LegalName: "Acme Serviços Ltda.", Document: "11444777000161"}
	common := []string{documentFiscalNotice, "<strong>Vencimento:</strong> 09/11/2026", "<strong>Emissão:</strong> 01/11/2026",
		"Acme Serviços Ltda.", "<strong>CNPJ do emitente:</strong> 11.444.777/0001-61", "<strong>CNPJ:</strong> 11.222.333/0001-81",
		"<strong>Endereço de cobrança:</strong> Avenida Paulista, 1000, Conjunto 12 - Bela Vista - São Paulo/SP - CEP 01310-100",
		"<strong>Período de faturamento:</strong> 01/11/2026 a 30/11/2026", "<td>01/11/2026 a 30/11/2026</td>",
		"<strong>Condições de pagamento:</strong> Pagamento até 09/11/2026 (8 dias após a emissão).", "Pedido de compra 4521"}
	cases := []struct {
		name           string
		customerFacing bool
		checkout       string
		want, absent   []string
		totals         map[string]string
	}{
		{"painel", false, "", append([]string{"<strong>Situação:</strong> Incobrável"}, common...), []string{"<script>", "Pagar esta fatura"},
			map[string]string{"Valor baixado": "-R$ 85,00"}},
		{"portal", true, "https://pagamentos.exemplo.test/checkout/abc", append([]string{"<strong>Situação:</strong> Encerrada",
			`href="https://pagamentos.exemplo.test/checkout/abc"`}, common...), []string{"Incobrável", "<script>", "Valor baixado"},
			map[string]string{"Saldo encerrado pela organização": "-R$ 85,00"}},
		{"link inseguro neutralizado", true, "javascript:alert(1)", []string{"#ZgotmplZ"}, []string{"javascript:"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.writeInvoiceDocument(w, httptest.NewRequest(http.MethodGet, "/", nil), invoiceDocumentData{Issuer: issuer, Invoice: inv,
				Location: loc, CheckoutURL: tc.checkout, CustomerFacing: tc.customerFacing})
			body := w.Body.String()
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("Content-Type: %q", ct)
			}
			if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
				t.Fatalf("CSP: %q", csp)
			}
			for _, s := range tc.want {
				if !strings.Contains(body, s) {
					t.Errorf("documento sem %q:\n%s", s, body)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(body, s) {
					t.Errorf("documento não deveria conter %q:\n%s", s, body)
				}
			}
			if tc.totals == nil {
				return
			}
			got := map[string]string{}
			var order []string
			for _, m := range totalRows.FindAllStringSubmatch(body, -1) {
				got[m[1]] = m[2]
				order = append(order, m[1])
			}
			want := map[string]string{"Subtotal": "R$ 200,00", "Descontos": "-R$ 20,00", "Total": "R$ 180,00", "Créditos aplicados": "-R$ 15,00",
				"Notas de crédito": "-R$ 30,00", "Pago": "-R$ 50,00", "Saldo": "R$ 0,00"}
			for k, v := range tc.totals {
				want[k] = v
			}
			if len(got) != len(want) || order[2] != "Total" || order[len(order)-1] != "Saldo" {
				t.Fatalf("quadro de totais: %v (ordem %v)", got, order)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("total %q: %q, esperado %q", k, got[k], v)
				}
			}
		})
	}
}

// checkTotalsClose confere que o quadro fecha a conta: subtotal menos
// descontos dá o total, e o total mais os ajustes seguintes (créditos,
// notas, pagamentos, baixa e, na cancelada, a devolução do crédito e o valor
// anulado) dá o saldo da fatura.
func checkTotalsClose(t *testing.T, inv billing.Invoice, customerFacing bool) {
	t.Helper()
	rows := documentTotals(inv, customerFacing)
	if rows[2].Label != "Total" || rows[len(rows)-1].Label != "Saldo" {
		t.Fatalf("ordem do quadro: %+v", rows)
	}
	if rows[0].Amount+rows[1].Amount != rows[2].Amount || rows[2].Amount != inv.Total.Amount() {
		t.Fatalf("subtotal e descontos não fecham com o total: %+v", rows)
	}
	due := rows[2].Amount
	for _, r := range rows[3 : len(rows)-1] {
		due += r.Amount
	}
	if due != rows[len(rows)-1].Amount || due != inv.AmountDue.Amount() {
		t.Fatalf("o quadro não fecha com o saldo (%s, saldo %v): %+v", inv.Status, inv.AmountDue, rows)
	}
}

// O quadro fecha em combinações reais produzidas pelo domínio do
// faturamento: emissão com desconto e crédito, pagamentos parciais e
// excedentes, notas de crédito, baixa, recuperação da baixa e cancelamento,
// em sequências fixas e em sequências pseudoaleatórias reproduzíveis.
func TestDocumentTotalsAddUp(t *testing.T) {
	at := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	brl := func(v int64) money.Money { return money.New(v, money.BRL) }
	newIssued := func(t *testing.T, amount, discount, credit int64) *billing.Invoice {
		t.Helper()
		inv := billing.NewDraft(uuid.New(), uuid.New(), uuid.New(), nil, billing.KindManual, money.BRL, nil, customers.Snapshot{Name: "Cliente"}, at)
		if err := inv.AddItem(billing.Item{Description: "Plano", Quantity: big.NewRat(1, 1), UnitAmount: big.NewRat(amount, 1), Amount: brl(amount),
			DiscountAmount: brl(discount)}); err != nil {
			t.Fatal(err)
		}
		if _, err := inv.ApplyCredit(brl(credit)); err != nil {
			t.Fatal(err)
		}
		if err := inv.Issue("FAT-1", at, at.Add(5*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		return inv
	}
	type step func(inv *billing.Invoice) error
	pay := func(v int64) step {
		return func(inv *billing.Invoice) error { _, _, err := inv.ApplyPayment(brl(v), at); return err }
	}
	note := func(v int64) step { return func(inv *billing.Invoice) error { return inv.ApplyCreditNote(brl(v), at) } }
	writeOff := func(inv *billing.Invoice) error { _, err := inv.MarkUncollectible("baixa", at); return err }
	void := func(inv *billing.Invoice) error { return inv.Void("emitida em duplicidade", at) }
	cases := []struct {
		name                     string
		amount, discount, credit int64
		steps                    []step
		status                   billing.Status
	}{
		{"em aberto", 9990, 0, 0, nil, billing.Open},
		{"desconto, crédito, nota e pagamento parcial", 20000, 2000, 1500, []step{note(3000), pay(5000)}, billing.PartiallyPaid},
		{"baixada depois de nota e pagamento", 20000, 2000, 1500, []step{note(3000), pay(5000), writeOff}, billing.Uncollectible},
		{"baixa recuperada em parte", 20000, 0, 0, []step{pay(2000), writeOff, pay(3000)}, billing.Uncollectible},
		{"baixa recuperada por inteiro", 20000, 0, 0, []step{writeOff, pay(20000)}, billing.Paid},
		{"paga com excedente", 10000, 500, 0, []step{pay(20000)}, billing.Paid},
		{"quitada por nota de crédito", 10000, 0, 2500, []step{note(7500)}, billing.Paid},
		{"cancelada", 18000, 0, 0, []step{void}, billing.Void},
		{"cancelada com crédito aplicado", 18000, 0, 1500, []step{void}, billing.Void},
		{"cancelada com desconto e crédito", 20000, 2000, 1500, []step{void}, billing.Void},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := newIssued(t, tc.amount, tc.discount, tc.credit)
			for i, st := range tc.steps {
				if err := st(inv); err != nil {
					t.Fatalf("passo %d: %v", i, err)
				}
			}
			if inv.Status != tc.status {
				t.Fatalf("situação %s, esperada %s", inv.Status, tc.status)
			}
			for _, customerFacing := range []bool{false, true} {
				checkTotalsClose(t, *inv, customerFacing)
			}
		})
	}
	// Sequências pseudoaleatórias (semente fixa) de operações do domínio; as
	// operações recusadas na situação corrente são ignoradas.
	rng := rand.New(rand.NewPCG(79, 2026))
	for i := range 300 {
		total := 1000 + rng.Int64N(50000)
		inv := newIssued(t, total, rng.Int64N(total/2), rng.Int64N(total))
		for range 1 + rng.IntN(6) {
			v := 1 + rng.Int64N(total)
			switch rng.IntN(4) {
			case 0:
				_ = pay(v)(inv)
			case 1:
				_ = note(v)(inv)
			case 2:
				_ = writeOff(inv)
			case 3:
				_ = void(inv)
			}
			checkTotalsClose(t, *inv, i%2 == 0)
		}
	}
}

func TestDocumentFormatting(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) *calendar.Date { v := calendar.Date{Year: 2026, Month: time.November, Day: d}; return &v }
	start := time.Date(2026, time.November, 5, 3, 0, 0, 0, time.UTC)
	cases := []struct{ name, got, want string }{
		{"CPF", formatDocument("cpf", "52998224725"), "529.982.247-25"},
		{"CNPJ", formatDocument("cnpj", "11222333000181"), "11.222.333/0001-81"},
		{"documento fora do formato", formatDocument("cpf", "anonimizado"), "anonimizado"},
		{"sem endereço", formatAddress(nil), ""},
		{"endereço sem complemento, exterior", formatAddress(&customers.Address{Street: "Rua A", Number: "1", District: "Centro", City: "Lisboa",
			State: "LX", PostalCode: "12345678", Country: "PT"}), "Rua A, 1 - Centro - Lisboa/LX - CEP 12345-678 - PT"},
		{"período de um dia", formatPeriod(&calendar.Period{Start: start, End: start.Add(24 * time.Hour)}, loc), "05/11/2026"},
		{"período vazio", formatPeriod(&calendar.Period{Start: start, End: start}, loc), "05/11/2026"},
		{"sem período", formatPeriod(nil, loc), ""},
		{"sem vencimento", documentTerms(day(1), nil), "-"},
		{"à vista", documentTerms(day(9), day(9)), "Pagamento à vista, até 09/11/2026."},
		{"um dia", documentTerms(day(8), day(9)), "Pagamento até 09/11/2026 (1 dia após a emissão)."},
		{"sem emissão", documentTerms(nil, day(9)), "Pagamento até 09/11/2026."},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: %q, esperado %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestDocumentStatus(t *testing.T) {
	if got := documentStatus(billing.Status("desconhecida"), false); got != "desconhecida" {
		t.Fatalf("situação desconhecida: %q", got)
	}
	if got := documentStatus(billing.Paid, true); got != "Paga" {
		t.Fatalf("situação paga: %q", got)
	}
}

// Fatura cancelada com crédito aplicado: o documento mostra o cancelamento,
// a devolução do crédito e saldo zero; o motivo aparece somente no painel. O
// emitente vem dos dados gravados na emissão, não dos dados atuais.
func TestInvoiceDocumentVoidAndIssuerSnapshot(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Deps: Deps{Clock: clock.System{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	at := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	brl := func(v int64) money.Money { return money.New(v, money.BRL) }
	inv := billing.NewDraft(uuid.New(), uuid.New(), uuid.New(), nil, billing.KindManual, money.BRL, nil, customers.Snapshot{Name: "Cliente"}, at)
	if err := inv.AddItem(billing.Item{Description: "Plano", Quantity: big.NewRat(1, 1), UnitAmount: big.NewRat(18000, 1), Amount: brl(18000),
		DiscountAmount: brl(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := inv.ApplyCredit(brl(1500)); err != nil {
		t.Fatal(err)
	}
	if err := inv.Issue("ACME-2026-000200", at, at.Add(5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := inv.Void("Emitida em duplicidade", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	inv.Issuer = &billing.Issuer{Name: "Acme na emissão", LegalName: "Acme Antiga Ltda.", DocumentNumber: "11444777000161"}
	current := DocumentIssuer{Name: "Acme hoje", LegalName: "Acme Nova S.A.", Document: "11222333000181"}
	for _, customerFacing := range []bool{false, true} {
		w := httptest.NewRecorder()
		a.writeInvoiceDocument(w, httptest.NewRequest(http.MethodGet, "/", nil), invoiceDocumentData{Issuer: current, Invoice: *inv, Location: loc,
			CustomerFacing: customerFacing})
		body := w.Body.String()
		got := map[string]string{}
		for _, m := range totalRows.FindAllStringSubmatch(body, -1) {
			got[m[1]] = m[2]
		}
		want := map[string]string{"Total": "R$ 180,00", "Créditos aplicados": "-R$ 15,00", "Crédito devolvido ao cliente": "R$ 15,00",
			"Valor cancelado": "-R$ 180,00", "Saldo": "R$ 0,00"}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("cliente=%v: %s = %q, esperado %q (%v)", customerFacing, k, got[k], v, got)
			}
		}
		for _, s := range []string{"<strong>Situação:</strong> Cancelada", "Fatura cancelada em 01/11/2026; não há valor a pagar.",
			"O crédito aplicado de R$ 15,00 foi devolvido ao saldo de créditos do cliente.", "<strong>Condições de pagamento:</strong> Fatura cancelada.",
			"Acme na emissão (Acme Antiga Ltda.)", "11.444.777/0001-61"} {
			if !strings.Contains(body, s) {
				t.Errorf("cliente=%v: documento sem %q", customerFacing, s)
			}
		}
		for _, s := range []string{"Acme hoje", "Acme Nova", "11.222.333/0001-81", "R$ 165,00"} {
			if strings.Contains(body, s) {
				t.Errorf("cliente=%v: documento não deveria conter %q", customerFacing, s)
			}
		}
		if reason := strings.Contains(body, "Motivo: Emitida em duplicidade"); reason == customerFacing {
			t.Errorf("cliente=%v: motivo do cancelamento exibido=%v", customerFacing, reason)
		}
	}
	// Sem emitente gravado (fatura anterior à ADR-079), valem os dados atuais.
	inv.Issuer = nil
	if got := documentIssuer(*inv, current); got != current {
		t.Fatalf("emitente de fatura antiga: %+v", got)
	}
}

// O endereço do emitente sai no documento quando foi gravado na emissão. A
// fatura emitida antes do endereço da organização (emitente gravado sem
// endereço) e a anterior à ADR-079 (sem emitente gravado) não ganham o
// endereço atual da organização, e o documento continua completo.
func TestInvoiceDocumentIssuerAddress(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Deps: Deps{Clock: clock.System{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	at := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	brl := func(v int64) money.Money { return money.New(v, money.BRL) }
	inv := billing.NewDraft(uuid.New(), uuid.New(), uuid.New(), nil, billing.KindManual, money.BRL, nil, customers.Snapshot{Name: "Cliente"}, at)
	if err := inv.AddItem(billing.Item{Description: "Plano", Quantity: big.NewRat(1, 1), UnitAmount: big.NewRat(10000, 1), Amount: brl(10000),
		DiscountAmount: brl(0)}); err != nil {
		t.Fatal(err)
	}
	if err := inv.Issue("ACME-2026-000300", at, at.Add(5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// O endereço atual da organização, diferente do gravado na emissão.
	current := DocumentIssuer{Name: "Acme hoje", Document: "11444777000161", Address: "Rua Atual, 9 - Centro - Campinas/SP - CEP 13010-000"}
	const label = "<strong>Endereço do emitente:</strong>"
	cases := []struct {
		name   string
		issuer *billing.Issuer
		want   string
	}{
		{"gravado na emissão", &billing.Issuer{Name: "Acme", DocumentNumber: "11444777000161", Address: &customers.Address{Street: "Av. Paulista",
			Number: "1000", Complement: "10º andar", District: "Bela Vista", City: "São Paulo", State: "SP", PostalCode: "01310100", Country: "BR"}},
			label + " Av. Paulista, 1000, 10º andar - Bela Vista - São Paulo/SP - CEP 01310-100"},
		{"emitente gravado sem endereço", &billing.Issuer{Name: "Acme", DocumentNumber: "11444777000161"}, ""},
		{"sem emitente gravado", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := *inv
			doc.Issuer = tc.issuer
			w := httptest.NewRecorder()
			a.writeInvoiceDocument(w, httptest.NewRequest(http.MethodGet, "/", nil), invoiceDocumentData{Issuer: current, Invoice: doc, Location: loc})
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, documentFiscalNotice) || !strings.Contains(body, "</html>") {
				t.Fatalf("documento incompleto (%d):\n%s", w.Code, body)
			}
			if tc.want != "" && !strings.Contains(body, tc.want) {
				t.Errorf("documento sem %q:\n%s", tc.want, body)
			}
			if tc.want == "" && strings.Contains(body, label) {
				t.Errorf("documento não deveria ter endereço do emitente:\n%s", body)
			}
			if strings.Contains(body, "Rua Atual") {
				t.Errorf("o endereço atual da organização não substitui o da emissão:\n%s", body)
			}
		})
	}
}
