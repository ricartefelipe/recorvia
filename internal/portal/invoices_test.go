package portal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// addInvoice inclui uma fatura no fim da ordem de listagem do tenant.
func (e *env) addInvoice(tenantID, customerID uuid.UUID, status billing.Status, total, due int64, dueAt *time.Time) billing.Invoice {
	inv := billing.Invoice{ID: uuid.New(), TenantID: tenantID, CustomerID: customerID, Number: "ACME-" + uuid.NewString()[:6], Status: status,
		Currency: money.BRL, Total: money.New(total, money.BRL), AmountDue: money.New(due, money.BRL), DueAt: dueAt,
		Items: []billing.Item{{Description: "Plano Profissional", Amount: money.New(total+500, money.BRL), DiscountAmount: money.New(500, money.BRL)}}}
	d := e.store.tenants[tenantID]
	d.invoices = append(d.invoices, inv)
	return inv
}

// portalSession abre uma sessão do portal para o cliente padrão.
func (e *env) portalSession(t *testing.T) Session {
	t.Helper()
	_, token := e.link(t)
	return e.exchange(t, token)
}

func TestSafeCheckoutURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://pagamentos.exemplo.test/checkout/abc", true},
		{"http://localhost:8090/checkout/abc", true},
		{"javascript:alert(1)", false},
		{"data:text/html,<p>x</p>", false},
		{"//pagamentos.exemplo.test/checkout", false},
		{"/checkout/relativo", false},
		{"https://usuario:senha@pagamentos.exemplo.test/", false},
		{"https://%zz", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := safeCheckoutURL(tc.url); got != tc.want {
				t.Fatalf("safeCheckoutURL(%q) = %v, esperado %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestInvoicesListOnlyIssuedInvoicesOfSessionCustomer(t *testing.T) {
	e := newEnv(t)
	e.d().tenant.Timezone = "America/Sao_Paulo"
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	// 03:00 UTC de 10/11 ainda é 09/11 em São Paulo; o vencimento é o fim do dia civil.
	dueAt := time.Date(2026, time.November, 10, 3, 0, 0, 0, time.UTC)
	future := testNow.Add(72 * time.Hour)
	overdue := e.addInvoice(e.tenantID, e.customer.ID, billing.Open, 15990, 15990, &dueAt)
	paid := e.addInvoice(e.tenantID, e.customer.ID, billing.Paid, 9900, 0, &dueAt)
	e.addInvoice(e.tenantID, e.customer.ID, billing.Draft, 100, 100, nil)
	e.addInvoice(e.tenantID, other.ID, billing.Open, 777, 777, &future)
	unsafe := e.addInvoice(e.tenantID, e.customer.ID, billing.PartiallyPaid, 5000, 2500, &future)
	e.d().checkouts[overdue.ID] = "https://pagamentos.exemplo.test/checkout/abc"
	e.d().checkouts[paid.ID] = "https://pagamentos.exemplo.test/checkout/antigo"
	e.d().checkouts[unsafe.ID] = "javascript:alert(1)"
	s := e.portalSession(t)

	for _, leaky := range []bool{false, true} {
		e.store.leaky = leaky
		got, err := e.svc.Invoices(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0].ID != overdue.ID || got[1].ID != paid.ID || got[2].ID != unsafe.ID {
			t.Fatalf("leaky=%v: faturas fora do escopo da sessão: %+v", leaky, got)
		}
		if !got[0].Overdue || got[0].DueDate == nil || got[0].DueDate.String() != "2026-11-09" || got[0].CheckoutURL != e.d().checkouts[overdue.ID] {
			t.Fatalf("fatura vencida: %+v", got[0])
		}
		if got[1].Overdue || got[1].CheckoutURL != "" {
			t.Fatalf("fatura paga não tem pagamento pelo portal: %+v", got[1])
		}
		if got[2].Overdue || got[2].CheckoutURL != "" {
			t.Fatalf("link de pagamento inseguro exposto: %+v", got[2])
		}
	}
	e.store.leaky = false

	// Cliente sem faturas recebe lista vazia (não nula).
	empty, err := e.svc.Invoices(context.Background(), Session{TenantID: e.tenantID, CustomerID: uuid.New()})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("lista vazia: %v %v", empty, err)
	}
}

func TestInvoicesListLimit(t *testing.T) {
	e := newEnv(t)
	for range MaxInvoices + 5 {
		e.addInvoice(e.tenantID, e.customer.ID, billing.Paid, 100, 0, nil)
	}
	got, err := e.svc.Invoices(context.Background(), e.portalSession(t))
	if err != nil || len(got) != MaxInvoices {
		t.Fatalf("limite da listagem: %d %v", len(got), err)
	}
}

func TestInvoiceDetailRestrictedToSessionCustomer(t *testing.T) {
	e := newEnv(t)
	e.d().tenant.Timezone = "America/Sao_Paulo"
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	due := testNow.Add(24 * time.Hour)
	own := e.addInvoice(e.tenantID, e.customer.ID, billing.Open, 15990, 15990, &due)
	draft := e.addInvoice(e.tenantID, e.customer.ID, billing.Draft, 100, 100, nil)
	foreign := e.addInvoice(e.tenantID, other.ID, billing.Open, 777, 777, &due)
	e.d().checkouts[own.ID] = "https://pagamentos.exemplo.test/checkout/abc"
	otherTenant := e.addTenant("Beta Consultoria")
	foreignTenant := e.addInvoice(otherTenant, e.addCustomer(otherTenant, "Cliente Beta").ID, billing.Open, 1, 1, &due)
	s := e.portalSession(t)

	d, err := e.svc.Invoice(context.Background(), s, own.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != own.ID || d.OrganizationName != "Acme Serviços" || d.Location.String() != "America/Sao_Paulo" || d.CheckoutURL == "" || d.Overdue {
		t.Fatalf("detalhe: %+v", d)
	}
	for _, leaky := range []bool{false, true} {
		e.store.leaky = leaky
		for name, id := range map[string]uuid.UUID{"rascunho": draft.ID, "outro cliente": foreign.ID, "outro tenant": foreignTenant.ID, "inexistente": uuid.New()} {
			t.Run(name, func(t *testing.T) {
				_, err := e.svc.Invoice(context.Background(), s, id)
				assertErr(t, err, apperr.KindNotFound, "invoice_not_found")
			})
		}
	}
}

func TestInvoiceLines(t *testing.T) {
	inv := Invoice{Invoice: billing.Invoice{Items: []billing.Item{
		{Description: "Plano", Amount: money.New(20000, money.BRL), DiscountAmount: money.New(2000, money.BRL)},
		{Description: "Consumo", Amount: money.New(350, money.BRL), DiscountAmount: money.Zero(money.BRL)},
	}}}
	lines, err := inv.Lines()
	if err != nil || len(lines) != 2 || lines[0].Amount.Amount() != 18000 || lines[1].Amount.Amount() != 350 || lines[0].Description != "Plano" {
		t.Fatalf("linhas: %+v %v", lines, err)
	}
	bad := Invoice{Invoice: billing.Invoice{Items: []billing.Item{{Amount: money.New(1, money.BRL), DiscountAmount: money.New(1, money.Currency{})}}}}
	if _, err := bad.Lines(); err == nil {
		t.Fatal("moedas divergentes deveriam falhar")
	}
}

func TestInvoiceQueriesPropagateFailures(t *testing.T) {
	e := newEnv(t)
	due := testNow.Add(24 * time.Hour)
	inv := e.addInvoice(e.tenantID, e.customer.ID, billing.Open, 100, 100, &due)
	s := e.portalSession(t)
	for _, op := range []string{"Tenant", "Invoices", "CheckoutURLs"} {
		e.store.fail[op] = errBoom
		if _, err := e.svc.Invoices(context.Background(), s); !errors.Is(err, errBoom) {
			t.Fatalf("listagem com falha em %s: %v", op, err)
		}
		e.store.fail[op] = nil
	}
	for _, op := range []string{"Tenant", "Invoice", "CheckoutURLs"} {
		e.store.fail[op] = errBoom
		if _, err := e.svc.Invoice(context.Background(), s, inv.ID); !errors.Is(err, errBoom) {
			t.Fatalf("detalhe com falha em %s: %v", op, err)
		}
		e.store.fail[op] = nil
	}
	e.d().tenant.Timezone = "Fuso/Inexistente"
	if _, err := e.svc.Invoices(context.Background(), s); err == nil {
		t.Fatal("fuso inválido deveria falhar")
	}
	if _, err := e.svc.Invoice(context.Background(), s, inv.ID); err == nil {
		t.Fatal("fuso inválido deveria falhar no detalhe")
	}
}

func TestUpdateContactPreferences(t *testing.T) {
	e := newEnv(t)
	fin := customers.Contact{ID: uuid.New(), CustomerID: e.customer.ID, Name: "Financeiro", Email: "fin@cliente.test", ReceivesBillingEmail: true}
	ops := customers.Contact{ID: uuid.New(), CustomerID: e.customer.ID, Name: "Operações", Email: "ops@cliente.test", ReceivesBillingEmail: false}
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	foreign := customers.Contact{ID: uuid.New(), CustomerID: other.ID, Name: "Alheio", Email: "alheio@cliente.test", ReceivesBillingEmail: true}
	e.d().contacts[e.customer.ID] = []customers.Contact{fin, ops}
	e.d().contacts[other.ID] = []customers.Contact{foreign}
	s := e.portalSession(t)
	audits := len(e.d().audits)

	v, err := e.svc.UpdateContactPreferences(context.Background(), s, []ContactPreference{
		{ContactID: fin.ID, ReceivesBillingEmail: false}, {ContactID: ops.ID, ReceivesBillingEmail: false}})
	if err != nil {
		t.Fatal(err)
	}
	if v.CustomerName != "Cliente Exemplo" || v.OrganizationName != "Acme Serviços" || len(v.Contacts) != 2 || v.Contacts[0].ReceivesBillingEmail ||
		v.Contacts[1].ReceivesBillingEmail || v.Session.ID != s.ID {
		t.Fatalf("resumo após a alteração: %+v", v)
	}
	if got := e.d().contacts[e.customer.ID]; got[0].ReceivesBillingEmail || got[1].ReceivesBillingEmail {
		t.Fatalf("preferências não gravadas: %+v", got)
	}
	if len(e.d().audits) != audits+1 {
		t.Fatalf("esperava uma entrada de auditoria, obtive %d", len(e.d().audits)-audits)
	}
	a := e.d().audits[len(e.d().audits)-1]
	changes, _ := a.Details["changes"].([]map[string]any)
	if a.Action != "customer.contact_preferences_updated" || a.Actor.Type != audit.ActorCustomerPortal || a.ResourceID != e.customer.ID.String() ||
		len(changes) != 1 || changes[0]["contact_id"] != fin.ID {
		t.Fatalf("auditoria registra apenas o que mudou: %+v", a)
	}
	if e.store.calls["LockCustomer"] == 0 {
		t.Fatal("a alteração deve bloquear o cliente")
	}

	// Sem mudança efetiva, nada é gravado nem auditado.
	writes := e.store.calls["SetContactPreference"]
	if _, err := e.svc.UpdateContactPreferences(context.Background(), s, []ContactPreference{{ContactID: fin.ID, ReceivesBillingEmail: false}}); err != nil {
		t.Fatal(err)
	}
	if e.store.calls["SetContactPreference"] != writes || len(e.d().audits) != audits+1 {
		t.Fatal("alteração sem efeito não deveria gravar nem auditar")
	}
	if v, err := e.svc.UpdateContactPreferences(context.Background(), s, nil); err != nil || len(v.Contacts) != 2 {
		t.Fatalf("lista vazia mantém as preferências: %+v %v", v, err)
	}

	// Contato de outro cliente: inexistente, e nada da requisição é aplicado.
	_, err = e.svc.UpdateContactPreferences(context.Background(), s, []ContactPreference{
		{ContactID: fin.ID, ReceivesBillingEmail: true}, {ContactID: foreign.ID, ReceivesBillingEmail: false}})
	assertErr(t, err, apperr.KindNotFound, "contact_not_found")
	if e.d().contacts[e.customer.ID][0].ReceivesBillingEmail || !e.d().contacts[other.ID][0].ReceivesBillingEmail {
		t.Fatal("alteração parcial persistida")
	}
}

func TestUpdateContactPreferencesValidation(t *testing.T) {
	e := newEnv(t)
	s := e.portalSession(t)
	id := uuid.New()
	tooMany := make([]ContactPreference, MaxContactPreferences+1)
	for i := range tooMany {
		tooMany[i] = ContactPreference{ContactID: uuid.New()}
	}
	cases := map[string][]ContactPreference{
		"acima do limite":    tooMany,
		"contato repetido":   {{ContactID: id}, {ContactID: id, ReceivesBillingEmail: true}},
		"identificador nulo": {{ContactID: uuid.Nil}},
	}
	for name, prefs := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.svc.UpdateContactPreferences(context.Background(), s, prefs)
			assertErr(t, err, apperr.KindInvalid, "invalid_contact_preferences")
		})
	}
}

