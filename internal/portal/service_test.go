package portal

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

var testNow = time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)

const origin = "ip:203.0.113.7"

type env struct {
	tt       *testing.T
	store    *fakeStore
	sessions *fakeSessions
	counter  *fakeCounter
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	customer customers.Customer
	creator  Creator
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{tt: t, store: newFakeStore(), sessions: newFakeSessions(), counter: newFakeCounter(), clock: clock.NewManual(testNow)}
	e.svc = NewService(e.store, e.sessions, e.counter, e.clock, DefaultPolicy())
	e.tenantID = e.addTenant("Acme Serviços")
	e.customer = e.addCustomer(e.tenantID, "Cliente Exemplo")
	uid := uuid.New()
	e.creator = Creator{Actor: audit.User(uid, "Ana"), UserID: &uid}
	return e
}

func (e *env) addTenant(name string) uuid.UUID {
	id := uuid.New()
	d := newTenantData()
	d.tenant = &tenancy.Tenant{ID: id, Name: name, Status: tenancy.TenantActive}
	e.store.tenants[id] = d
	return id
}

func (e *env) addCustomer(tenantID uuid.UUID, name string) customers.Customer {
	c := customers.Customer{ID: uuid.New(), TenantID: tenantID, Name: name, Status: customers.Active}
	e.store.tenants[tenantID].customers[c.ID] = c
	return c
}

func (e *env) d() *tenantData { return e.store.tenants[e.tenantID] }

func (e *env) t() *testing.T { return e.tt }

func (e *env) link(t *testing.T) (Link, string) {
	t.Helper()
	l, token, err := e.svc.CreateLink(context.Background(), e.tenantID, e.creator, e.customer.ID)
	if err != nil {
		t.Fatal(err)
	}
	return l, token
}

func (e *env) exchange(t *testing.T, token string) Session {
	t.Helper()
	s, err := e.svc.Exchange(context.Background(), token, origin)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertErr(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Kind != kind || (code != "" && e.Code != code) {
		t.Fatalf("erro = %v, esperado %v/%s", err, kind, code)
	}
}

func TestLinkState(t *testing.T) {
	exp := testNow.Add(time.Hour)
	used, revoked := testNow, testNow
	cases := []struct {
		name string
		l    Link
		at   time.Time
		want LinkState
	}{
		{"pendente", Link{ExpiresAt: exp}, testNow, LinkPending},
		{"expirado no instante exato", Link{ExpiresAt: exp}, exp, LinkExpired},
		{"usado", Link{ExpiresAt: exp, UsedAt: &used}, exp.Add(time.Hour), LinkUsed},
		{"revogado prevalece sobre uso", Link{ExpiresAt: exp, UsedAt: &used, RevokedAt: &revoked}, testNow, LinkRevoked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.State(tc.at); got != tc.want {
				t.Fatalf("State = %s, esperado %s", got, tc.want)
			}
		})
	}
}

func TestSessionCSRFAndActor(t *testing.T) {
	s := Session{CustomerID: uuid.New(), CSRFToken: "token-da-sessao"}
	if !s.ValidCSRF("token-da-sessao") || s.ValidCSRF("") || s.ValidCSRF("outro") || (Session{}).ValidCSRF("") {
		t.Fatal("validação CSRF inesperada")
	}
	if a := s.Actor(); a.Type != audit.ActorCustomerPortal || a.ID != s.CustomerID.String() {
		t.Fatalf("ator: %+v", a)
	}
}

