package portal

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

func (e *env) revoke(t *testing.T, linkID uuid.UUID) RevokeResult {
	t.Helper()
	res, err := e.svc.RevokeLink(context.Background(), e.tenantID, e.creator.Actor, e.customer.ID, linkID)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) lastAudit() audit.Entry { return e.d().audits[len(e.d().audits)-1] }

func TestRevokeLinkEndsSessionsAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	s := e.exchange(t, token)
	e.clock.Advance(5 * time.Minute)
	revokedAt := e.clock.Now()

	res := e.revoke(t, l.ID)
	if !res.Revoked || res.SessionCleanupErr != nil || res.Link.ID != l.ID || res.Link.RevokedAt == nil || !res.Link.RevokedAt.Equal(revokedAt) ||
		res.Link.RevokeReason != RevokeRequested || res.Link.State(revokedAt) != LinkRevoked {
		t.Fatalf("revogação: %+v", res)
	}
	stored := e.d().links[l.ID]
	if stored.RevokedAt == nil || !stored.RevokedAt.Equal(revokedAt) || stored.RevokeReason != RevokeRequested || stored.UsedAt == nil {
		t.Fatalf("link gravado: %+v", stored)
	}
	a := e.lastAudit()
	if a.Action != "customer.portal_link_revoked" || a.ResourceType != "customer" || a.ResourceID != e.customer.ID.String() || a.Actor != e.creator.Actor ||
		a.Details["link_id"] != l.ID || a.Details["previous_state"] != string(LinkUsed) || !a.OccurredAt.Equal(revokedAt) {
		t.Fatalf("auditoria: %+v", a)
	}
	// A sessão aberta com o link foi removida do armazenamento, não apenas recusada.
	if _, ok := e.sessions.items[s.ID]; ok || !slices.Equal(e.sessions.byLink, []uuid.UUID{l.ID}) {
		t.Fatalf("sessão do link revogado no armazenamento: %v %v", e.sessions.items, e.sessions.byLink)
	}
	_, err := e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")

	// Repetição: mesmo resultado, sem alterar o link nem auditar de novo; a
	// limpeza das sessões é refeita.
	audits := len(e.d().audits)
	e.clock.Advance(time.Hour)
	again := e.revoke(t, l.ID)
	if again.Revoked || again.SessionCleanupErr != nil || again.Link.RevokedAt == nil || !again.Link.RevokedAt.Equal(revokedAt) ||
		again.Link.RevokeReason != RevokeRequested {
		t.Fatalf("repetição: %+v", again)
	}
	if len(e.d().audits) != audits || !e.d().links[l.ID].RevokedAt.Equal(revokedAt) || e.store.calls["RevokeLink"] != 1 {
		t.Fatal("a repetição não pode alterar o link nem gerar auditoria")
	}
	if !slices.Equal(e.sessions.byLink, []uuid.UUID{l.ID, l.ID}) {
		t.Fatalf("a repetição refaz a limpeza das sessões: %v", e.sessions.byLink)
	}
}

func TestRevokeLinkRecordsPreviousState(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, token string)
		want  LinkState
	}{
		{"pendente", func(*env, string) {}, LinkPending},
		{"usado", func(e *env, token string) { e.exchange(e.t(), token) }, LinkUsed},
		{"expirado", func(e *env, _ string) { e.clock.Advance(72 * time.Hour) }, LinkExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l, token := e.link(t)
			tc.setup(e, token)
			if res := e.revoke(t, l.ID); !res.Revoked {
				t.Fatalf("revogação: %+v", res)
			}
			if got := e.lastAudit().Details["previous_state"]; got != string(tc.want) {
				t.Fatalf("situação anterior %v, esperado %s", got, tc.want)
			}
			// O link revogado não pode mais ser trocado por sessão.
			_, err := e.svc.Exchange(context.Background(), token, origin)
			assertErr(t, err, apperr.KindUnauthenticated, "portal_link_invalid")
		})
	}
}