func TestUpdateContactPreferencesFailures(t *testing.T) {
	e := newEnv(t)
	fin := customers.Contact{ID: uuid.New(), CustomerID: e.customer.ID, Name: "Financeiro", Email: "fin@cliente.test", ReceivesBillingEmail: true}
	e.d().contacts[e.customer.ID] = []customers.Contact{fin}
	s := e.portalSession(t)
	prefs := []ContactPreference{{ContactID: fin.ID, ReceivesBillingEmail: false}}
	for _, op := range []string{"Tenant", "LockCustomer", "Contacts", "SetContactPreference", "Audit"} {
		e.store.fail[op] = errBoom
		if _, err := e.svc.UpdateContactPreferences(context.Background(), s, prefs); !errors.Is(err, errBoom) {
			t.Fatalf("falha em %s: %v", op, err)
		}
		e.store.fail[op] = nil
	}
	if !e.d().contacts[e.customer.ID][0].ReceivesBillingEmail {
		t.Fatal("falhas não devem persistir alterações")
	}
	c := e.d().customers[e.customer.ID]
	c.Status = customers.Archived
	e.d().customers[c.ID] = c
	_, err := e.svc.UpdateContactPreferences(context.Background(), s, prefs)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")
}

// FuzzSafeCheckoutURL garante que somente endereços que começam por
// http:// ou https:// chegam ao cliente como destino de navegação.
func FuzzSafeCheckoutURL(f *testing.F) {
	for _, s := range []string{"https://pagamentos.exemplo.test/checkout/abc", "http://localhost:8090/c/1", "javascript:alert(1)",
		"JaVaScRiPt:alert(1)", " https://x.test", "//x.test", "https://u:p@x.test", "data:text/html,x", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if !safeCheckoutURL(raw) {
			return
		}
		lower := strings.ToLower(raw)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			t.Fatalf("endereço sem esquema http(s) aceito: %q", raw)
		}
	})
}