func TestValidLinkToken(t *testing.T) {
	token, _ := tenancy.NewToken()
	cases := map[string]bool{
		token:                         true,
		"":                            false,
		token[:42]:                    false,
		token + "A":                   false,
		strings.Repeat("*", 43):       false,
		token[:42] + "=":              false,
		strings.Repeat("A", 42) + "B": false, // bits finais não nulos (codificação não canônica)
	}
	for in, want := range cases {
		if got := validLinkToken(in); got != want {
			t.Errorf("validLinkToken(%q) = %v, esperado %v", in, got, want)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("política padrão: %v", err)
	}
	cases := map[string]func(*Policy){
		"link curto":         func(p *Policy) { p.LinkTTL = time.Minute },
		"link longo":         func(p *Policy) { p.LinkTTL = MaxLinkTTL + time.Second },
		"sessão sem duração": func(p *Policy) { p.SessionTTL = 0 },
		"inatividade acima":  func(p *Policy) { p.SessionIdle = p.SessionTTL + time.Second },
		"renovação acima":    func(p *Policy) { p.TouchInterval = p.SessionIdle },
		"limite negativo":    func(p *Policy) { p.MaxFailures = -1 },
		"limite sem janela":  func(p *Policy) { p.FailureWindow = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := DefaultPolicy()
			mutate(&p)
			if p.Validate() == nil {
				t.Fatal("política inválida aceita")
			}
		})
	}
	svc := NewService(nil, nil, nil, clock.NewManual(testNow), DefaultPolicy())
	if svc.Policy() != DefaultPolicy() {
		t.Fatal("Policy deve devolver a política configurada")
	}
}

func TestCreateLinkStoresOnlyHashAndSupersedesPrevious(t *testing.T) {
	e := newEnv(t)
	first, firstToken := e.link(t)
	if !validLinkToken(firstToken) || !bytes.Equal(first.TokenHash, tenancy.HashToken(firstToken)) {
		t.Fatal("token devolvido fora do formato ou hash divergente")
	}
	stored := e.d().links[first.ID]
	if bytes.Contains(stored.TokenHash, []byte(firstToken)) || !stored.ExpiresAt.Equal(testNow.Add(72*time.Hour)) ||
		stored.CreatedByUser == nil || stored.CreatedByCredential != nil {
		t.Fatalf("link gravado: %+v", stored)
	}
	a := e.d().audits[0]
	if a.Action != "customer.portal_link_created" || a.ResourceID != e.customer.ID.String() || a.Actor != e.creator.Actor || a.Details["superseded_links"] != 0 {
		t.Fatalf("auditoria: %+v", a)
	}
	e.clock.Advance(time.Minute)
	second, _ := e.link(t)
	if prev := e.d().links[first.ID]; prev.RevokedAt == nil || prev.RevokeReason != RevokeSuperseded {
		t.Fatalf("link anterior deveria ser substituído: %+v", prev)
	}
	if e.d().links[second.ID].RevokedAt != nil || e.d().audits[1].Details["superseded_links"] != 1 {
		t.Fatal("novo link deve ficar ativo e a auditoria registra a substituição")
	}
	if _, err := e.svc.Exchange(context.Background(), firstToken, origin); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("link substituído aceito: %v", err)
	}
}

