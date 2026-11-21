//go:build integration

package pgstore_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// linkRow lê, como dono das tabelas, as colunas de revogação de um link.
func linkRow(t *testing.T, id uuid.UUID) (revokedAt *time.Time, reason *string) {
	t.Helper()
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT revoked_at, revoke_reason FROM customer_portal_links WHERE id = $1`, id).
		Scan(&revokedAt, &reason); err != nil {
		t.Fatal(err)
	}
	return revokedAt, reason
}

func revocationAudits(t *testing.T, env *fixture.Env, tn fixture.Tenant, customerID uuid.UUID) int {
	t.Helper()
	return countRows(t, env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.portal_link_revoked'
		AND resource_type = 'customer' AND resource_id = $2`, tn.ID, customerID.String())
}

// Listagem e revogação explícita no PostgreSQL: a lista traz situação,
// datas e autoria (membro ou credencial) sem o hash do token; a revogação
// grava o motivo, audita uma vez, remove a sessão aberta com o link e é
// idempotente.
func TestPortalLinkListingAndExplicitRevocation(t *testing.T) {
	ctx := context.Background()
	env, svc, sessions := portalEnv(t, 4)
	tn := apiTenant(t, env, "portal-revogacao")
	cust, other := env.NewCustomer(t, tn), env.NewCustomer(t, tn)
	creds := apicredentials.NewService(env.Store.APICredentials(), nil, env.Clock, apicredentials.DefaultPolicy())
	issued, err := creds.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "ERP interno", Scopes: []string{"customers.portal_links"}})
	if err != nil {
		t.Fatal(err)
	}
	credID := issued.Credential.ID
	byCredential := portal.Creator{Actor: audit.APICredential(credID, "ERP interno"), CredentialID: &credID}

	first, _, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Minute)
	second, token, err := svc.CreateLink(ctx, tn.ID, byCredential, cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Exchange(ctx, token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherSess, err := svc.Exchange(ctx, otherToken, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}

	page, err := svc.Links(ctx, tn.ID, cust.ID, portal.PageQuery{})
	if err != nil || len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("listagem: %+v %v", page, err)
	}
	newest, oldest := page.Items[0], page.Items[1]
	if newest.ID != second.ID || newest.Current != portal.LinkUsed || newest.UsedAt == nil || newest.CreatedByCredential == nil ||
		*newest.CreatedByCredential != credID || newest.CreatedByUser != nil || newest.CreatorName != "ERP interno" ||
		!newest.ExpiresAt.Equal(second.ExpiresAt) || !newest.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("link usado gerado por credencial: %+v", newest)
	}
	if oldest.ID != first.ID || oldest.Current != portal.LinkRevoked || oldest.RevokeReason != portal.RevokeSuperseded || oldest.CreatedByUser == nil ||
		*oldest.CreatedByUser != tn.Owner.ID || oldest.CreatorName != tn.Owner.DisplayName {
		t.Fatalf("link substituído gerado por membro: %+v", oldest)
	}
	for _, l := range page.Items {
		if l.TokenHash != nil || l.TenantID != tn.ID || l.CustomerID != cust.ID {
			t.Fatalf("link listado com hash ou fora do escopo: %+v", l)
		}
	}

	env.Clock.Advance(time.Minute)
	revokedAt := env.Clock.Now()
	res, err := svc.RevokeLink(ctx, tn.ID, byCredential.Actor, cust.ID, second.ID)
	if err != nil || !res.Revoked || res.SessionCleanupErr != nil {
		t.Fatalf("revogação: %+v %v", res, err)
	}
	at, reason := linkRow(t, second.ID)
	if at == nil || !at.Equal(revokedAt) || reason == nil || *reason != portal.RevokeRequested {
		t.Fatalf("link revogado no banco: %v %v", at, reason)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'customer.portal_link_revoked'
		AND resource_id = $2 AND actor_type = 'api_credential' AND actor_id = $3 AND details->>'link_id' = $4 AND details->>'previous_state' = 'used'`,
		tn.ID, cust.ID.String(), credID.String(), second.ID.String()); n != 1 {
		t.Fatalf("auditoria da revogação = %d, esperado 1", n)
	}
	if sessions.has(sess.ID) {
		t.Fatal("a sessão aberta com o link revogado deveria ter sido removida")
	}
	if _, err := svc.Authenticate(ctx, sess.ID); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("sessão do link revogado: %v", err)
	}
	if _, err := svc.Authenticate(ctx, otherSess.ID); err != nil {
		t.Fatalf("a sessão de outro cliente não pode ser afetada: %v", err)
	}

	// Repetição: nada muda no banco nem na auditoria.
	env.Clock.Advance(time.Hour)
	again, err := svc.RevokeLink(ctx, tn.ID, tn.Actor, cust.ID, second.ID)
	if err != nil || again.Revoked || again.Link.RevokedAt == nil || !again.Link.RevokedAt.Equal(revokedAt) {
		t.Fatalf("repetição: %+v %v", again, err)
	}
	if at, _ := linkRow(t, second.ID); at == nil || !at.Equal(revokedAt) || revocationAudits(t, env, tn, cust.ID) != 1 {
		t.Fatal("a repetição não pode regravar a revogação nem auditar de novo")
	}
	// Link já substituído: a revogação explícita não troca o motivo.
	if res, err := svc.RevokeLink(ctx, tn.ID, tn.Actor, cust.ID, first.ID); err != nil || res.Revoked {
		t.Fatalf("link já substituído: %+v %v", res, err)
	}
	if _, reason := linkRow(t, first.ID); reason == nil || *reason != portal.RevokeSuperseded {
		t.Fatalf("motivo do link substituído alterado: %v", reason)
	}
	page, err = svc.Links(ctx, tn.ID, cust.ID, portal.PageQuery{})
	if err != nil || len(page.Items) != 2 || page.Items[0].Current != portal.LinkRevoked || page.Items[0].RevokeReason != portal.RevokeRequested ||
		page.Items[0].RevokedAt == nil || !page.Items[0].RevokedAt.Equal(revokedAt) {
		t.Fatalf("listagem depois da revogação: %+v %v", page, err)
	}
	// Depois da revogação explícita, um novo link pode ser gerado e usado.
	_, fresh, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Exchange(ctx, fresh, "ip:teste"); err != nil {
		t.Fatalf("novo link depois da revogação: %v", err)
	}
}

// Um link nunca é listado nem revogado a partir de outro cliente ou de
// outro tenant, pelo caso de uso e diretamente pelo adaptador.
func TestPortalLinkListingAndRevocationAreIsolated(t *testing.T) {
	ctx := context.Background()
	env, svc, sessions := portalEnv(t, 4)
	a, b := env.NewTenant(t, "portal-links-a"), env.NewTenant(t, "portal-links-b")
	custA, otherA, custB := env.NewCustomer(t, a), env.NewCustomer(t, a), env.NewCustomer(t, b)
	linkA, tokenA, err := svc.CreateLink(ctx, a.ID, ownerCreator(a), custA.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessA, err := svc.Exchange(ctx, tokenA, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	linkOther, _, err := svc.CreateLink(ctx, a.ID, ownerCreator(a), otherA.ID)
	if err != nil {
		t.Fatal(err)
	}
	linkB, _, err := svc.CreateLink(ctx, b.ID, ownerCreator(b), custB.ID)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name           string
		tenant         fixture.Tenant
		customer, link uuid.UUID
		code           string
	}{
		{"link de outro cliente do mesmo tenant", a, custA.ID, linkOther.ID, "portal_link_not_found"},
		{"link do cliente pelo caminho de outro cliente", a, otherA.ID, linkA.ID, "portal_link_not_found"},
		{"link de outro tenant", a, custA.ID, linkB.ID, "portal_link_not_found"},
		{"cliente de outro tenant", b, custA.ID, linkA.ID, "customer_not_found"},
		{"link de A pelo cliente de B", b, custB.ID, linkA.ID, "portal_link_not_found"},
		{"link inexistente", a, custA.ID, ids.New(), "portal_link_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RevokeLink(ctx, tc.tenant.ID, tc.tenant.Actor, tc.customer, tc.link)
			e, ok := apperr.As(err)
			if !ok || e.Kind != apperr.KindNotFound || e.Code != tc.code {
				t.Fatalf("erro = %v, esperado NotFound/%s", err, tc.code)
			}
		})
	}
	// O adaptador também não revoga o link pelo cliente errado nem em outro tenant.
	for name, target := range map[string]struct{ tenant, customer uuid.UUID }{"cliente errado": {a.ID, otherA.ID}, "outro tenant": {b.ID, custA.ID}} {
		err := env.Store.Portal().InTenant(ctx, target.tenant, func(tx portal.Tx) error {
			return tx.RevokeLink(ctx, target.customer, linkA.ID, env.Clock.Now(), portal.RevokeRequested)
		})
		if !apperr.IsKind(err, apperr.KindConflict) {
			t.Fatalf("adaptador, %s: %v", name, err)
		}
	}
	for name, id := range map[string]uuid.UUID{"A": linkA.ID, "outro cliente": linkOther.ID, "B": linkB.ID} {
		if at, _ := linkRow(t, id); at != nil {
			t.Fatalf("link %s revogado por chamada recusada", name)
		}
	}
	if revocationAudits(t, env, a, custA.ID)+revocationAudits(t, env, a, otherA.ID)+revocationAudits(t, env, b, custB.ID) != 0 {
		t.Fatal("chamadas recusadas não geram auditoria")
	}
	if !sessions.has(sessA.ID) {
		t.Fatal("a sessão do cliente não pode ser removida por chamada recusada")
	}

	// Listagem: cada cliente vê somente os próprios links; no contexto de
	// outro tenant, o cliente não existe e o adaptador não devolve linhas.
	for _, c := range []struct {
		tenant   fixture.Tenant
		customer uuid.UUID
		want     uuid.UUID
	}{{a, custA.ID, linkA.ID}, {a, otherA.ID, linkOther.ID}, {b, custB.ID, linkB.ID}} {
		page, err := svc.Links(ctx, c.tenant.ID, c.customer, portal.PageQuery{})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != c.want {
			t.Fatalf("links do cliente %s: %+v %v", c.customer, page, err)
		}
	}
	if _, err := svc.Links(ctx, b.ID, custA.ID, portal.PageQuery{}); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("links de cliente de A no contexto de B: %v", err)
	}
	err = env.Store.Portal().InTenant(ctx, b.ID, func(tx portal.Tx) error {
		links, err := tx.Links(ctx, custA.ID, nil, portal.MaxPageSize)
		if err != nil || len(links) != 0 {
			t.Fatalf("adaptador: links de A no contexto de B: %v %v", links, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Um cursor obtido na listagem de outro cliente é só uma posição.
	env.Clock.Advance(time.Minute)
	if _, _, err := svc.CreateLink(ctx, a.ID, ownerCreator(a), otherA.ID); err != nil {
		t.Fatal(err)
	}
	foreign, err := svc.Links(ctx, a.ID, otherA.ID, portal.PageQuery{Limit: 1})
	if err != nil || foreign.NextCursor == "" {
		t.Fatalf("cursor de outro cliente: %+v %v", foreign, err)
	}
	page, err := svc.Links(ctx, a.ID, custA.ID, portal.PageQuery{Cursor: foreign.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range page.Items {
		if l.CustomerID != custA.ID || l.ID != linkA.ID {
			t.Fatalf("cursor de outro cliente alcançou link alheio: %+v", l)
		}
	}
}

// Paginação dos links no PostgreSQL: cada link aparece uma vez, do mais novo
// para o mais antigo, com desempate pelo identificador.
func TestPortalLinkPaginationOverPostgres(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 4)
	tn := env.NewTenant(t, "portal-links-pag")
	cust := env.NewCustomer(t, tn)
	type created struct {
		id uuid.UUID
		at time.Time
	}
	var all []created
	for i := range 7 {
		if i%2 == 0 {
			env.Clock.Advance(time.Minute)
		}
		l, _, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, created{l.ID, l.CreatedAt})
	}
	slices.SortFunc(all, func(x, y created) int {
		if c := y.at.Compare(x.at); c != 0 {
			return c
		}
		return slices.Compare(y.id[:], x.id[:])
	})
	var got []uuid.UUID
	q := portal.PageQuery{Limit: 3}
	for range 10 {
		page, err := svc.Links(ctx, tn.ID, cust.ID, q)
		if err != nil || len(page.Items) > 3 {
			t.Fatalf("página: %+v %v", page, err)
		}
		for _, l := range page.Items {
			got = append(got, l.ID)
		}
		if q.Cursor = page.NextCursor; q.Cursor == "" {
			break
		}
	}
	if len(got) != len(all) {
		t.Fatalf("links percorridos: %d, esperado %d", len(got), len(all))
	}
	for i, c := range all {
		if got[i] != c.id {
			t.Fatalf("posição %d: %v, esperado %v", i, got[i], c.id)
		}
	}
}

// Revogações concorrentes do mesmo link: todas respondem com sucesso, o link
// é revogado e auditado uma única vez.
func TestPortalLinkConcurrentRevocation(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 8)
	tn := env.NewTenant(t, "portal-revogacao-conc")
	cust := env.NewCustomer(t, tn)
	l, _, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	var wg sync.WaitGroup
	results := make(chan portal.RevokeResult, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.RevokeLink(ctx, tn.ID, tn.Actor, cust.ID, l.ID)
			results <- res
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	effective := 0
	for res := range results {
		if res.Revoked {
			effective++
		}
	}
	if effective != 1 || revocationAudits(t, env, tn, cust.ID) != 1 {
		t.Fatalf("revogações efetivas = %d, auditorias = %d; esperado 1 e 1", effective, revocationAudits(t, env, tn, cust.ID))
	}
}

// Revogação concorrente com a troca do link por sessão: qualquer que seja a
// ordem, ao final o link está revogado e nenhuma sessão permanece válida nem
// guardada.
func TestPortalLinkRevocationRacesWithExchange(t *testing.T) {
	ctx := context.Background()
	env, svc, sessions := portalEnv(t, 8)
	tn := env.NewTenant(t, "portal-revogacao-troca")
	for round := range 8 {
		cust := env.NewCustomer(t, tn)
		l, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var sess portal.Session
		var exchangeErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			sess, exchangeErr = svc.Exchange(ctx, token, "ip:teste")
		}()
		go func() {
			defer wg.Done()
			_, revokeErr = svc.RevokeLink(ctx, tn.ID, tn.Actor, cust.ID, l.ID)
		}()
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("rodada %d: revogação: %v", round, revokeErr)
		}
		if exchangeErr != nil && !apperr.IsKind(exchangeErr, apperr.KindUnauthenticated) {
			t.Fatalf("rodada %d: troca: %v", round, exchangeErr)
		}
		if at, _ := linkRow(t, l.ID); at == nil {
			t.Fatalf("rodada %d: link não revogado", round)
		}
		if exchangeErr == nil {
			if sessions.has(sess.ID) {
				t.Fatalf("rodada %d: sessão aberta antes da revogação continua guardada", round)
			}
			if _, err := svc.Authenticate(ctx, sess.ID); !apperr.IsKind(err, apperr.KindUnauthenticated) {
				t.Fatalf("rodada %d: sessão de link revogado aceita: %v", round, err)
			}
		}
	}
}

// insertIssuedInvoice grava uma fatura emitida do cliente no instante dado.
func insertIssuedInvoice(t *testing.T, env *fixture.Env, tn fixture.Tenant, customerID, id uuid.UUID, number string, issuedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, number, status, kind, issued_at, due_at, subtotal, total, amount_due,
			customer_snapshot) VALUES ($1, $2, $3, $4, 'open', 'manual', $5, $6, 1000, 1000, 1000, '{}')`,
			id, tn.ID, customerID, number, issuedAt, issuedAt.Add(10*24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// portalSessionFor abre uma sessão do portal para o cliente.
func portalSessionFor(t *testing.T, svc *portal.Service, tn fixture.Tenant, customerID uuid.UUID) portal.Session {
	t.Helper()
	ctx := context.Background()
	_, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), customerID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Exchange(ctx, token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// Paginação das faturas do portal no PostgreSQL: cada fatura emitida do
// cliente aparece uma única vez, na ordem de emissão e identificador
// decrescentes; faturas emitidas durante a navegação não deslocam as páginas
// seguintes; e o cursor de outro cliente ou de outro tenant não alcança
// faturas alheias.
func TestPortalInvoicePaginationIsStableAndScoped(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 4)
	a, b := env.NewTenant(t, "portal-pag-a"), env.NewTenant(t, "portal-pag-b")
	cust, other, custB := env.NewCustomer(t, a), env.NewCustomer(t, a), env.NewCustomer(t, b)
	base := env.Clock.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)
	type issued struct {
		id uuid.UUID
		at time.Time
	}
	var own []issued
	for i := range 9 {
		// Emissões repetidas de três em três exercitam o desempate pelo identificador.
		inv := issued{ids.New(), base.Add(time.Duration(i/3) * time.Hour)}
		insertIssuedInvoice(t, env, a, cust.ID, inv.id, fmt.Sprintf("PAG-%04d", i), inv.at)
		own = append(own, inv)
		insertIssuedInvoice(t, env, a, other.ID, ids.New(), fmt.Sprintf("OUT-%04d", i), inv.at)
		insertIssuedInvoice(t, env, b, custB.ID, ids.New(), fmt.Sprintf("B-%04d", i), inv.at)
	}
	if err := env.DB.InTenantTx(ctx, a.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, status, kind, subtotal, total, amount_due, customer_snapshot)
			VALUES ($1, $2, $3, 'draft', 'manual', 500, 500, 500, '{}')`, ids.New(), a.ID, cust.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	order := func(list []issued) []uuid.UUID {
		sorted := slices.Clone(list)
		slices.SortFunc(sorted, func(x, y issued) int {
			if c := y.at.Compare(x.at); c != 0 {
				return c
			}
			return slices.Compare(y.id[:], x.id[:])
		})
		out := make([]uuid.UUID, 0, len(sorted))
		for _, inv := range sorted {
			out = append(out, inv.id)
		}
		return out
	}
	want := order(own)
	ownSet := map[uuid.UUID]bool{}
	for _, inv := range own {
		ownSet[inv.id] = true
	}
	sess := portalSessionFor(t, svc, a, cust.ID)
	walk := func(s portal.Session, q portal.PageQuery) []uuid.UUID {
		t.Helper()
		var got []uuid.UUID
		for range 50 {
			page, err := svc.Invoices(ctx, s, q)
			if err != nil {
				t.Fatal(err)
			}
			if q.Limit > 0 && len(page.Items) > q.Limit {
				t.Fatalf("página com %d itens excede o limite %d", len(page.Items), q.Limit)
			}
			for _, inv := range page.Items {
				if inv.TenantID != s.TenantID || inv.CustomerID != s.CustomerID {
					t.Fatalf("fatura fora do escopo da sessão: %+v", inv.Invoice)
				}
				got = append(got, inv.ID)
			}
			if q.Cursor = page.NextCursor; q.Cursor == "" {
				return got
			}
		}
		t.Fatal("a paginação não termina")
		return nil
	}
	for _, limit := range []int{1, 2, 4, 9, 10} {
		if got := walk(sess, portal.PageQuery{Limit: limit}); !slices.Equal(got, want) {
			t.Fatalf("limite %d: %v, esperado %v", limit, got, want)
		}
	}

	// Inserções entre as páginas: uma fatura mais nova que todas, outra no
	// mesmo instante da última já entregue com identificador maior (ficaria
	// antes dela) e outra mais antiga que todas (ainda não percorrida).
	first, err := svc.Invoices(ctx, sess, portal.PageQuery{Limit: 4})
	if err != nil || len(first.Items) != 4 || first.NextCursor == "" {
		t.Fatalf("primeira página: %+v %v", first, err)
	}
	last := first.Items[3]
	newest := issued{ids.New(), base.Add(24 * time.Hour)}
	tie := issued{uuid.Max, *last.IssuedAt}
	oldest := issued{ids.New(), base.Add(-time.Hour)}
	for i, inv := range []issued{newest, tie, oldest} {
		insertIssuedInvoice(t, env, a, cust.ID, inv.id, fmt.Sprintf("NOVA-%04d", i), inv.at)
	}
	rest := walk(sess, portal.PageQuery{Limit: 4, Cursor: first.NextCursor})
	if wantRest := append(slices.Clone(want[4:]), oldest.id); !slices.Equal(rest, wantRest) {
		t.Fatalf("páginas depois das inserções: %v, esperado %v", rest, wantRest)
	}
	own = append(own, newest, tie, oldest)
	for _, inv := range []issued{newest, tie, oldest} {
		ownSet[inv.id] = true
	}
	if got := walk(sess, portal.PageQuery{Limit: 5}); !slices.Equal(got, order(own)) {
		t.Fatalf("ao recomeçar: %v, esperado %v", got, order(own))
	}

	// Cursores de outro cliente e de outro tenant são apenas posições.
	sessOther, sessB := portalSessionFor(t, svc, a, other.ID), portalSessionFor(t, svc, b, custB.ID)
	for name, s := range map[string]portal.Session{"outro cliente": sessOther, "outro tenant": sessB} {
		foreign, err := svc.Invoices(ctx, s, portal.PageQuery{Limit: 2})
		if err != nil || len(foreign.Items) != 2 || foreign.NextCursor == "" {
			t.Fatalf("%s: %+v %v", name, foreign, err)
		}
		for _, inv := range foreign.Items {
			if ownSet[inv.ID] {
				t.Fatalf("%s enxerga fatura do cliente: %v", name, inv.ID)
			}
		}
		for _, id := range walk(sess, portal.PageQuery{Limit: 3, Cursor: foreign.NextCursor}) {
			if !ownSet[id] {
				t.Fatalf("cursor de %s alcançou fatura alheia: %v", name, id)
			}
		}
		// E o cursor do cliente, na sessão alheia, não traz faturas dele.
		for _, id := range walk(s, portal.PageQuery{Limit: 3, Cursor: first.NextCursor}) {
			if ownSet[id] {
				t.Fatalf("cursor do cliente na sessão de %s trouxe fatura dele: %v", name, id)
			}
		}
	}
	// Cursor inválido: 422, sem consulta.
	if _, err := svc.Invoices(ctx, sess, portal.PageQuery{Cursor: "invalido"}); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("cursor inválido: %v", err)
	}
}