func TestRevokeLinkByAPICredentialIsAuditedWithItsActor(t *testing.T) {
	e := newEnv(t)
	l, _ := e.link(t)
	actor := audit.APICredential(uuid.New(), "ERP interno")
	if _, err := e.svc.RevokeLink(context.Background(), e.tenantID, actor, e.customer.ID, l.ID); err != nil {
		t.Fatal(err)
	}
	if a := e.lastAudit(); a.Action != "customer.portal_link_revoked" || a.Actor != actor {
		t.Fatalf("auditoria: %+v", a)
	}
}

// Um link só é revogado pela organização e pelo cliente a que pertence: de
// outro cliente ou de outra organização responde como inexistente e nada
// muda, inclusive as sessões alheias.
func TestRevokeLinkIsRestrictedToCustomerAndTenant(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	own := e.exchange(t, token)
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	otherLink, otherToken, err := e.svc.CreateLink(context.Background(), e.tenantID, e.creator, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherSession := e.exchange(t, otherToken)
	tenantB := e.addTenant("Beta Consultoria")
	customerB := e.addCustomer(tenantB, "Cliente Beta")
	linkB, tokenB, err := e.svc.CreateLink(context.Background(), tenantB, e.creator, customerB.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionB := e.exchange(t, tokenB)
	audits := len(e.d().audits)

	cases := []struct {
		name               string
		tenant, cust, link uuid.UUID
		code               string
	}{
		{"link de outro cliente", e.tenantID, e.customer.ID, otherLink.ID, "portal_link_not_found"},
		{"link do cliente pelo caminho de outro", e.tenantID, other.ID, l.ID, "portal_link_not_found"},
		{"link de outra organização", e.tenantID, e.customer.ID, linkB.ID, "portal_link_not_found"},
		{"link inexistente", e.tenantID, e.customer.ID, uuid.New(), "portal_link_not_found"},
		{"cliente de outra organização", e.tenantID, customerB.ID, linkB.ID, "customer_not_found"},
		{"link desta organização pela outra", tenantB, customerB.ID, l.ID, "portal_link_not_found"},
		{"cliente inexistente", e.tenantID, uuid.New(), l.ID, "customer_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.RevokeLink(context.Background(), tc.tenant, e.creator.Actor, tc.cust, tc.link)
			assertErr(t, err, apperr.KindNotFound, tc.code)
		})
	}
	if len(e.d().audits) != audits || len(e.sessions.byLink) != 0 || e.store.calls["RevokeLink"] != 0 {
		t.Fatal("recusas não revogam, não auditam nem removem sessões")
	}
	for name, id := range map[string]uuid.UUID{"próprio": l.ID, "outro cliente": otherLink.ID} {
		if e.d().links[id].RevokedAt != nil {
			t.Fatalf("link %s revogado por uma chamada recusada", name)
		}
	}
	if e.store.tenants[tenantB].links[linkB.ID].RevokedAt != nil {
		t.Fatal("link de outra organização revogado")
	}

	// Revogar o link de um cliente não encerra sessões de outros clientes
	// nem de outras organizações.
	e.revoke(t, l.ID)
	if _, err := e.svc.Authenticate(context.Background(), own.ID); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("sessão do link revogado: %v", err)
	}
	for name, s := range map[string]Session{"outro cliente": otherSession, "outra organização": sessionB} {
		got, err := e.svc.Authenticate(context.Background(), s.ID)
		if err != nil || got.CustomerID != s.CustomerID || got.TenantID != s.TenantID {
			t.Fatalf("sessão de %s afetada: %+v %v", name, got, err)
		}
	}
}

func TestRevokeLinkFailures(t *testing.T) {
	for _, op := range []string{"InTenant", "LockCustomer", "Link", "RevokeLink", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			l, token := e.link(t)
			s := e.exchange(t, token)
			e.store.fail[op] = errBoom
			if _, err := e.svc.RevokeLink(context.Background(), e.tenantID, e.creator.Actor, e.customer.ID, l.ID); !errors.Is(err, errBoom) {
				t.Fatalf("erro: %v", err)
			}
			delete(e.store.fail, op)
			if e.d().links[l.ID].RevokedAt != nil || len(e.sessions.byLink) != 0 {
				t.Fatal("falha não pode revogar o link nem remover a sessão")
			}
			if _, err := e.svc.Authenticate(context.Background(), s.ID); err != nil {
				t.Fatalf("sem revogação, a sessão continua válida: %v", err)
			}
		})
	}
	t.Run("commit", func(t *testing.T) {
		e := newEnv(t)
		l, token := e.link(t)
		s := e.exchange(t, token)
		e.store.commitErr = errBoom
		if _, err := e.svc.RevokeLink(context.Background(), e.tenantID, e.creator.Actor, e.customer.ID, l.ID); !errors.Is(err, errBoom) {
			t.Fatalf("erro do commit: %v", err)
		}
		e.store.commitErr = nil
		if _, ok := e.sessions.items[s.ID]; !ok || len(e.sessions.byLink) != 0 {
			t.Fatal("sem commit, as sessões não são removidas")
		}
	})
}