func TestCreateLinkByAPICredential(t *testing.T) {
	e := newEnv(t)
	cid := uuid.New()
	l, _, err := e.svc.CreateLink(context.Background(), e.tenantID, Creator{Actor: audit.APICredential(cid, "ERP"), CredentialID: &cid}, e.customer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.CreatedByCredential == nil || *l.CreatedByCredential != cid || l.CreatedByUser != nil {
		t.Fatalf("autoria: %+v", l)
	}
	if a := e.d().audits[0]; a.Actor.Type != audit.ActorAPICredential {
		t.Fatalf("ator da auditoria: %+v", a.Actor)
	}
}

func TestCreateLinkRejections(t *testing.T) {
	e := newEnv(t)
	uid, cid := uuid.New(), uuid.New()
	for _, c := range []Creator{{}, {UserID: &uid, CredentialID: &cid}} {
		if _, _, err := e.svc.CreateLink(context.Background(), e.tenantID, c, e.customer.ID); err == nil {
			t.Fatal("autor inválido aceito")
		}
	}
	_, _, err := e.svc.CreateLink(context.Background(), e.tenantID, e.creator, uuid.New())
	assertErr(t, err, apperr.KindNotFound, "customer_not_found")
	// Cliente de outro tenant não é encontrado no tenant da requisição.
	other := e.addTenant("Outra")
	foreign := e.addCustomer(other, "Cliente de outro tenant")
	_, _, err = e.svc.CreateLink(context.Background(), e.tenantID, e.creator, foreign.ID)
	assertErr(t, err, apperr.KindNotFound, "customer_not_found")
	archived := e.customer
	archived.Status = customers.Archived
	e.d().customers[archived.ID] = archived
	_, _, err = e.svc.CreateLink(context.Background(), e.tenantID, e.creator, archived.ID)
	assertErr(t, err, apperr.KindConflict, "customer_archived")
	if len(e.d().links) != 0 || len(e.d().audits) != 0 {
		t.Fatal("rejeições não gravam links nem auditoria")
	}
	for _, op := range []string{"LockCustomer", "RevokeActiveLinks", "InsertLink", "Audit"} {
		f := newEnv(t)
		f.store.fail[op] = errBoom
		if _, _, err := f.svc.CreateLink(context.Background(), f.tenantID, f.creator, f.customer.ID); !errors.Is(err, errBoom) {
			t.Fatalf("%s: %v", op, err)
		}
	}
}

func TestExchangeCreatesRestrictedSession(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	e.clock.Advance(10 * time.Minute)
	s := e.exchange(t, token)
	if s.TenantID != e.tenantID || s.CustomerID != e.customer.ID || s.LinkID != l.ID || s.ID == "" || s.CSRFToken == "" || s.ID == s.CSRFToken {
		t.Fatalf("sessão: %+v", s)
	}
	if !s.ExpiresAt.Equal(e.clock.Now().Add(2 * time.Hour)) {
		t.Fatalf("expiração absoluta: %v", s.ExpiresAt)
	}
	saved := e.sessions.items[s.ID]
	if saved.ttl != 30*time.Minute {
		t.Fatalf("TTL no armazenamento deve ser a inatividade: %v", saved.ttl)
	}
	if used := e.d().links[l.ID].UsedAt; used == nil || !used.Equal(e.clock.Now()) {
		t.Fatalf("link não marcado como usado: %v", used)
	}
	a := e.d().audits[len(e.d().audits)-1]
	if a.Action != "customer_portal.session_started" || a.Actor.Type != audit.ActorCustomerPortal || a.Actor.ID != e.customer.ID.String() {
		t.Fatalf("auditoria: %+v", a)
	}
}

func TestExchangeRejectsReusedExpiredAndRevokedLinks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, l Link, token string) string
	}{
		{"usado duas vezes", func(e *env, _ Link, token string) string { e.exchange(e.t(), token); return token }},
		{"expirado", func(e *env, _ Link, token string) string { e.clock.Advance(72 * time.Hour); return token }},
		{"revogado", func(e *env, l Link, token string) string {
			if _, err := e.svc.RevokeLink(context.Background(), e.tenantID, e.creator.Actor, e.customer.ID, l.ID); err != nil {
				e.t().Fatal(err)
			}
			return token
		}},
		{"desconhecido", func(*env, Link, string) string { t, _ := tenancy.NewToken(); return t }},
		{"malformado", func(*env, Link, string) string { return "token-invalido" }},
		{"cliente arquivado", func(e *env, _ Link, token string) string {
			c := e.customer
			c.Status = customers.Archived
			e.d().customers[c.ID] = c
			return token
		}},
		{"cliente removido", func(e *env, _ Link, token string) string { delete(e.d().customers, e.customer.ID); return token }},
		{"organização suspensa", func(e *env, _ Link, token string) string { e.d().tenant.Status = tenancy.TenantSuspended; return token }},
		{"organização ausente", func(e *env, _ Link, token string) string { e.d().tenant = nil; return token }},
		{"link ausente no tenant", func(e *env, _ Link, token string) string {
			// Localizado pela busca global, mas inexistente no contexto do tenant.
			e.store.fail["Link"] = apperr.NotFound("portal_link_not_found", "Link não encontrado.")
			return token
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l, token := e.link(t)
			presented := tc.setup(e, l, token)
			sessionsBefore := len(e.sessions.items)
			_, err := e.svc.Exchange(context.Background(), presented, origin)
			assertErr(t, err, apperr.KindUnauthenticated, "portal_link_invalid")
			if len(e.sessions.items) != sessionsBefore {
				t.Fatal("tentativa recusada não cria sessão")
			}
			if e.counter.counts["portal_access:"+origin] != 1 || e.counter.windows["portal_access:"+origin] != 15*time.Minute {
				t.Fatalf("falha não contabilizada: %v", e.counter.counts)
			}
		})
	}
}

