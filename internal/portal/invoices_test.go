package portal

import (
	"context"
	"errors"
	"slices"
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

// addInvoice inclui uma fatura no fim da ordem de listagem do tenant: cada
// fatura emitida recebe uma emissão um minuto anterior à da última incluída
// (rascunhos não têm emissão).
func (e *env) addInvoice(tenantID, customerID uuid.UUID, status billing.Status, total, due int64, dueAt *time.Time) billing.Invoice {
	d := e.store.tenants[tenantID]
	inv := billing.Invoice{ID: uuid.New(), TenantID: tenantID, CustomerID: customerID, Number: "ACME-" + uuid.NewString()[:6], Status: status,
		Currency: money.BRL, Total: money.New(total, money.BRL), AmountDue: money.New(due, money.BRL), DueAt: dueAt,
		Items: []billing.Item{{Description: "Plano Profissional", Amount: money.New(total+500, money.BRL), DiscountAmount: money.New(500, money.BRL)}}}
	if status != billing.Draft {
		at := testNow.Add(-time.Duration(len(d.invoices)+1) * time.Minute)
		inv.IssuedAt = &at
	}
	d.invoices = append(d.invoices, inv)
	return inv
}

// issueAt inclui uma fatura emitida do cliente padrão no instante indicado.
func (e *env) issueAt(at time.Time) billing.Invoice {
	inv := e.addInvoice(e.tenantID, e.customer.ID, billing.Open, 1000, 1000, nil)
	d := e.d()
	d.invoices[len(d.invoices)-1].IssuedAt = &at
	inv.IssuedAt = &at
	return inv
}

// invoicePages percorre a listagem até o fim e devolve os identificadores
// na ordem recebida.
func (e *env) invoicePages(t *testing.T, s Session, limit int) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	q := PageQuery{Limit: limit}
	for range 1000 {
		page, err := e.svc.Invoices(context.Background(), s, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > limit {
			t.Fatalf("página com %d itens excede o limite %d", len(page.Items), limit)
		}
		for _, inv := range page.Items {
			ids = append(ids, inv.ID)
		}
		if page.NextCursor == "" {
			return ids
		}
		q.Cursor = page.NextCursor
	}
	t.Fatal("a paginação não termina")
	return nil
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
		page, err := e.svc.Invoices(context.Background(), s, PageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		got := page.Items
		if page.NextCursor != "" {
			t.Fatalf("leaky=%v: página única não tem cursor seguinte: %q", leaky, page.NextCursor)
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
	empty, err := e.svc.Invoices(context.Background(), Session{TenantID: e.tenantID, CustomerID: uuid.New()}, PageQuery{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("lista vazia: %+v %v", empty, err)
	}
}

func TestInvoicesPageSizeLimits(t *testing.T) {
	e := newEnv(t)
	for range MaxPageSize + 5 {
		e.addInvoice(e.tenantID, e.customer.ID, billing.Paid, 100, 0, nil)
	}
	s := e.portalSession(t)
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"padrão", 0, DefaultPageSize},
		{"negativo usa o padrão", -3, DefaultPageSize},
		{"informado", 7, 7},
		{"máximo", MaxPageSize, MaxPageSize},
		{"acima do máximo", MaxPageSize + 50, MaxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := e.svc.Invoices(context.Background(), s, PageQuery{Limit: tc.limit})
			if err != nil || len(page.Items) != tc.want || page.NextCursor == "" {
				t.Fatalf("página: %d itens, cursor %q, erro %v", len(page.Items), page.NextCursor, err)
			}
		})
	}
}

// A paginação percorre todas as faturas uma única vez, na ordem da emissão
// (e do identificador, no empate), com qualquer tamanho de página.
func TestInvoicesPaginationCoversEachInvoiceOnce(t *testing.T) {
	e := newEnv(t)
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	var want []uuid.UUID
	for i := range 11 {
		// Emissões repetidas de duas em duas exercitam o desempate pelo identificador.
		inv := e.issueAt(testNow.Add(-time.Duration(i/2) * time.Hour))
		want = append(want, inv.ID)
		e.addInvoice(e.tenantID, other.ID, billing.Open, 777, 777, nil)
		e.addInvoice(e.tenantID, e.customer.ID, billing.Draft, 100, 100, nil)
	}
	sortByPosition(e.d().invoices, want)
	s := e.portalSession(t)
	for _, limit := range []int{1, 2, 3, 5, 11, 12, 100} {
		got := e.invoicePages(t, s, limit)
		if !slices.Equal(got, want) {
			t.Fatalf("limite %d: páginas %v, esperado %v", limit, got, want)
		}
	}
	// Página exata: sem item além do limite, não há cursor seguinte.
	page, err := e.svc.Invoices(context.Background(), s, PageQuery{Limit: len(want)})
	if err != nil || len(page.Items) != len(want) || page.NextCursor != "" {
		t.Fatalf("página exata: %d itens, cursor %q, erro %v", len(page.Items), page.NextCursor, err)
	}
}

// sortByPosition ordena ids pela ordem da listagem (emissão e identificador
// decrescentes) das faturas correspondentes.
func sortByPosition(invoices []billing.Invoice, ids []uuid.UUID) {
	byID := make(map[uuid.UUID]billing.Invoice, len(invoices))
	for _, inv := range invoices {
		byID[inv.ID] = inv
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		if before(issuedAt(byID[b]), b, issuedAt(byID[a]), a) {
			return -1
		}
		return 1
	})
}

