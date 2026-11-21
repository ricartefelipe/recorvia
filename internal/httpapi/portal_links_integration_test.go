//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// newPortalLink gera um link pelo painel e devolve o identificador e o token.
func newPortalLink(t *testing.T, c *client, customerID uuid.UUID) (id, token string) {
	t.Helper()
	st, body, _ := c.do(http.MethodPost, "/api/v1/customers/"+customerID.String()+"/portal-links", nil)
	if st != http.StatusCreated {
		t.Fatalf("geração do link: %d %v", st, body)
	}
	id, _ = body["id"].(string)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("a geração deve devolver o identificador do link: %v", body)
	}
	return id, portalToken(t, body)
}

// listPortalLinks lê a lista de links do cliente e a valida contra o contrato.
func listPortalLinks(t *testing.T, c *client, customerID uuid.UUID, query string) (items []map[string]any, next string) {
	t.Helper()
	st, body, _ := c.do(http.MethodGet, "/api/v1/customers/"+customerID.String()+"/portal-links"+query, nil)
	if st != http.StatusOK {
		t.Fatalf("lista de links: %d %v", st, body)
	}
	assertSchema(t, "PortalLinkPage", body)
	items = objects(t, body["data"])
	for _, l := range items {
		for _, secret := range []string{"token", "token_hash", "url", "hash"} {
			if _, ok := l[secret]; ok {
				t.Fatalf("a lista de links não expõe %s: %v", secret, l)
			}
		}
	}
	next, _ = body["next_cursor"].(string)
	return items, next
}