// Link usado duas vezes ao mesmo tempo: a segunda troca encontra o link
// marcado pela primeira (bloqueio da linha no banco) e é recusada.
func TestExchangeConcurrentUseRejectedByStore(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	e.store.fail["MarkLinkUsed"] = apperr.Conflict("portal_link_unavailable", "O link já foi utilizado, revogado ou expirou.")
	_, err := e.svc.Exchange(context.Background(), token, origin)
	assertErr(t, err, apperr.KindConflict, "portal_link_unavailable")
	if e.d().links[l.ID].UsedAt != nil || len(e.sessions.items) != 0 {
		t.Fatal("conflito não pode consumir o link nem criar sessão")
	}
}

func TestExchangeSessionStoreFailureKeepsLinkUsable(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	e.sessions.saveErr = errBoom
	_, err := e.svc.Exchange(context.Background(), token, origin)
	assertErr(t, err, apperr.KindUnavailable, "session_store_unavailable")
	if e.d().links[l.ID].UsedAt != nil {
		t.Fatal("falha ao gravar a sessão não pode consumir o link")
	}
	e.sessions.saveErr = nil
	if s := e.exchange(t, token); s.LinkID != l.ID {
		t.Fatal("o link continua utilizável depois da falha")
	}
}

func TestExchangeCommitFailureDiscardsSession(t *testing.T) {
	e := newEnv(t)
	l, token := e.link(t)
	e.store.commitErr = errBoom
	if _, err := e.svc.Exchange(context.Background(), token, origin); !errors.Is(err, errBoom) {
		t.Fatalf("erro do commit: %v", err)
	}
	if len(e.sessions.items) != 0 || e.sessions.deletes != 1 {
		t.Fatal("sessão gravada antes do commit com falha deve ser descartada")
	}
	if e.d().links[l.ID].UsedAt != nil {
		t.Fatal("sem commit, o link não é consumido")
	}
	if e.counter.counts["portal_access:"+origin] != 0 {
		t.Fatal("falha de infraestrutura não conta como tentativa inválida")
	}
}

func TestExchangeStoreFailures(t *testing.T) {
	for _, op := range []string{"FindLink", "Link", "Tenant", "Customer", "MarkLinkUsed", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			_, token := e.link(t)
			e.store.fail[op] = errBoom
			if _, err := e.svc.Exchange(context.Background(), token, origin); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v", err)
			}
			if len(e.sessions.items) != 0 {
				t.Fatal("falha não cria sessão")
			}
		})
	}
}

