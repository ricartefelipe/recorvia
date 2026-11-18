package httpapi

import (
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

func TestInvoiceDocumentRendering(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Deps: Deps{Clock: clock.System{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	issued := time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC)
	// O vencimento é o fim do dia civil: 03:00 UTC de 10/11 ainda é 09/11 em São Paulo.
	due := time.Date(2026, time.November, 10, 3, 0, 0, 0, time.UTC)
	inv := billing.Invoice{Number: "ACME-2026-000123", Status: billing.Uncollectible, Currency: money.BRL, IssuedAt: &issued, DueAt: &due,
		CustomerSnapshot: customers.Snapshot{Name: "<script>alert(1)</script>", DocumentNumber: "11222333000181"},
		Subtotal:         money.New(15990, money.BRL), DiscountTotal: money.Zero(money.BRL), CreditApplied: money.Zero(money.BRL),
		AmountPaid: money.Zero(money.BRL), AmountDue: money.Zero(money.BRL),
		Items: []billing.Item{{Description: "Plano Profissional", Quantity: big.NewRat(1, 1), Amount: money.New(15990, money.BRL),
			DiscountAmount: money.Zero(money.BRL)}}}
	cases := []struct {
		name           string
		customerFacing bool
		checkout       string
		want, absent   []string
	}{
		{"painel", false, "", []string{"<strong>Situação:</strong> Incobrável", "<strong>Vencimento:</strong> 09/11/2026", "01/11/2026", "R$ 159,90"},
			[]string{"<script>", "Pagar esta fatura"}},
		{"portal", true, "https://pagamentos.exemplo.test/checkout/abc", []string{"<strong>Situação:</strong> Encerrada",
			`href="https://pagamentos.exemplo.test/checkout/abc"`}, []string{"Incobrável", "<script>"}},
		{"link inseguro neutralizado", true, "javascript:alert(1)", []string{"#ZgotmplZ"}, []string{"javascript:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			a.writeInvoiceDocument(w, httptest.NewRequest(http.MethodGet, "/", nil), invoiceDocumentData{Org: "Acme Serviços", Invoice: inv,
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
		})
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