func (a *apiEnv) portalKeys(t *testing.T, pattern string) []string {
	t.Helper()
	keys, err := a.rdb.Keys(context.Background(), "recorvia:portal:"+pattern).Result()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// Listagem e revogação explícita pelo painel: permissão igual à da geração,
// CSRF, isolamento entre clientes e organizações, encerramento da sessão do
// portal no Redis, auditoria única e idempotência.
func TestPortalLinkListAndRevokeOverHTTP(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := a.env.NewTenant(t, "links-a"), a.env.NewTenant(t, "links-b")
	cust, other := a.env.NewCustomer(t, tnA), a.env.NewCustomer(t, tnA)
	custB := a.env.NewCustomer(t, tnB)
	operatorUser := member(t, a.env, tnA, tenancy.RoleOperator)
	operator := a.login(operatorUser, &tnA.ID)
	auditor := a.login(member(t, a.env, tnA, tenancy.RoleAuditor), &tnA.ID)
	owner := a.login(tnA.Owner, &tnA.ID)
	ownerB := a.login(tnB.Owner, &tnB.ID)
	base := "/api/v1/customers/" + cust.ID.String() + "/portal-links"

	if items, next := listPortalLinks(t, operator, cust.ID, ""); len(items) != 0 || next != "" {
		t.Fatalf("cliente sem links: %v %q", items, next)
	}
	linkID, token := newPortalLink(t, operator, cust.ID)
	otherLinkID, otherToken := newPortalLink(t, operator, other.ID)
	linkBID, tokenB := newPortalLink(t, ownerB, custB.ID)

	items, _ := listPortalLinks(t, operator, cust.ID, "")
	if len(items) != 1 {
		t.Fatalf("links do cliente: %v", items)
	}
	created := items[0]
	by, _ := created["created_by"].(map[string]any)
	if created["id"] != linkID || created["status"] != "pending" || created["used_at"] != nil || created["revoked_at"] != nil ||
		created["revoke_reason"] != nil || by["type"] != "user" || by["id"] != operatorUser.ID.String() || by["name"] != operatorUser.DisplayName {
		t.Fatalf("link pendente: %v", created)
	}
	if exp := instant(t, created["expires_at"]); !exp.After(instant(t, created["created_at"])) {
		t.Fatalf("validade do link: %v", created)
	}

	// Quem não pode gerar links não os lista nem revoga; outra organização
	// não encontra o cliente; sem CSRF a revogação é recusada.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		path := base
		if method == http.MethodDelete {
			path += "/" + linkID
		}
		if st, body, _ := auditor.do(method, path, nil); st != http.StatusForbidden || body["code"] != "forbidden" {
			t.Fatalf("%s pelo auditor: %d %v", method, st, body)
		}
		if st, body, _ := ownerB.do(method, path, nil); st != http.StatusNotFound || body["code"] != "customer_not_found" {
			t.Fatalf("%s por outra organização: %d %v", method, st, body)
		}
	}
	session := &http.Cookie{Name: "recorvia_session", Value: operator.cookie}
	if r := a.raw(http.MethodDelete, base+"/"+linkID, nil, map[string]string{"X-Recorvia-Tenant": tnA.ID.String()}, session); r.status != http.StatusForbidden ||
		r.body["code"] != "csrf_invalid" {
		t.Fatalf("revogação sem CSRF: %d %v", r.status, r.body)
	}
	// Link de outro cliente, de outra organização, inexistente ou malformado: 404, nada muda.
	for name, c := range map[string]struct{ path, code string }{
		"link de outro cliente":             {base + "/" + otherLinkID, "portal_link_not_found"},
		"link do cliente por outro cliente": {"/api/v1/customers/" + other.ID.String() + "/portal-links/" + linkID, "portal_link_not_found"},
		"link de outra organização":         {base + "/" + linkBID, "portal_link_not_found"},
		"link inexistente":                  {base + "/" + uuid.NewString(), "portal_link_not_found"},
		"identificador malformado":          {base + "/nao-e-uuid", "not_found"},
	} {
		if st, body, _ := operator.do(http.MethodDelete, c.path, nil); st != http.StatusNotFound || body["code"] != c.code {
			t.Fatalf("%s: %d %v", name, st, body)
		}
	}

	// As três sessões do portal ficam abertas; a revogação alcança só a do link.
	cookie, _ := openPortal(t, a, token)
	otherCookie, _ := openPortal(t, a, otherToken)
	cookieB, _ := openPortal(t, a, tokenB)
	if n := len(a.portalKeys(t, "portal_session:*")); n != 3 {
		t.Fatalf("sessões do portal no Redis: %d", n)
	}
	if items, _ := listPortalLinks(t, operator, cust.ID, ""); items[0]["status"] != "used" || items[0]["used_at"] == nil {
		t.Fatalf("link usado: %v", items[0])
	}
	if st, body, _ := operator.do(http.MethodDelete, base+"/"+linkID, nil); st != http.StatusNoContent {
		t.Fatalf("revogação: %d %v", st, body)
	}
	// A sessão foi removida do Redis pela revogação, antes de qualquer nova
	// requisição do portal; as demais continuam lá.
	if n := len(a.portalKeys(t, "portal_session:*")); n != 2 {
		t.Fatalf("sessões do portal no Redis depois da revogação: %d", n)
	}
	if keys := a.portalKeys(t, "link_sessions:"+linkID); len(keys) != 0 {
		t.Fatalf("índice de sessões do link revogado: %v", keys)
	}
	for _, path := range []string{"/api/portal/v1/session", "/api/portal/v1/invoices"} {
		if r := a.raw(http.MethodGet, path, nil, nil, cookie); r.status != http.StatusUnauthorized || r.body["code"] != "portal_session_invalid" {
			t.Fatalf("%s com a sessão do link revogado: %d %v", path, r.status, r.body)
		}
	}
	for name, c := range map[string]*http.Cookie{"outro cliente": otherCookie, "outra organização": cookieB} {
		if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, c); r.status != http.StatusOK {
			t.Fatalf("sessão de %s afetada pela revogação: %d %v", name, r.status, r.body)
		}
	}
	// Idempotência: repetir responde 204 e a auditoria continua única.
	for range 2 {
		if st, body, _ := operator.do(http.MethodDelete, base+"/"+linkID, nil); st != http.StatusNoContent {
			t.Fatalf("revogação repetida: %d %v", st, body)
		}
	}
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.portal_link_revoked&resource_id="+cust.ID.String(), nil)
	ev := objects(t, events["data"])
	if st != http.StatusOK || len(ev) != 1 || ev[0]["actor_type"] != "user" || ev[0]["actor_id"] != operatorUser.ID.String() {
		t.Fatalf("auditoria da revogação: %d %v", st, events)
	}
	if details, _ := ev[0]["details"].(map[string]any); details["link_id"] != linkID || details["previous_state"] != "used" {
		t.Fatalf("detalhes da auditoria: %v", ev[0])
	}
	if st, all, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.portal_link_revoked", nil); st != http.StatusOK ||
		len(objects(t, all["data"])) != 1 {
		t.Fatalf("recusas não podem gerar auditoria: %d %v", st, all)
	}
	items, _ = listPortalLinks(t, operator, cust.ID, "")
	if items[0]["status"] != "revoked" || items[0]["revoke_reason"] != "revoked" || items[0]["revoked_at"] == nil || items[0]["used_at"] == nil {
		t.Fatalf("link revogado: %v", items[0])
	}
	if items, _ := listPortalLinks(t, operator, other.ID, ""); len(items) != 1 || items[0]["id"] != otherLinkID || items[0]["status"] != "used" {
		t.Fatalf("link de outro cliente alterado: %v", items)
	}

	// Link pendente revogado não pode mais ser aberto.
	pendingID, pendingToken := newPortalLink(t, operator, cust.ID)
	if st, body, _ := operator.do(http.MethodDelete, base+"/"+pendingID, nil); st != http.StatusNoContent {
		t.Fatalf("revogação do link pendente: %d %v", st, body)
	}
	if r := a.portalAccess(pendingToken); r.status != http.StatusUnauthorized || r.body["code"] != "portal_link_invalid" {
		t.Fatalf("link revogado aceito: %d %v", r.status, r.body)
	}
	// Cliente arquivado: os links continuam listáveis e revogáveis.
	archivedID, _ := newPortalLink(t, operator, cust.ID)
	if st, body, _ := owner.do(http.MethodPost, "/api/v1/customers/"+cust.ID.String()+"/archive", nil); st != http.StatusOK {
		t.Fatalf("arquivamento: %d %v", st, body)
	}
	if st, body, _ := operator.do(http.MethodDelete, base+"/"+archivedID, nil); st != http.StatusNoContent {
		t.Fatalf("revogação com cliente arquivado: %d %v", st, body)
	}

	// Paginação da lista: do mais novo para o mais antigo, sem repetição.
	want := []string{archivedID, pendingID, linkID}
	var got []string
	query := "?limit=2"
	for range 5 {
		page, next := listPortalLinks(t, operator, cust.ID, query)
		if len(page) > 2 {
			t.Fatalf("página acima do limite: %v", page)
		}
		for _, l := range page {
			got = append(got, l["id"].(string))
		}
		if next == "" {
			break
		}
		query = "?limit=2&cursor=" + next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("páginas de links: %v, esperado %v", got, want)
	}
	for name, c := range map[string]struct{ query, code string }{"cursor inválido": {"?cursor=invalido", "invalid_cursor"},
		"limite zero": {"?limit=0", "invalid_limit"}, "limite acima do máximo": {"?limit=101", "invalid_limit"}} {
		if st, body, _ := operator.do(http.MethodGet, base+c.query, nil); st != http.StatusUnprocessableEntity || body["code"] != c.code {
			t.Fatalf("%s: %d %v", name, st, body)
		}
	}
}

