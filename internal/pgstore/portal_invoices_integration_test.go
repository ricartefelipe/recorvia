//go:build integration

package pgstore_test

import (
	"context"
	"crypto/sha256"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// customerInvoice devolve a única fatura emitida do cliente.
func customerInvoice(t *testing.T, env *fixture.Env, tn fixture.Tenant, customerID uuid.UUID) billing.Invoice {
	t.Helper()
	pg, err := env.Billing.List(context.Background(), tn.ID, billing.ListFilter{CustomerID: &customerID, Limit: 10})
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("faturas do cliente: %v %d", err, len(pg.Items))
	}
	return pg.Items[0]
}

// insertCharges grava tentativas de cobrança com link hospedado, sem chamar
// o provedor. statuses define a situação de cada tentativa, em ordem.
func insertCharges(t *testing.T, env *fixture.Env, tn fixture.Tenant, inv billing.Invoice, statuses ...string) {
	t.Helper()
	ctx := context.Background()
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		var account uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM payment_provider_accounts WHERE tenant_id = $1 AND provider = 'simulator'`, tn.ID).Scan(&account); err != nil {
			account = ids.New()
			hash := sha256.Sum256([]byte(account.String()))
			if _, err := tx.Exec(ctx, `INSERT INTO payment_provider_accounts (id, tenant_id, provider, environment, webhook_endpoint_token_hash)
				VALUES ($1, $2, 'simulator', 'local', $3)`, account, tn.ID, hash[:]); err != nil {
				return err
			}
		}
		for i, st := range statuses {
			if _, err := tx.Exec(ctx, `INSERT INTO charges (id, tenant_id, invoice_id, customer_id, provider_account_id, provider, attempt, status, amount,
				checkout_url, due_date) VALUES ($1, $2, $3, $4, $5, 'simulator', $6, $7, $8, $9, $10::timestamptz::date)`,
				ids.New(), tn.ID, inv.ID, inv.CustomerID, account, i+1, st, inv.AmountDue.Amount(), checkoutURL(inv.ID, i+1), *inv.DueAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkoutURL(invoiceID uuid.UUID, attempt int) string {
	return "https://pagamentos.exemplo.test/checkout/" + invoiceID.String() + "/" + strconv.Itoa(attempt)
}

// As consultas do portal filtram tenant e cliente: faturas de outro cliente,
// rascunhos e dados de outro tenant não aparecem; o link de pagamento é o da
// tentativa mais recente que aguarda pagamento.
func TestPortalInvoiceQueriesAreScopedToCustomer(t *testing.T) {
	ctx := context.Background()
	env, _, _ := portalEnv(t, 4)
	a, b := env.NewTenant(t, "portal-faturas-a"), env.NewTenant(t, "portal-faturas-b")
	cust, other, custB := env.NewCustomer(t, a), env.NewCustomer(t, a), env.NewCustomer(t, b)
	planA, planB := env.NewFlatPlan(t, a, 12000), env.NewFlatPlan(t, b, 3000)
	env.NewSubscription(t, a, cust.ID, planA.ID, nil)
	env.NewSubscription(t, a, other.ID, planA.ID, nil)
	env.NewSubscription(t, b, custB.ID, planB.ID, nil)
	for _, tn := range []fixture.Tenant{a, b} {
		if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
			t.Fatal(err)
		}
	}
	own, foreign, invB := customerInvoice(t, env, a, cust.ID), customerInvoice(t, env, a, other.ID), customerInvoice(t, env, b, custB.ID)
	draft := ids.New()
	if err := env.DB.InTenantTx(ctx, a.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, status, kind, subtotal, total, amount_due, customer_snapshot)
			VALUES ($1, $2, $3, 'draft', 'manual', 500, 500, 500, '{}')`, draft, a.ID, cust.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	insertCharges(t, env, a, own, "failed", "awaiting_payment", "canceled")
	insertCharges(t, env, a, foreign, "awaiting_payment")
	insertCharges(t, env, b, invB, "awaiting_payment")

	err := env.Store.Portal().InTenant(ctx, a.ID, func(tx portal.Tx) error {
		invs, err := tx.Invoices(ctx, cust.ID, portal.MaxInvoices)
		if err != nil {
			return err
		}
		if len(invs) != 1 || invs[0].ID != own.ID || len(invs[0].Items) != 1 || invs[0].Items[0].Amount.Amount() != 12000 {
			t.Fatalf("faturas do cliente: %+v", invs)
		}
		got, err := tx.Invoice(ctx, cust.ID, own.ID)
		if err != nil || got.ID != own.ID || len(got.Items) != 1 || got.Number != own.Number {
			t.Fatalf("fatura do cliente: %+v %v", got, err)
		}
		for name, id := range map[string]uuid.UUID{"outro cliente": foreign.ID, "rascunho": draft, "outro tenant": invB.ID} {
			if _, err := tx.Invoice(ctx, cust.ID, id); !apperr.IsKind(err, apperr.KindNotFound) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		urls, err := tx.CheckoutURLs(ctx, []uuid.UUID{own.ID, foreign.ID, invB.ID, draft})
		if err != nil {
			return err
		}
		if len(urls) != 2 || urls[own.ID] != checkoutURL(own.ID, 2) || urls[foreign.ID] != checkoutURL(foreign.ID, 1) {
			t.Fatalf("links de pagamento: %v", urls)
		}
		if urls, err := tx.CheckoutURLs(ctx, nil); err != nil || len(urls) != 0 {
			t.Fatalf("sem faturas: %v %v", urls, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// No contexto de B, nada de A é visível, mesmo com o cliente de A.
	err = env.Store.Portal().InTenant(ctx, b.ID, func(tx portal.Tx) error {
		invs, err := tx.Invoices(ctx, cust.ID, portal.MaxInvoices)
		if err != nil || len(invs) != 0 {
			t.Fatalf("faturas de A no contexto de B: %v %v", invs, err)
		}
		if _, err := tx.Invoice(ctx, cust.ID, own.ID); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Fatalf("fatura de A no contexto de B: %v", err)
		}
		urls, err := tx.CheckoutURLs(ctx, []uuid.UUID{own.ID})
		if err != nil || len(urls) != 0 {
			t.Fatalf("link de A no contexto de B: %v %v", urls, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Alterações concorrentes da mesma preferência são serializadas pelo
// bloqueio do cliente: a mudança é gravada e auditada uma única vez, e
// contatos de outro cliente não são alcançados.
func TestPortalContactPreferencesConcurrentAndScoped(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 8)
	tn := env.NewTenant(t, "portal-preferencias")
	cust, other := env.NewCustomer(t, tn), env.NewCustomer(t, tn)
	contact, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, cust.ID, customers.ContactInput{Name: "Financeiro", Email: "fin@cliente.test",
		ReceivesBillingEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := env.Customers.AddContact(ctx, tn.ID, tn.Actor, other.ID, customers.ContactInput{Name: "Alheio", Email: "alheio@cliente.test",
		ReceivesBillingEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Exchange(ctx, token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}

	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.UpdateContactPreferences(ctx, sess, []portal.ContactPreference{{ContactID: contact.ID, ReceivesBillingEmail: false}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.contact_preferences_updated'`, tn.ID); n != 1 {
		t.Fatalf("auditorias da alteração = %d, esperado 1", n)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM customer_contacts WHERE tenant_id = $1 AND id = $2 AND NOT receives_billing_email`, tn.ID, contact.ID); n != 1 {
		t.Fatal("preferência não gravada")
	}
	_, err = svc.UpdateContactPreferences(ctx, sess, []portal.ContactPreference{{ContactID: foreign.ID, ReceivesBillingEmail: false}})
	if !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("contato de outro cliente: %v", err)
	}
	// O adaptador também recusa o contato de outro cliente diretamente.
	err = env.Store.Portal().InTenant(ctx, tn.ID, func(tx portal.Tx) error {
		return tx.SetContactPreference(ctx, cust.ID, foreign.ID, false)
	})
	if !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("adaptador com contato de outro cliente: %v", err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM customer_contacts WHERE tenant_id = $1 AND id = $2 AND receives_billing_email`, tn.ID, foreign.ID); n != 1 {
		t.Fatal("contato de outro cliente alterado")
	}
}