// A falha do armazenamento de sessões não desfaz nem esconde a revogação:
// o link fica revogado, a sessão é recusada na requisição seguinte e a
// repetição da chamada conclui a limpeza.
func TestRevokeLinkSurvivesSessionStoreFailure(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	s := e.exchange(t, token)
	e.sessions.byLinkErr = errBoom
	res, err := e.svc.RevokeLink(context.Background(), e.tenantID, e.creator.Actor, e.customer.ID, l.ID)
	if err != nil || !res.Revoked || !errors.Is(res.SessionCleanupErr, errBoom) {
		t.Fatalf("revogação com armazenamento de sessões indisponível: %+v %v", res, err)
	}
	if e.d().links[l.ID].RevokedAt == nil || e.lastAudit().Action != "customer.portal_link_revoked" {
		t.Fatal("o link deve ficar revogado e auditado")
	}
	if _, ok := e.sessions.items[s.ID]; !ok {
		t.Fatal("cenário inválido: a sessão deveria continuar no armazenamento")
	}
	_, err = e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")

	// Outra sessão gravada para o mesmo link (o armazenamento real não a
	// teria) mostra que a repetição conclui a limpeza.
	e.sessions.items["sessao-residual"] = savedSession{s: Session{ID: "sessao-residual", LinkID: l.ID}}
	e.sessions.byLinkErr = nil
	again := e.revoke(t, l.ID)
	if again.Revoked || again.SessionCleanupErr != nil || len(e.sessions.items) != 0 {
		t.Fatalf("repetição: %+v, sessões %v", again, e.sessions.items)
	}
}

// Gerar um novo link revoga o anterior e também remove do armazenamento a
// sessão aberta com ele.
func TestCreateLinkRemovesSessionsOfSupersededLink(t *testing.T) {
	e := newEnv(t)
	first, token := e.link(t)
	s := e.exchange(t, token)
	if len(e.sessions.byLink) != 0 {
		t.Fatal("o primeiro link não substitui nenhum outro")
	}
	e.sessions.byLinkErr = errBoom
	if _, _, err := e.svc.CreateLink(context.Background(), e.tenantID, e.creator, e.customer.ID); err != nil {
		t.Fatalf("a falha da limpeza não impede a geração: %v", err)
	}
	e.sessions.byLinkErr = nil
	second, _ := e.link(t)
	if _, ok := e.sessions.items[s.ID]; !ok || len(e.sessions.byLink) != 2 || e.sessions.byLink[0] != first.ID || e.sessions.byLink[1] == second.ID {
		t.Fatalf("limpeza das sessões dos links substituídos: %v", e.sessions.byLink)
	}
	// A sessão permaneceu porque a limpeza falhou na substituição do link que
	// a originou; ainda assim ela é recusada.
	_, err := e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")

	f := newEnv(t)
	_, token = f.link(t)
	s = f.exchange(t, token)
	f.link(t)
	if _, ok := f.sessions.items[s.ID]; ok {
		t.Fatal("a sessão do link substituído deveria ter sido removida")
	}
	f.store.commitErr = errBoom
	calls := len(f.sessions.byLink)
	if _, _, err := f.svc.CreateLink(context.Background(), f.tenantID, f.creator, f.customer.ID); !errors.Is(err, errBoom) || len(f.sessions.byLink) != calls {
		t.Fatalf("sem commit, nenhuma sessão é removida: %v", err)
	}
}