// Credencial de API com o escopo customers.portal_links gera, lista e revoga
// links sem cookie nem CSRF: a autoria do link é a credencial e a revogação,
// que não tem coluna de autoria, fica na auditoria com o ator api_credential
// (ADR-059). Sem o escopo, ou em outra organização, nada é alcançado.
func TestPortalLinkRevocationByAPICredential(t *testing.T) {
	a := newAPI(t)
	tn, tnB := apiPlanTenant(t, a, "links-credencial"), apiPlanTenant(t, a, "links-credencial-b")
	cust := a.env.NewCustomer(t, tn)
	owner := a.login(tn.Owner, &tn.ID)
	credID, _, secret := createCredential(t, owner, "customers.portal_links")
	_, _, reader := createCredential(t, owner, "customers.read")
	_, _, foreign := createCredential(t, a.login(tnB.Owner, &tnB.ID), "customers.portal_links")
	base := "/api/v1/customers/" + cust.ID.String() + "/portal-links"

	r := a.raw(http.MethodPost, base, nil, bearer(secret))
	if r.status != http.StatusCreated {
		t.Fatalf("geração por credencial: %d %v", r.status, r.body)
	}
	linkID, _ := r.body["id"].(string)
	cookie, _ := openPortal(t, a, portalToken(t, r.body))

	list := a.raw(http.MethodGet, base, nil, bearer(secret))
	if list.status != http.StatusOK {
		t.Fatalf("lista por credencial: %d %v", list.status, list.body)
	}
	assertSchema(t, "PortalLinkPage", list.body)
	items := objects(t, list.body["data"])
	by, _ := items[0]["created_by"].(map[string]any)
	if len(items) != 1 || items[0]["id"] != linkID || items[0]["status"] != "used" || by["type"] != "api_credential" || by["id"] != credID ||
		by["name"] != "ERP interno" {
		t.Fatalf("link gerado por credencial: %v", items)
	}
	// Escopo insuficiente e credencial de outra organização.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		path := base
		if method == http.MethodDelete {
			path += "/" + linkID
		}
		if r := a.raw(method, path, nil, bearer(reader)); r.status != http.StatusForbidden || r.body["code"] != "forbidden" {
			t.Fatalf("%s sem o escopo: %d %v", method, r.status, r.body)
		}
		if r := a.raw(method, path, nil, bearer(foreign)); r.status != http.StatusNotFound || r.body["code"] != "customer_not_found" {
			t.Fatalf("%s por credencial de outra organização: %d %v", method, r.status, r.body)
		}
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie); r.status != http.StatusOK {
		t.Fatalf("a sessão do portal deveria continuar aberta: %d %v", r.status, r.body)
	}

	for range 2 {
		if r := a.raw(http.MethodDelete, base+"/"+linkID, nil, bearer(secret)); r.status != http.StatusNoContent {
			t.Fatalf("revogação por credencial: %d %v", r.status, r.body)
		}
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie); r.status != http.StatusUnauthorized {
		t.Fatalf("sessão do link revogado pela credencial: %d %v", r.status, r.body)
	}
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.portal_link_revoked&resource_id="+cust.ID.String(), nil)
	ev := objects(t, events["data"])
	if st != http.StatusOK || len(ev) != 1 || ev[0]["actor_type"] != "api_credential" || ev[0]["actor_id"] != credID {
		t.Fatalf("auditoria da revogação por credencial: %d %v", st, events)
	}
	// As credenciais nunca abrem o portal, e a sessão do portal nunca revoga links.
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, bearer(secret)); r.status != http.StatusUnauthorized {
		t.Fatalf("credencial no portal: %d %v", r.status, r.body)
	}
}