func TestExchangeAttemptLimit(t *testing.T) {
	e := newEnv(t)
	_, token := e.link(t)
	for range DefaultPolicy().MaxFailures {
		_, err := e.svc.Exchange(context.Background(), "token-invalido", origin)
		assertErr(t, err, apperr.KindUnauthenticated, "portal_link_invalid")
	}
	_, err := e.svc.Exchange(context.Background(), token, origin)
	assertErr(t, err, apperr.KindRateLimited, "portal_access_attempts_exceeded")
	if l := e.d().links; len(l) != 1 {
		t.Fatal("estado inesperado")
	}
	// Outra origem continua podendo usar o link.
	if _, err := e.svc.Exchange(context.Background(), token, "ip:198.51.100.1"); err != nil {
		t.Fatalf("outra origem: %v", err)
	}
	// Falha do contador não bloqueia nem impede a verificação.
	f := newEnv(t)
	_, token = f.link(t)
	f.counter.readErr, f.counter.recErr = errBoom, errBoom
	if _, err := f.svc.Exchange(context.Background(), "token-invalido", origin); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("token inválido: %v", err)
	}
	if _, err := f.svc.Exchange(context.Background(), token, origin); err != nil {
		t.Fatalf("falha do contador não deve bloquear: %v", err)
	}
	// Sem contador, sem limitação.
	g := newEnv(t)
	g.svc = NewService(g.store, g.sessions, nil, g.clock, DefaultPolicy())
	for range DefaultPolicy().MaxFailures + 1 {
		if _, err := g.svc.Exchange(context.Background(), "token-invalido", origin); !apperr.IsKind(err, apperr.KindUnauthenticated) {
			t.Fatalf("sem contador: %v", err)
		}
	}
}

func TestAuthenticateSession(t *testing.T) {
	e := newEnv(t)
	_, token := e.link(t)
	s := e.exchange(t, token)
	saves := e.sessions.saves
	e.clock.Advance(30 * time.Second)
	got, err := e.svc.Authenticate(context.Background(), s.ID)
	if err != nil || got.ID != s.ID || got.CustomerID != e.customer.ID {
		t.Fatalf("sessão: %+v %v", got, err)
	}
	if e.sessions.saves != saves {
		t.Fatal("dentro do intervalo de renovação a sessão não é regravada")
	}
	e.clock.Advance(time.Minute)
	got, err = e.svc.Authenticate(context.Background(), s.ID)
	if err != nil || !got.LastSeenAt.Equal(e.clock.Now()) || e.sessions.saves != saves+1 {
		t.Fatalf("renovação: %+v %v", got, err)
	}
	if ttl := e.sessions.items[s.ID].ttl; ttl != 30*time.Minute {
		t.Fatalf("TTL renovado: %v", ttl)
	}
	// Perto do fim absoluto, a TTL não ultrapassa a expiração.
	e.clock.Set(s.ExpiresAt.Add(-10 * time.Minute))
	sess := e.sessions.items[s.ID]
	sess.s.LastSeenAt = e.clock.Now().Add(-2 * time.Minute)
	e.sessions.items[s.ID] = sess
	if _, err := e.svc.Authenticate(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if ttl := e.sessions.items[s.ID].ttl; ttl != 10*time.Minute {
		t.Fatalf("TTL limitada pela expiração absoluta: %v", ttl)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, s Session) string
		kind  apperr.Kind
		code  string
		gone  bool
	}{
		{"sem identificador", func(*env, Session) string { return "" }, apperr.KindUnauthenticated, "portal_session_invalid", false},
		{"desconhecida", func(*env, Session) string { return "sessao-desconhecida" }, apperr.KindUnauthenticated, "portal_session_invalid", false},
		{"inativa", func(e *env, s Session) string { e.clock.Advance(30 * time.Minute); return s.ID }, apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"expiração absoluta", func(e *env, s Session) string {
			v := e.sessions.items[s.ID]
			v.s.ExpiresAt = e.clock.Now()
			e.sessions.items[s.ID] = v
			return s.ID
		}, apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"link substituído por um novo", func(e *env, s Session) string { e.link(e.t()); return s.ID }, apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"cliente arquivado", func(e *env, s Session) string {
			c := e.customer
			c.Status = customers.Archived
			e.d().customers[c.ID] = c
			return s.ID
		}, apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"organização suspensa", func(e *env, s Session) string { e.d().tenant.Status = tenancy.TenantSuspended; return s.ID },
			apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"link inexistente", func(e *env, s Session) string { delete(e.d().links, s.LinkID); return s.ID },
			apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"cliente divergente do link", func(e *env, s Session) string {
			v := e.sessions.items[s.ID]
			v.s.CustomerID = e.addCustomer(e.tenantID, "Outro cliente").ID
			e.sessions.items[s.ID] = v
			return s.ID
		}, apperr.KindUnauthenticated, "portal_session_invalid", true},
		{"armazenamento indisponível", func(e *env, s Session) string { e.sessions.getErr = errBoom; return s.ID },
			apperr.KindUnavailable, "session_store_unavailable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, token := e.link(t)
			s := e.exchange(t, token)
			id := tc.setup(e, s)
			_, err := e.svc.Authenticate(context.Background(), id)
			assertErr(t, err, tc.kind, tc.code)
			if _, present := e.sessions.items[s.ID]; tc.gone == present {
				t.Fatalf("sessão removida = %v, esperado %v", !present, tc.gone)
			}
		})
	}
}