// Faturas emitidas durante a navegação não deslocam as páginas seguintes:
// nada é repetido nem pulado, e a fatura nova aparece ao recomeçar.
func TestInvoicesPaginationIsStableUnderInserts(t *testing.T) {
	e := newEnv(t)
	var want []uuid.UUID
	for i := range 6 {
		want = append(want, e.issueAt(testNow.Add(-time.Duration(i+1)*time.Hour)).ID)
	}
	s := e.portalSession(t)
	first, err := e.svc.Invoices(context.Background(), s, PageQuery{Limit: 2})
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("primeira página: %+v %v", first, err)
	}
	// Entre as páginas: uma fatura mais nova que todas e outra emitida no
	// mesmo instante da última já entregue, com identificador maior (viria
	// antes dela na ordem).
	newest := e.issueAt(testNow)
	tie := e.issueAt(*first.Items[1].IssuedAt)
	d := e.d()
	maxID := uuid.Max
	d.invoices[len(d.invoices)-1].ID = maxID
	tie.ID = maxID
	got := []uuid.UUID{first.Items[0].ID, first.Items[1].ID}
	q := PageQuery{Limit: 2, Cursor: first.NextCursor}
	for q.Cursor != "" {
		page, err := e.svc.Invoices(context.Background(), s, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, inv := range page.Items {
			got = append(got, inv.ID)
		}
		q.Cursor = page.NextCursor
	}
	if !slices.Equal(got, want) {
		t.Fatalf("páginas depois das inserções: %v, esperado %v", got, want)
	}
	all := e.invoicePages(t, s, 3)
	if len(all) != len(want)+2 || all[0] != newest.ID || !slices.Contains(all, tie.ID) {
		t.Fatalf("ao recomeçar, as faturas novas aparecem: %v", all)
	}
}