// flakyPortalSessions falha a remoção de sessões por link enquanto fail
// estiver ligado; as demais operações usam o Redis real.
type flakyPortalSessions struct {
	portal.SessionStore
	fail atomic.Bool
}

func (f *flakyPortalSessions) DeleteByLink(ctx context.Context, linkID uuid.UUID) error {
	if f.fail.Load() {
		return errors.New("redis indisponível (simulado)")
	}
	return f.SessionStore.DeleteByLink(ctx, linkID)
}

// A revogação vale mesmo quando a remoção das sessões no Redis falha: a rota
// responde 204, o link fica revogado e auditado, a sessão é recusada na
// requisição seguinte e a repetição da chamada conclui a limpeza.
func TestPortalLinkRevocationSurvivesSessionStoreFailure(t *testing.T) {
	flaky := &flakyPortalSessions{}
	a := newAPI(t, func(s *apiSetup) {
		s.portalSessions = func(inner portal.SessionStore) portal.SessionStore {
			flaky.SessionStore = inner
			return flaky
		}
	})
	tn := a.env.NewTenant(t, "links-redis")
	cust := a.env.NewCustomer(t, tn)
	owner := a.login(tn.Owner, &tn.ID)
	linkID, token := newPortalLink(t, owner, cust.ID)
	cookie, _ := openPortal(t, a, token)
	path := "/api/v1/customers/" + cust.ID.String() + "/portal-links/" + linkID

	flaky.fail.Store(true)
	if st, body, _ := owner.do(http.MethodDelete, path, nil); st != http.StatusNoContent {
		t.Fatalf("revogação com falha na limpeza das sessões: %d %v", st, body)
	}
	if items, _ := listPortalLinks(t, owner, cust.ID, ""); len(items) != 1 || items[0]["status"] != "revoked" {
		t.Fatalf("o link deveria estar revogado: %v", items)
	}
	if n := len(a.portalKeys(t, "portal_session:*")); n != 1 {
		t.Fatalf("cenário inválido: a sessão deveria continuar no Redis (%d)", n)
	}
	// Mesmo guardada, a sessão não é aceita; a recusa também a remove.
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices", nil, nil, cookie); r.status != http.StatusUnauthorized ||
		r.body["code"] != "portal_session_invalid" {
		t.Fatalf("sessão de link revogado: %d %v", r.status, r.body)
	}
	if n := len(a.portalKeys(t, "portal_session:*")); n != 0 {
		t.Fatalf("a sessão recusada deveria ter sido removida (%d)", n)
	}
	// A repetição conclui a limpeza (o índice do link) sem nova auditoria.
	if keys := a.portalKeys(t, "link_sessions:"+linkID); len(keys) != 1 {
		t.Fatalf("cenário inválido: o índice do link deveria continuar no Redis: %v", keys)
	}
	flaky.fail.Store(false)
	if st, body, _ := owner.do(http.MethodDelete, path, nil); st != http.StatusNoContent {
		t.Fatalf("revogação repetida: %d %v", st, body)
	}
	if keys := a.portalKeys(t, "*"); len(keys) != 0 {
		t.Fatalf("nada do portal deveria restar no Redis: %v", keys)
	}
	st, events, _ := owner.do(http.MethodGet, "/api/v1/audit-events?action=customer.portal_link_revoked&resource_id="+cust.ID.String(), nil)
	if st != http.StatusOK || len(objects(t, events["data"])) != 1 {
		t.Fatalf("auditoria da revogação: %d %v", st, events)
	}
}