func TestLinksListStatesNewestFirstWithoutTokenHash(t *testing.T) {
	e := newEnv(t)
	cid := uuid.New()
	credential := Creator{Actor: audit.APICredential(cid, "ERP interno"), CredentialID: &cid}

	first, _ := e.link(t)
	e.clock.Advance(time.Minute)
	second, token := e.link(t)
	e.exchange(t, token)
	e.clock.Advance(time.Minute)
	e.revoke(t, second.ID)
	e.clock.Advance(time.Minute)
	third, _, err := e.svc.CreateLink(context.Background(), e.tenantID, credential, e.customer.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.d().creators[first.ID], e.d().creators[second.ID], e.d().creators[third.ID] = "Ana", "Ana", "ERP interno"

	page, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{})
	if err != nil || len(page.Items) != 3 || page.NextCursor != "" {
		t.Fatalf("listagem: %+v %v", page, err)
	}
	got := page.Items
	if got[0].ID != third.ID || got[1].ID != second.ID || got[2].ID != first.ID {
		t.Fatalf("ordem: %v %v %v", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Current != LinkPending || got[0].CreatedByCredential == nil || *got[0].CreatedByCredential != cid || got[0].CreatedByUser != nil ||
		got[0].CreatorName != "ERP interno" || got[0].UsedAt != nil || got[0].RevokedAt != nil {
		t.Fatalf("link pendente gerado por credencial: %+v", got[0])
	}
	if got[1].Current != LinkRevoked || got[1].RevokeReason != RevokeRequested || got[1].UsedAt == nil || got[1].CreatorName != "Ana" ||
		got[1].CreatedByUser == nil || *got[1].CreatedByUser != *e.creator.UserID {
		t.Fatalf("link usado e revogado: %+v", got[1])
	}
	if got[2].Current != LinkRevoked || got[2].RevokeReason != RevokeSuperseded || got[2].UsedAt != nil {
		t.Fatalf("link substituído: %+v", got[2])
	}
	for _, l := range got {
		if l.TokenHash != nil || l.TenantID != e.tenantID || l.CustomerID != e.customer.ID {
			t.Fatalf("link listado com hash ou fora do escopo: %+v", l)
		}
	}

	// A situação acompanha o relógio: usado e, sem uso, expirado.
	f := newEnv(t)
	_, token = f.link(t)
	f.exchange(t, token)
	if page, err := f.svc.Links(context.Background(), f.tenantID, f.customer.ID, PageQuery{}); err != nil || page.Items[0].Current != LinkUsed {
		t.Fatalf("link usado: %+v %v", page, err)
	}
	g := newEnv(t)
	g.link(t)
	g.clock.Advance(72 * time.Hour)
	if page, err := g.svc.Links(context.Background(), g.tenantID, g.customer.ID, PageQuery{}); err != nil || page.Items[0].Current != LinkExpired {
		t.Fatalf("link expirado: %+v %v", page, err)
	}
	// Cliente sem links: lista vazia (não nula). Cliente arquivado continua listável.
	archived := g.addCustomer(g.tenantID, "Arquivado")
	archived.Status = customers.Archived
	g.d().customers[archived.ID] = archived
	if page, err := g.svc.Links(context.Background(), g.tenantID, archived.ID, PageQuery{}); err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("cliente sem links: %+v %v", page, err)
	}
}

func TestLinksPaginationCoversEachLinkOnce(t *testing.T) {
	e := newEnv(t)
	var want []uuid.UUID
	for i := range 7 {
		// Dois links no mesmo instante exercitam o desempate pelo identificador.
		if i%2 == 0 {
			e.clock.Advance(time.Minute)
		}
		l, _ := e.link(t)
		want = append(want, l.ID)
	}
	slices.SortFunc(want, func(a, b uuid.UUID) int {
		la, lb := e.d().links[a], e.d().links[b]
		if before(lb.CreatedAt, lb.ID, la.CreatedAt, la.ID) {
			return -1
		}
		return 1
	})
	for _, limit := range []int{1, 2, 3, 7, 8} {
		var got []uuid.UUID
		q := PageQuery{Limit: limit}
		for range 20 {
			page, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, q)
			if err != nil || len(page.Items) > limit {
				t.Fatalf("limite %d: %d itens, erro %v", limit, len(page.Items), err)
			}
			for _, l := range page.Items {
				got = append(got, l.ID)
			}
			if q.Cursor = page.NextCursor; q.Cursor == "" {
				break
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("limite %d: páginas %v, esperado %v", limit, got, want)
		}
	}
	// Um link gerado entre as páginas não desloca as seguintes.
	first, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{Limit: 3})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("primeira página: %+v %v", first, err)
	}
	e.clock.Advance(time.Minute)
	e.link(t)
	rest, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{Limit: 100, Cursor: first.NextCursor})
	if err != nil || len(rest.Items) != 4 {
		t.Fatalf("página seguinte: %+v %v", rest, err)
	}
	for i, l := range rest.Items {
		if l.ID != want[3+i] {
			t.Fatalf("página seguinte deslocada pela inserção: %v, esperado %v", l.ID, want[3+i])
		}
	}
}