// Sessão de um tenant nunca é validada no contexto de outro: a transação é
// aberta com o tenant gravado na sessão e o link precisa existir nele.
func TestAuthenticateSessionBoundToTenant(t *testing.T) {
	e := newEnv(t)
	_, token := e.link(t)
	s := e.exchange(t, token)
	other := e.addTenant("Outra")
	v := e.sessions.items[s.ID]
	v.s.TenantID = other
	e.sessions.items[s.ID] = v
	_, err := e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")
}

func TestAuthenticateInfrastructureFailures(t *testing.T) {
	for _, op := range []string{"InTenant", "Link", "Tenant", "Customer"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			_, token := e.link(t)
			s := e.exchange(t, token)
			e.store.fail[op] = errBoom
			if _, err := e.svc.Authenticate(context.Background(), s.ID); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v", err)
			}
			if _, ok := e.sessions.items[s.ID]; !ok {
				t.Fatal("falha de infraestrutura não encerra a sessão")
			}
		})
	}
	e := newEnv(t)
	_, token := e.link(t)
	s := e.exchange(t, token)
	e.clock.Advance(2 * time.Minute)
	e.sessions.saveErr = errBoom
	_, err := e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnavailable, "session_store_unavailable")
}

func TestViewAndLogout(t *testing.T) {
	e := newEnv(t)
	contact := customers.Contact{ID: uuid.New(), CustomerID: e.customer.ID, Name: "Financeiro", Email: "fin@cliente.test", ReceivesBillingEmail: true}
	e.d().contacts[e.customer.ID] = []customers.Contact{contact}
	_, token := e.link(t)
	s := e.exchange(t, token)
	v, err := e.svc.View(context.Background(), s)
	if err != nil || v.CustomerName != "Cliente Exemplo" || v.OrganizationName != "Acme Serviços" || len(v.Contacts) != 1 || v.Contacts[0].ID != contact.ID {
		t.Fatalf("resumo: %+v %v", v, err)
	}
	for _, op := range []string{"Tenant", "Customer", "Contacts"} {
		e.store.fail[op] = errBoom
		if _, err := e.svc.View(context.Background(), s); !errors.Is(err, errBoom) {
			t.Fatalf("%s: %v", op, err)
		}
		e.store.fail[op] = nil
	}
	e.sessions.deleteErr = errBoom
	audits := len(e.d().audits)
	assertErr(t, e.svc.Logout(context.Background(), s), apperr.KindUnavailable, "session_store_unavailable")
	if len(e.d().audits) != audits {
		t.Fatal("encerramento sem remoção da sessão não é auditado")
	}
	e.sessions.deleteErr = nil
	if err := e.svc.Logout(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.sessions.items[s.ID]; ok {
		t.Fatal("sessão não removida")
	}
	if a := e.d().audits[len(e.d().audits)-1]; a.Action != "customer_portal.session_ended" || a.Actor.Type != audit.ActorCustomerPortal {
		t.Fatalf("auditoria: %+v", a)
	}
	_, err = e.svc.Authenticate(context.Background(), s.ID)
	assertErr(t, err, apperr.KindUnauthenticated, "portal_session_invalid")
	e.store.fail["Audit"] = errBoom
	if err := e.svc.Logout(context.Background(), s); !errors.Is(err, errBoom) {
		t.Fatalf("falha da auditoria: %v", err)
	}
}