// A sessão do portal não alcança as rotas de links do painel.
func TestPortalSessionCannotManageLinks(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "links-portal")
	cust := a.env.NewCustomer(t, tn)
	linkID, token := newPortalLink(t, a.login(tn.Owner, &tn.ID), cust.ID)
	cookie, csrf := openPortal(t, a, token)
	headers := map[string]string{"X-CSRF-Token": csrf, "X-Recorvia-Tenant": tn.ID.String()}
	base := "/api/v1/customers/" + cust.ID.String() + "/portal-links"
	for method, path := range map[string]string{http.MethodGet: base, http.MethodDelete: base + "/" + linkID, http.MethodPost: base} {
		if r := a.raw(method, path, nil, headers, cookie); r.status != http.StatusUnauthorized {
			t.Fatalf("%s %s com o cookie do portal: %d %v", method, path, r.status, r.body)
		}
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/session", nil, nil, cookie); r.status != http.StatusOK {
		t.Fatalf("a sessão do portal deveria continuar aberta: %d %v", r.status, r.body)
	}
}

// insertPortalInvoice grava uma fatura emitida do cliente no instante dado.
func insertPortalInvoice(t *testing.T, a *apiEnv, tn fixture.Tenant, customerID uuid.UUID, number string, issuedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	id := ids.New()
	err := a.env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, number, status, kind, issued_at, due_at, subtotal, total, amount_due,
			customer_snapshot) VALUES ($1, $2, $3, $4, 'open', 'manual', $5, $6, 1000, 1000, 1000, '{}')`,
			id, tn.ID, customerID, number, issuedAt, issuedAt.Add(10*24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

// portalInvoicePage lê uma página da listagem do portal e a valida contra o contrato.
func portalInvoicePage(t *testing.T, a *apiEnv, cookie *http.Cookie, query string) (ids []string, next string) {
	t.Helper()
	r := a.raw(http.MethodGet, "/api/portal/v1/invoices"+query, nil, nil, cookie)
	if r.status != http.StatusOK {
		t.Fatalf("listagem do portal%s: %d %v", query, r.status, r.body)
	}
	assertSchema(t, "PortalInvoicePage", r.body)
	for _, inv := range objects(t, r.body["data"]) {
		assertSchema(t, "PortalInvoice", inv)
		ids = append(ids, inv["id"].(string))
	}
	next, _ = r.body["next_cursor"].(string)
	return ids, next
}

// portalInvoiceWalk percorre a listagem do portal a partir de query até o fim.
func portalInvoiceWalk(t *testing.T, a *apiEnv, cookie *http.Cookie, limit int, cursor string) []string {
	t.Helper()
	var all []string
	for range 60 {
		query := fmt.Sprintf("?limit=%d", limit)
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page, next := portalInvoicePage(t, a, cookie, query)
		if len(page) > limit {
			t.Fatalf("página com %d itens excede o limite %d", len(page), limit)
		}
		all = append(all, page...)
		if next == "" {
			return all
		}
		cursor = next
	}
	t.Fatal("a paginação não termina")
	return nil
}

// Paginação das faturas do portal pela API: limit, cursor e next_cursor no
// padrão das demais listagens, estável sob inserções e sempre restrita ao
// cliente e à organização da sessão, qualquer que seja o cursor recebido.
func TestPortalInvoicePaginationOverHTTP(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := a.env.NewTenant(t, "portal-pagina-a"), a.env.NewTenant(t, "portal-pagina-b")
	cust, other, custB := a.env.NewCustomer(t, tnA), a.env.NewCustomer(t, tnA), a.env.NewCustomer(t, tnB)
	base := a.env.Clock.Now().Add(-72 * time.Hour).Truncate(time.Microsecond)
	const total = 27
	own := map[string]bool{}
	var want []string
	for i := range total {
		at := base.Add(time.Duration(i) * time.Minute)
		id := insertPortalInvoice(t, a, tnA, cust.ID, fmt.Sprintf("PAG-%04d", i), at)
		own[id] = true
		want = append([]string{id}, want...)
		if i < 6 {
			insertPortalInvoice(t, a, tnA, other.ID, fmt.Sprintf("OUT-%04d", i), at)
			insertPortalInvoice(t, a, tnB, custB.ID, fmt.Sprintf("B-%04d", i), at)
		}
	}
	addDraftInvoice(t, a, tnA, cust.ID)
	owner, ownerB := a.login(tnA.Owner, &tnA.ID), a.login(tnB.Owner, &tnB.ID)
	linkID, token := newPortalLink(t, owner, cust.ID)
	cookie, _ := openPortal(t, a, token)
	otherCookie, _ := openPortal(t, a, createPortalLink(t, owner, other.ID))
	cookieB, _ := openPortal(t, a, createPortalLink(t, ownerB, custB.ID))

	// Sem parâmetros: a página padrão tem 25 itens e indica a continuação.
	first, next := portalInvoicePage(t, a, cookie, "")
	if len(first) != 25 || next == "" || !slices.Equal(first, want[:25]) {
		t.Fatalf("página padrão: %d itens, cursor %q", len(first), next)
	}
	if rest, end := portalInvoicePage(t, a, cookie, "?cursor="+next); !slices.Equal(rest, want[25:]) || end != "" {
		t.Fatalf("última página: %v, cursor %q", rest, end)
	}
	for _, limit := range []int{1, 10, 27, 100} {
		if got := portalInvoiceWalk(t, a, cookie, limit, ""); !slices.Equal(got, want) {
			t.Fatalf("limite %d: %d faturas fora da ordem ou repetidas: %v", limit, len(got), got)
		}
	}

	// Fatura emitida entre as páginas não desloca as seguintes.
	page, cursor := portalInvoicePage(t, a, cookie, "?limit=10")
	newest := insertPortalInvoice(t, a, tnA, cust.ID, "PAG-NOVA", base.Add(48*time.Hour))
	own[newest] = true
	if rest := portalInvoiceWalk(t, a, cookie, 10, cursor); !slices.Equal(append(slices.Clone(page), rest...), want) {
		t.Fatalf("páginas depois da emissão de nova fatura: %v", rest)
	}
	if again := portalInvoiceWalk(t, a, cookie, 10, ""); len(again) != total+1 || again[0] != newest {
		t.Fatalf("ao recomeçar, a fatura nova vem primeiro: %v", again)
	}

	// Isolamento do cursor: o de outro cliente ou de outra organização, usado
	// nesta sessão, só percorre faturas deste cliente; e o deste cliente, nas
	// outras sessões, não revela nenhuma fatura dele.
	for name, c := range map[string]*http.Cookie{"outro cliente": otherCookie, "outra organização": cookieB} {
		foreignPage, foreignCursor := portalInvoicePage(t, a, c, "?limit=2")
		if len(foreignPage) != 2 || foreignCursor == "" {
			t.Fatalf("%s: página %v, cursor %q", name, foreignPage, foreignCursor)
		}
		for _, id := range portalInvoiceWalk(t, a, c, 100, "") {
			if own[id] {
				t.Fatalf("a sessão de %s lista fatura do cliente: %s", name, id)
			}
		}
		reached := portalInvoiceWalk(t, a, cookie, 5, foreignCursor)
		if len(reached) == 0 {
			t.Fatalf("cursor de %s: esperava as faturas mais antigas do próprio cliente", name)
		}
		for _, id := range reached {
			if !own[id] {
				t.Fatalf("cursor de %s alcançou fatura alheia: %s", name, id)
			}
		}
		for _, id := range portalInvoiceWalk(t, a, c, 5, cursor) {
			if own[id] {
				t.Fatalf("cursor do cliente na sessão de %s revelou fatura dele: %s", name, id)
			}
		}
	}

	for name, c := range map[string]struct{ query, code string }{
		"cursor malformado":      {"?cursor=nao-e-cursor", "invalid_cursor"},
		"limite zero":            {"?limit=0", "invalid_limit"},
		"limite acima do máximo": {"?limit=101", "invalid_limit"},
		"limite não numérico":    {"?limit=dez", "invalid_limit"},
	} {
		if r := a.raw(http.MethodGet, "/api/portal/v1/invoices"+c.query, nil, nil, cookie); r.status != http.StatusUnprocessableEntity || r.body["code"] != c.code {
			t.Fatalf("%s: %d %v", name, r.status, r.body)
		}
	}
	// O cursor da lista de links do painel não serve para as faturas do portal.
	newPortalLink(t, owner, other.ID)
	if _, linkCursor := listPortalLinks(t, owner, other.ID, "?limit=1"); linkCursor == "" {
		t.Fatal("esperava cursor na lista de links")
	} else if r := a.raw(http.MethodGet, "/api/portal/v1/invoices?cursor="+linkCursor, nil, nil, cookie); r.status != http.StatusUnprocessableEntity ||
		r.body["code"] != "invalid_cursor" {
		t.Fatalf("cursor de outra listagem: %d %v", r.status, r.body)
	}
	// Revogado o link, o cursor guardado pelo cliente não dá acesso a nada.
	if st, body, _ := owner.do(http.MethodDelete, "/api/v1/customers/"+cust.ID.String()+"/portal-links/"+linkID, nil); st != http.StatusNoContent {
		t.Fatalf("revogação: %d %v", st, body)
	}
	if r := a.raw(http.MethodGet, "/api/portal/v1/invoices?cursor="+cursor, nil, nil, cookie); r.status != http.StatusUnauthorized ||
		r.body["code"] != "portal_session_invalid" {
		t.Fatalf("cursor com a sessão revogada: %d %v", r.status, r.body)
	}
}