// A listagem alcança somente os links do cliente e da organização pedidos,
// mesmo que o armazenamento devolva linhas a mais, e um cursor obtido para
// outro cliente não muda esse escopo.
func TestLinksAreRestrictedToCustomerAndTenant(t *testing.T) {
	e := newEnv(t)
	own, _ := e.link(t)
	other := e.addCustomer(e.tenantID, "Outro Cliente")
	var otherCursor string
	for range 3 {
		e.clock.Advance(time.Minute)
		if _, _, err := e.svc.CreateLink(context.Background(), e.tenantID, e.creator, other.ID); err != nil {
			t.Fatal(err)
		}
	}
	tenantB := e.addTenant("Beta Consultoria")
	customerB := e.addCustomer(tenantB, "Cliente Beta")
	if _, _, err := e.svc.CreateLink(context.Background(), tenantB, e.creator, customerB.ID); err != nil {
		t.Fatal(err)
	}
	page, err := e.svc.Links(context.Background(), e.tenantID, other.ID, PageQuery{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("links do outro cliente: %+v %v", page, err)
	}
	otherCursor = page.NextCursor

	for _, leaky := range []bool{false, true} {
		e.store.leaky = leaky
		for name, cursor := range map[string]string{"sem cursor": "", "cursor de outro cliente": otherCursor} {
			page, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].ID != own.ID || page.Items[0].TokenHash != nil {
				t.Fatalf("leaky=%v, %s: links fora do escopo ou com hash: %+v", leaky, name, page.Items)
			}
		}
	}
	e.store.leaky = false
	// Cliente de outra organização não é encontrado nesta, e vice-versa.
	_, err = e.svc.Links(context.Background(), e.tenantID, customerB.ID, PageQuery{})
	assertErr(t, err, apperr.KindNotFound, "customer_not_found")
	_, err = e.svc.Links(context.Background(), tenantB, e.customer.ID, PageQuery{})
	assertErr(t, err, apperr.KindNotFound, "customer_not_found")
	if page, err := e.svc.Links(context.Background(), tenantB, customerB.ID, PageQuery{}); err != nil || len(page.Items) != 1 ||
		page.Items[0].CustomerID != customerB.ID {
		t.Fatalf("links na outra organização: %+v %v", page, err)
	}
}

func TestLinksRejectInvalidCursorAndPropagateFailures(t *testing.T) {
	e := newEnv(t)
	e.link(t)
	invoiceCursor := encodeCursor(cursorInvoices, Position{At: testNow, ID: uuid.New()})
	for name, cursor := range map[string]string{"malformado": "###", "de outra listagem": invoiceCursor} {
		t.Run(name, func(t *testing.T) {
			calls := e.store.calls["InTenant"]
			_, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{Cursor: cursor})
			assertErr(t, err, apperr.KindInvalid, "invalid_cursor")
			if e.store.calls["InTenant"] != calls {
				t.Fatal("cursor inválido não deve consultar o armazenamento")
			}
		})
	}
	for _, op := range []string{"InTenant", "Customer", "Links"} {
		e.store.fail[op] = errBoom
		if _, err := e.svc.Links(context.Background(), e.tenantID, e.customer.ID, PageQuery{}); !errors.Is(err, errBoom) {
			t.Fatalf("%s: %v", op, err)
		}
		delete(e.store.fail, op)
	}
}