// O cursor é só uma posição: usado na sessão de outro cliente ou de outra
// organização, continua restrito às faturas daquela sessão.
func TestInvoiceCursorNeverReachesAnotherCustomerOrTenant(t *testing.T) {
	e := newEnv(t)
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	otherTenant := e.addTenant("Beta Consultoria")
	foreign := e.addCustomer(otherTenant, "Cliente Beta")
	own := map[uuid.UUID]bool{}
	for i := range 5 {
		own[e.issueAt(testNow.Add(-time.Duration(i+1)*time.Hour)).ID] = true
		e.addInvoice(e.tenantID, other.ID, billing.Open, 777, 777, nil)
		e.addInvoice(otherTenant, foreign.ID, billing.Open, 888, 888, nil)
	}
	s := e.portalSession(t)
	otherSession := Session{TenantID: e.tenantID, CustomerID: other.ID}
	foreignSession := Session{TenantID: otherTenant, CustomerID: foreign.ID}
	cursors := map[string]string{}
	for name, sess := range map[string]Session{"outro cliente": otherSession, "outro tenant": foreignSession} {
		page, err := e.svc.Invoices(context.Background(), sess, PageQuery{Limit: 1})
		if err != nil || len(page.Items) != 1 || page.NextCursor == "" || own[page.Items[0].ID] {
			t.Fatalf("%s: %+v %v", name, page, err)
		}
		cursors[name] = page.NextCursor
	}
	for name, cursor := range cursors {
		for _, leaky := range []bool{false, true} {
			e.store.leaky = leaky
			q := PageQuery{Limit: 2, Cursor: cursor}
			for range 10 {
				page, err := e.svc.Invoices(context.Background(), s, q)
				if err != nil {
					t.Fatal(err)
				}
				for _, inv := range page.Items {
					if !own[inv.ID] || inv.CustomerID != e.customer.ID || inv.TenantID != e.tenantID {
						t.Fatalf("cursor de %s (leaky=%v) alcançou fatura alheia: %+v", name, leaky, inv.Invoice)
					}
				}
				if q.Cursor = page.NextCursor; q.Cursor == "" {
					break
				}
			}
		}
	}
	e.store.leaky = false
}

func TestInvoicesRejectInvalidCursor(t *testing.T) {
	e := newEnv(t)
	e.issueAt(testNow.Add(-time.Hour))
	s := e.portalSession(t)
	linkCursor := encodeCursor(cursorLinks, Position{At: testNow, ID: uuid.New()})
	for name, cursor := range map[string]string{"malformado": "não-é-cursor", "de outra listagem": linkCursor, "longo": strings.Repeat("A", 400)} {
		t.Run(name, func(t *testing.T) {
			calls := e.store.calls["InTenant"]
			_, err := e.svc.Invoices(context.Background(), s, PageQuery{Cursor: cursor})
			assertErr(t, err, apperr.KindInvalid, "invalid_cursor")
			if e.store.calls["InTenant"] != calls {
				t.Fatal("cursor inválido não deve consultar o armazenamento")
			}
		})
	}
}

// Fatura listada sem emissão (o armazenamento real não a devolve) não gera
// cursor: a listagem falha em vez de produzir uma posição arbitrária.
func TestInvoicesWithoutIssueDateCannotAnchorCursor(t *testing.T) {
	e := newEnv(t)
	e.issueAt(testNow.Add(-time.Hour))
	e.addInvoice(e.tenantID, e.customer.ID, billing.Draft, 100, 100, nil)
	e.addInvoice(e.tenantID, e.customer.ID, billing.Draft, 100, 100, nil)
	s := e.portalSession(t)
	e.store.leaky = true
	if _, err := e.svc.Invoices(context.Background(), s, PageQuery{Limit: 2}); err == nil || apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("fatura sem emissão como âncora: %v", err)
	}
}

func TestInvoiceDetailRestrictedToSessionCustomer(t *testing.T) {
	e := newEnv(t)
	e.d().tenant.Timezone = "America/Sao_Paulo"
	e.d().tenant.LegalName, e.d().tenant.DocumentNumber = "Acme Serviços Ltda.", "11222333000181"
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
	if d.ID != own.ID || d.OrganizationName != "Acme Serviços" || d.OrganizationLegalName != "Acme Serviços Ltda." ||
		d.OrganizationDocument != "11222333000181" || d.Location.String() != "America/Sao_Paulo" || d.CheckoutURL == "" || d.Overdue {
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
		if _, err := e.svc.Invoices(context.Background(), s, PageQuery{}); !errors.Is(err, errBoom) {
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
	if _, err := e.svc.Invoices(context.Background(), s, PageQuery{}); err == nil {
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
