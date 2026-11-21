//go:build integration

package pgstore_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/ricartefelipe/recorvia/db"
	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/portal"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// memSessions guarda sessões do portal em memória (o Redis é exercitado nos
// testes da API); seguro para uso concorrente.
type memSessions struct {
	mu    sync.Mutex
	items map[string]portal.Session
}

func newMemSessions() *memSessions { return &memSessions{items: map[string]portal.Session{}} }

func (m *memSessions) Save(_ context.Context, s portal.Session, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[s.ID] = s
	return nil
}

func (m *memSessions) Get(_ context.Context, id string) (portal.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.items[id]
	if !ok {
		return portal.Session{}, portal.ErrSessionNotFound
	}
	return s, nil
}

func (m *memSessions) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

func (m *memSessions) DeleteByLink(_ context.Context, linkID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.items {
		if s.LinkID == linkID {
			delete(m.items, id)
		}
	}
	return nil
}

func (m *memSessions) has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.items[id]
	return ok
}

func portalEnv(t *testing.T, maxConns int32) (*fixture.Env, *portal.Service, *memSessions) {
	t.Helper()
	env := fixture.New(t, maxConns, now)
	sessions := newMemSessions()
	return env, portal.NewService(env.Store.Portal(), sessions, nil, env.Clock, portal.DefaultPolicy()), sessions
}

func ownerCreator(tn fixture.Tenant) portal.Creator {
	id := tn.Owner.ID
	return portal.Creator{Actor: tn.Actor, UserID: &id}
}

func TestPortalLinkSingleUseExpiryAndRevocation(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 4)
	tn := env.NewTenant(t, "portal")
	cust := env.NewCustomer(t, tn)

	l, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT token_hash FROM customer_portal_links WHERE id = $1`, l.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) == token || len(stored) != 32 {
		t.Fatal("somente o hash do token deve ser persistido")
	}
	sess, err := svc.Exchange(ctx, token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	if sess.TenantID != tn.ID || sess.CustomerID != cust.ID {
		t.Fatalf("sessão fora do escopo do link: %+v", sess)
	}
	// Token usado duas vezes.
	if _, err := svc.Exchange(ctx, token, "ip:teste"); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("segundo uso do link: %v", err)
	}
	if _, err := svc.Authenticate(ctx, sess.ID); err != nil {
		t.Fatalf("sessão válida recusada: %v", err)
	}
	// Um novo link revoga o anterior e encerra a sessão aberta com ele.
	_, expiring, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, sess.ID); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("sessão de link substituído continua válida: %v", err)
	}
	// Token expirado.
	env.Clock.Advance(portal.DefaultPolicy().LinkTTL)
	if _, err := svc.Exchange(ctx, expiring, "ip:teste"); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("link expirado aceito: %v", err)
	}
	var revoked int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT count(*) FROM customer_portal_links WHERE customer_id = $1 AND revoke_reason = 'superseded'`,
		cust.ID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("links substituídos: %d %v", revoked, err)
	}
	// A auditoria registra o ator customer_portal.
	var actorType string
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT actor_type FROM audit_log WHERE tenant_id = $1 AND action = 'customer_portal.session_started'`,
		tn.ID).Scan(&actorType); err != nil || actorType != string(audit.ActorCustomerPortal) {
		t.Fatalf("auditoria do portal: %q %v", actorType, err)
	}
}

// Trocas concorrentes do mesmo token: exatamente uma cria sessão.
func TestPortalLinkConcurrentExchange(t *testing.T) {
	ctx := context.Background()
	env, svc, sessions := portalEnv(t, 8)
	tn := env.NewTenant(t, "portal-conc")
	cust := env.NewCustomer(t, tn)
	_, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, errs[i] = svc.Exchange(ctx, token, "ip:teste") })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case apperr.IsKind(err, apperr.KindUnauthenticated), apperr.IsKind(err, apperr.KindConflict):
		default:
			t.Fatalf("erro inesperado: %v", err)
		}
	}
	if ok != 1 || len(sessions.items) != 1 {
		t.Fatalf("trocas aceitas: %d, sessões: %d", ok, len(sessions.items))
	}
}

// Gerações concorrentes de link para o mesmo cliente são serializadas: todas
// concluem e resta exatamente um link ativo.
func TestPortalLinkConcurrentCreation(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 8)
	tn := env.NewTenant(t, "portal-gen")
	cust := env.NewCustomer(t, tn)
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { _, _, errs[i] = svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("geração concorrente falhou: %v", err)
		}
	}
	var active, total int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL), count(*) FROM customer_portal_links WHERE customer_id = $1`,
		cust.ID).Scan(&active, &total); err != nil {
		t.Fatal(err)
	}
	if active != 1 || total != n {
		t.Fatalf("links ativos %d de %d", active, total)
	}
}

func TestPortalLinksAreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 4)
	a, b := env.NewTenant(t, "portal-a"), env.NewTenant(t, "portal-b")
	custA, custB := env.NewCustomer(t, a), env.NewCustomer(t, b)
	l, token, err := svc.CreateLink(ctx, a.ID, ownerCreator(a), custA.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Cliente de outro tenant não recebe link.
	if _, _, err := svc.CreateLink(ctx, a.ID, ownerCreator(a), custB.ID); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("link para cliente de outro tenant: %v", err)
	}
	// O link de A não é visível no contexto de B (RLS e filtro por tenant).
	err = env.Store.Portal().InTenant(ctx, b.ID, func(tx portal.Tx) error {
		_, err := tx.Link(ctx, l.ID, false)
		return err
	})
	if !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("link de A visto por B: %v", err)
	}
	// A sessão criada pelo link fica restrita ao tenant e ao cliente do link.
	sess, err := svc.Exchange(ctx, token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	if sess.TenantID != a.ID || sess.CustomerID != custA.ID {
		t.Fatalf("escopo da sessão: %+v", sess)
	}
	// FK composta impede link de A apontando para cliente de B.
	err = env.DB.InTenantTx(ctx, a.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO customer_portal_links (id, tenant_id, customer_id, token_hash, created_by_user_id, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, now(), now() + interval '1 hour')`, ids.New(), a.ID, custB.ID, tenancy.HashToken("x"), a.Owner.ID)
		return err
	})
	if !postgres.IsForeignKeyViolation(err) {
		t.Fatalf("esperava violação de FK composta: %v", err)
	}
}

func TestPortalLinkDatabaseGuards(t *testing.T) {
	ctx := context.Background()
	env, svc, _ := portalEnv(t, 2)
	tn := env.NewTenant(t, "portal-guard")
	cust := env.NewCustomer(t, tn)
	l, token, err := svc.CreateLink(ctx, tn.ID, ownerCreator(tn), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Exchange(ctx, token, "ip:teste"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		stmt  string
		check func(error) bool
	}{
		{"desfazer uso", `UPDATE customer_portal_links SET used_at = NULL WHERE tenant_id = $1 AND id = $2`, postgres.IsRaisedException},
		{"trocar token", `UPDATE customer_portal_links SET token_hash = sha256('outro'::bytea) WHERE tenant_id = $1 AND id = $2`, postgres.IsRaisedException},
		{"estender prazo", `UPDATE customer_portal_links SET expires_at = expires_at + interval '1 day' WHERE tenant_id = $1 AND id = $2`,
			postgres.IsRaisedException},
		{"excluir", `DELETE FROM customer_portal_links WHERE tenant_id = $1 AND id = $2`, postgres.IsInsufficientPrivilege},
		{"dois autores", `UPDATE customer_portal_links SET created_by_credential_id = gen_random_uuid() WHERE tenant_id = $1 AND id = $2`,
			postgres.IsRaisedException},
		{"revogar sem motivo", `UPDATE customer_portal_links SET revoked_at = now() WHERE tenant_id = $1 AND id = $2`, postgres.IsCheckViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.stmt, tn.ID, l.ID)
				return err
			})
			if !tc.check(err) {
				t.Fatalf("alteração proibida não recusada como esperado: %v", err)
			}
		})
	}
	// Sem contexto de tenant, a função de busca só devolve identificadores.
	var linkID, tenantID uuid.UUID
	if err := env.DB.Pool().QueryRow(ctx, `SELECT link_id, tenant_id FROM app_find_portal_link($1)`, tenancy.HashToken(token)).Scan(&linkID, &tenantID); err != nil ||
		linkID != l.ID || tenantID != tn.ID {
		t.Fatalf("busca do link: %v %v %v", linkID, tenantID, err)
	}
	var visible int
	if err := env.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM customer_portal_links`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("sem contexto, nenhuma linha deveria ser visível: %d %v", visible, err)
	}
}

// --- credenciais de API ---

func credentialsEnv(t *testing.T, maxConns int32) (*fixture.Env, *apicredentials.Service) {
	t.Helper()
	env := fixture.New(t, maxConns, now)
	return env, apicredentials.NewService(env.Store.APICredentials(), nil, env.Clock, apicredentials.DefaultPolicy())
}

func ownerGrantor(tn fixture.Tenant) apicredentials.Grantor {
	return apicredentials.Grantor{UserID: tn.Owner.ID, Role: tenancy.RoleOwner, Actor: tn.Actor}
}

func apiTenant(t *testing.T, env *fixture.Env, prefix string) fixture.Tenant {
	t.Helper()
	tn := env.NewTenant(t, prefix)
	if _, err := env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	return tn
}

// addMember associa um novo usuário à organização com o papel indicado.
func addMember(t *testing.T, env *fixture.Env, tn fixture.Tenant, role tenancy.Role) identity.User {
	t.Helper()
	ctx := context.Background()
	uid := ids.New()
	u, err := env.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@membro.test", EmailVerified: true, Name: "Membro"}, env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := env.Tenancy.Invite(ctx, tn.ID, tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner},
		tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}, u.Email, role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Tenancy.AcceptInvitation(ctx, tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}, token); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAPICredentialLifecycle(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 4)
	basic := env.NewTenant(t, "cred-basico")
	if _, err := svc.Create(ctx, basic.ID, ownerGrantor(basic), apicredentials.CreateInput{Name: "ERP", Scopes: []string{"customers.read"}}); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("plano sem api_access deveria recusar: %v", err)
	}
	tn := apiTenant(t, env, "cred")
	is, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "ERP interno", Scopes: []string{"customers.read", "customers.write"}})
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT secret_hash FROM api_credentials WHERE id = $1`, is.Credential.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	_, secret, _ := apicredentials.ParseToken(is.Token)
	if string(stored) == secret || len(stored) != 32 {
		t.Fatal("somente o hash do segredo deve ser persistido")
	}
	p, err := svc.Authenticate(ctx, is.Token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != tn.ID || !p.Can(tenancy.PermCustomersWrite) || p.Can(tenancy.PermInvoicesRead) {
		t.Fatalf("principal: %+v", p)
	}
	list, err := svc.List(ctx, tn.ID, 0, "")
	if err != nil || len(list.Items) != 1 || list.Items[0].LastUsedAt == nil || !list.Items[0].LastUsedAt.Equal(now) || list.NextCursor != "" {
		t.Fatalf("último uso: %+v %v", list, err)
	}
	// Rotação: o segredo anterior vale por 24 horas.
	env.Clock.Advance(time.Hour)
	rot, err := svc.Rotate(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, list.Items[0].Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{is.Token, rot.Token} {
		if _, err := svc.Authenticate(ctx, token, "ip:teste"); err != nil {
			t.Fatalf("token na janela de rotação: %v", err)
		}
	}
	env.Clock.Advance(apicredentials.RotationGrace)
	if _, err := svc.Authenticate(ctx, is.Token, "ip:teste"); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("segredo rotacionado após a janela: %v", err)
	}
	if _, err := svc.Authenticate(ctx, rot.Token, "ip:teste"); err != nil {
		t.Fatalf("segredo novo: %v", err)
	}
	// Revogação imediata, na versão gravada pela rotação; a versão anterior é
	// recusada pela condição do UPDATE.
	if err := svc.Revoke(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, rot.Credential.Version-1); !apperr.IsKind(err, apperr.KindPreconditionFailed) {
		t.Fatalf("revogação com versão anterior à rotação: %v", err)
	}
	if err := svc.Revoke(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, rot.Credential.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, rot.Token, "ip:teste"); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("credencial revogada aceita: %v", err)
	}
	if err := svc.Revoke(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, rot.Credential.Version+1); err != nil {
		t.Fatalf("revogação repetida: %v", err)
	}
	var actions []string
	rows, err := pgtest.OwnerPool(t).Query(ctx, `SELECT action FROM audit_log WHERE tenant_id = $1 AND resource_type = 'api_credential' ORDER BY occurred_at, id`, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(actions, []string{"api_credential.created", "api_credential.rotated", "api_credential.revoked"}) {
		t.Fatalf("auditoria: %v %v", actions, err)
	}
}

func TestAPICredentialIssuerMembershipAndScopes(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 4)
	tn := apiTenant(t, env, "cred-emissor")
	admin := addMember(t, env, tn, tenancy.RoleAdmin)
	g := apicredentials.Grantor{UserID: admin.ID, Role: tenancy.RoleAdmin, Actor: audit.User(admin.ID, admin.DisplayName)}
	is, err := svc.Create(ctx, tn.ID, g, apicredentials.CreateInput{Name: "Integração", Scopes: []string{"customers.write", "invoices.void"}})
	if err != nil {
		t.Fatal(err)
	}
	owner := tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner}
	ownerRef := tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}
	// Rebaixado a operador, o emissor reduz os escopos efetivos da credencial.
	changed, err := env.Tenancy.ChangeRole(ctx, tn.ID, owner, ownerRef, admin.ID, tenancy.RoleOperator, 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Authenticate(ctx, is.Token, "ip:teste")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Can(tenancy.PermCustomersWrite) || p.Can(tenancy.PermInvoicesVoid) {
		t.Fatalf("escopos efetivos após rebaixamento: %v", p.Scopes)
	}
	// Removido, a credencial deixa de autenticar até ser rotacionada por um membro ativo.
	if err := env.Tenancy.RemoveMember(ctx, tn.ID, owner, ownerRef, admin.ID, changed.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, is.Token, "ip:teste"); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("emissor removido: %v", err)
	}
	rot, err := svc.Rotate(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, is.Credential.Version)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := svc.Authenticate(ctx, rot.Token, "ip:teste"); err != nil || !p.Can(tenancy.PermInvoicesVoid) {
		t.Fatalf("após rotação pelo proprietário: %+v %v", p, err)
	}
}

func TestAPICredentialsAreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 4)
	a, b := apiTenant(t, env, "cred-a"), apiTenant(t, env, "cred-b")
	is, err := svc.Create(ctx, a.ID, ownerGrantor(a), apicredentials.CreateInput{Name: "ERP de A", Scopes: []string{"customers.read"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Rotate(ctx, b.ID, ownerGrantor(b), is.Credential.ID, is.Credential.Version); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("rotação a partir de outro tenant: %v", err)
	}
	if err := svc.Revoke(ctx, b.ID, ownerGrantor(b), is.Credential.ID, is.Credential.Version); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("revogação a partir de outro tenant: %v", err)
	}
	listB, err := svc.List(ctx, b.ID, 0, "")
	if err != nil || len(listB.Items) != 0 {
		t.Fatalf("lista de B contém credenciais de A: %v %v", listB, err)
	}
	p, err := svc.Authenticate(ctx, is.Token, "ip:teste")
	if err != nil || p.TenantID != a.ID {
		t.Fatalf("a credencial autentica somente no próprio tenant: %v %v", p.TenantID, err)
	}
	var visible int
	if err := env.DB.Pool().QueryRow(ctx, `SELECT count(*) FROM api_credentials`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("sem contexto, nenhuma credencial deveria ser visível: %d %v", visible, err)
	}
}

func TestAPICredentialDatabaseGuards(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 2)
	tn := apiTenant(t, env, "cred-guard")
	is, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "ERP", Scopes: []string{"customers.read"}})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(stmt string) error {
		return env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, tn.ID, is.Credential.ID)
			return err
		})
	}
	cases := []struct {
		name  string
		stmt  string
		check func(error) bool
	}{
		{"ampliar escopos", `UPDATE api_credentials SET scopes = scopes || '{invoices.void}' WHERE tenant_id = $1 AND id = $2`, postgres.IsRaisedException},
		{"trocar nome", `UPDATE api_credentials SET name = 'Outro nome' WHERE tenant_id = $1 AND id = $2`, postgres.IsRaisedException},
		{"estender validade", `UPDATE api_credentials SET expires_at = now() + interval '10 years' WHERE tenant_id = $1 AND id = $2`,
			postgres.IsRaisedException},
		{"escopo malformado", `INSERT INTO api_credentials (id, tenant_id, name, key_id, secret_hash, scopes, created_by, created_at)
			SELECT gen_random_uuid(), $1, 'Malformada', 'aaaaaaaaaaaaaaaa', sha256('x'::bytea), '{"Customers Read"}', created_by, now()
			  FROM api_credentials WHERE id = $2`, postgres.IsCheckViolation},
		{"excluir", `DELETE FROM api_credentials WHERE tenant_id = $1 AND id = $2`, postgres.IsInsufficientPrivilege},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := exec(tc.stmt); !tc.check(err) {
				t.Fatalf("alteração proibida não recusada como esperado: %v", err)
			}
		})
	}
	if err := svc.Revoke(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, is.Credential.Version); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE api_credentials SET revoked_at = NULL, revoked_by = NULL WHERE tenant_id = $1 AND id = $2`); !postgres.IsRaisedException(err) {
		t.Fatalf("desfazer revogação deveria ser recusado pelo banco: %v", err)
	}
	if err := exec(`UPDATE api_credentials SET last_used_at = now() WHERE tenant_id = $1 AND id = $2`); !postgres.IsRaisedException(err) {
		t.Fatalf("credencial revogada não pode ser alterada: %v", err)
	}
}

// Rotações concorrentes da mesma credencial, na mesma versão (If-Match), são
// serializadas pelo bloqueio da linha e decididas pela condição do UPDATE:
// somente uma é gravada, as demais respondem version_mismatch, e continuam
// válidos apenas o segredo novo e o anterior.
func TestAPICredentialConcurrentRotation(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 8)
	tn := apiTenant(t, env, "cred-rot")
	is, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "ERP", Scopes: []string{"customers.read"}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 4
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			r, err := svc.Rotate(ctx, tn.ID, ownerGrantor(tn), is.Credential.ID, is.Credential.Version)
			tokens[i], errs[i] = r.Token, err
		})
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case !apperr.IsKind(err, apperr.KindPreconditionFailed):
			t.Fatalf("rotação concorrente: %v", err)
		}
	}
	var version int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT version FROM api_credentials WHERE id = $1`, is.Credential.ID).Scan(&version); err != nil ||
		version != 2 || succeeded != 1 {
		t.Fatalf("rotações concorrentes na mesma versão: %d gravadas, versão %d %v", succeeded, version, err)
	}
	valid := 0
	for _, token := range append(tokens, is.Token) {
		if _, err := svc.Authenticate(ctx, token, "ip:teste"); err == nil {
			valid++
		}
	}
	if valid != 2 {
		t.Fatalf("segredos válidos após rotações: %d (esperado vigente e anterior)", valid)
	}
}

// holdingCredentials interrompe a primeira criação logo depois da contagem de
// credenciais ativas, com a transação ainda aberta, até release ser fechado.
type holdingCredentials struct {
	*pgstore.APICredentials
	counted chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func (s *holdingCredentials) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(apicredentials.Tx) error) error {
	return s.APICredentials.InTenant(ctx, tenantID, func(tx apicredentials.Tx) error { return fn(holdingCredentialsTx{Tx: tx, s: s}) })
}

type holdingCredentialsTx struct {
	apicredentials.Tx
	s *holdingCredentials
}

func (tx holdingCredentialsTx) CountActiveCredentials(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.Tx.CountActiveCredentials(ctx, now)
	if tx.s.held.CompareAndSwap(false, true) {
		close(tx.s.counted)
		<-tx.s.release
	}
	return n, err
}

// Duas criações simultâneas com uma vaga restante não ultrapassam o limite
// de credenciais ativas: a segunda espera o commit da primeira para contar e
// é recusada.
func TestAPICredentialConcurrentCreationRespectsActiveLimit(t *testing.T) {
	ctx := context.Background()
	env, plain := credentialsEnv(t, 4)
	tn := apiTenant(t, env, "cred-limite")
	in := apicredentials.CreateInput{Name: "Integração", Scopes: []string{"customers.read"}}
	for range apicredentials.MaxActivePerTenant - 1 {
		if _, err := plain.Create(ctx, tn.ID, ownerGrantor(tn), in); err != nil {
			t.Fatal(err)
		}
	}
	store := &holdingCredentials{APICredentials: env.Store.APICredentials(), counted: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	svc := apicredentials.NewService(store, nil, env.Clock, apicredentials.DefaultPolicy())

	first := make(chan error, 1)
	go func() {
		_, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), in)
		first <- err
	}()
	select {
	case <-store.counted:
	case err := <-first:
		t.Fatalf("a primeira criação terminou antes da contagem: %v", err)
	}
	var secondErr error
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_, secondErr = svc.Create(ctx, tn.ID, ownerGrantor(tn), in)
	}()
	// A segunda criação termina sozinha (sem serialização) ou fica aguardando
	// a transação da primeira.
	waitLockWaiters(t, pgtest.OwnerPool(t), 1, secondDone)
	release()
	firstErr := <-first
	<-secondDone

	if firstErr != nil {
		t.Fatalf("a primeira criação deveria ocupar a última vaga: %v", firstErr)
	}
	if e, ok := apperr.As(secondErr); !ok || e.Code != "api_credential_limit" {
		t.Fatalf("a segunda criação deveria ser recusada pelo limite: %v", secondErr)
	}
	var active int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT count(*)::int FROM api_credentials WHERE tenant_id = $1 AND revoked_at IS NULL`,
		tn.ID).Scan(&active); err != nil || active != apicredentials.MaxActivePerTenant {
		t.Fatalf("credenciais ativas: %d (limite %d) %v", active, apicredentials.MaxActivePerTenant, err)
	}
}

// randomKeyID gera um identificador público no formato das credenciais.
func randomKeyID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// Credenciais nunca são excluídas: a listagem é paginada por cursor, abre com
// as ativas (que podem ser revogadas pela interface) mesmo depois de mais
// credenciais revogadas e expiradas do que uma página, e percorre todas as
// demais, das mais recentes para as mais antigas.
func TestAPICredentialListKeepsActiveCredentialsBeyondListLimit(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 4)
	tn := apiTenant(t, env, "cred-lista")
	created := env.Clock.Now()
	old, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "ERP antigo", Scopes: []string{"customers.read"}})
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Minute)
	expiring, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: "Temporária", Scopes: []string{"customers.read"},
		ExpiresInDays: new(1)})
	if err != nil {
		t.Fatal(err)
	}
	const revoked = 200
	keys := make([]string, revoked)
	for i := range keys {
		keys[i] = randomKeyID(t)
	}
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO api_credentials (id, tenant_id, name, key_id, secret_hash, scopes, created_by, created_at, revoked_by, revoked_at)
			SELECT gen_random_uuid(), $1, 'Revogada ' || k.n, k.key_id, sha256(convert_to(k.key_id, 'UTF8')), '{customers.read}', $2,
			       $3::timestamptz + k.n * interval '1 second', $2, $3::timestamptz + k.n * interval '1 second'
			  FROM unnest($4::text[]) WITH ORDINALITY AS k(key_id, n)`, tn.ID, tn.Owner.ID, created.Add(2*time.Minute), keys)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(48 * time.Hour)

	var list []apicredentials.Credential
	pages, cursor := 0, ""
	for {
		pg, err := svc.List(ctx, tn.ID, 100, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		list = append(list, pg.Items...)
		if cursor = pg.NextCursor; cursor == "" {
			break
		}
	}
	if pages != 3 || len(list) != revoked+2 || list[0].ID != old.Credential.ID || list[0].State(env.Clock.Now()) != apicredentials.StateActive {
		t.Fatalf("a credencial ativa deveria abrir a listagem: %d páginas, %d itens, primeiro %s (esperado %s)", pages, len(list), list[0].ID,
			old.Credential.ID)
	}
	for i := 1; i < len(list); i++ {
		if list[i].State(env.Clock.Now()) == apicredentials.StateActive {
			t.Fatalf("credencial ativa fora do início da listagem: posição %d", i)
		}
		if i > 1 && list[i].CreatedAt.After(list[i-1].CreatedAt) {
			t.Fatalf("inativas fora da ordem da mais recente: posição %d", i)
		}
	}
	if list[len(list)-1].ID != expiring.Credential.ID {
		t.Fatal("a expirada mais antiga que as revogadas deveria fechar a listagem")
	}
	if _, err := svc.Authenticate(ctx, old.Token, "ip:teste"); err != nil {
		t.Fatalf("a credencial ativa continua autenticando: %v", err)
	}
}

// A situação que ordena as credenciais (ativas primeiro) é a do instante da
// primeira página, levado no cursor: a credencial mais recente, que expira
// durante a navegação, não reaparece no bloco das inativas das páginas
// seguintes; uma nova listagem já a mostra entre as inativas.
func TestAPICredentialListCursorKeepsOrderWhenCredentialExpires(t *testing.T) {
	ctx := context.Background()
	env, svc := credentialsEnv(t, 4)
	tn := apiTenant(t, env, "cred-cursor")
	create := func(name string, days *int) apicredentials.Credential {
		t.Helper()
		is, err := svc.Create(ctx, tn.ID, ownerGrantor(tn), apicredentials.CreateInput{Name: name, Scopes: []string{"customers.read"}, ExpiresInDays: days})
		if err != nil {
			t.Fatal(err)
		}
		env.Clock.Advance(time.Minute)
		return is.Credential
	}
	oldest, middle, revoked := create("Mais antiga", nil), create("Intermediária", nil), create("Revogada", nil)
	if err := svc.Revoke(ctx, tn.ID, ownerGrantor(tn), revoked.ID, revoked.Version); err != nil {
		t.Fatal(err)
	}
	expiring := create("Temporária", new(1))
	ids := func(items []apicredentials.Credential) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(items))
		for _, c := range items {
			out = append(out, c.ID)
		}
		return out
	}
	first, err := svc.List(ctx, tn.ID, 1, "")
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != expiring.ID || first.NextCursor == "" {
		t.Fatalf("primeira página: %+v %v", first, err)
	}
	env.Clock.Advance(48 * time.Hour)
	seen := ids(first.Items)
	for cursor := first.NextCursor; cursor != ""; {
		pg, err := svc.List(ctx, tn.ID, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		seen, cursor = append(seen, ids(pg.Items)...), pg.NextCursor
	}
	if want := []uuid.UUID{expiring.ID, middle.ID, oldest.ID, revoked.ID}; !slices.Equal(seen, want) {
		t.Fatalf("páginas depois da expiração: %v, esperado %v", seen, want)
	}
	fresh, err := svc.List(ctx, tn.ID, 10, "")
	if err != nil || !slices.Equal(ids(fresh.Items), []uuid.UUID{middle.ID, oldest.ID, expiring.ID, revoked.ID}) {
		t.Fatalf("nova listagem com a expirada entre as inativas: %v %v", ids(fresh.Items), err)
	}
	// Cursor fora da forma canônica ou de outra listagem: 422 invalid_cursor.
	for _, c := range []string{"abc", first.NextCursor + "x", "MjAyNi0wMS0wMVQwMDowMDowMFp8MDAwMDAwMDAtMDAwMC0wMDAwLTAwMDAtMDAwMDAwMDAwMDAw"} {
		if _, err := svc.List(ctx, tn.ID, 1, c); !apperr.IsKind(err, apperr.KindInvalid) {
			t.Errorf("cursor %q: %v", c, err)
		}
	}
}

// A migration 00012 é reversível: o Down remove tabelas e funções e o Up as
// recria.
func TestPortalAndCredentialsMigrationIsReversible(t *testing.T) {
	ctx := context.Background()
	env := pgtest.Start(t)
	sqlDB, err := sql.Open("pgx", env.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	fsys, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		t.Fatal(err)
	}
	// Migrations posteriores à 00012 (por exemplo, a 00013 de recebíveis)
	// também são revertidas pelo DownTo e reaplicadas pelo Up.
	current, err := p.GetDBVersion(ctx)
	if err != nil || current < 12 {
		t.Fatalf("versão aplicada: %d %v", current, err)
	}
	if _, err := p.DownTo(ctx, 9); err != nil {
		t.Fatalf("down: %v", err)
	}
	t.Cleanup(func() {
		if _, err := p.Up(context.Background()); err != nil {
			t.Errorf("up após o teste: %v", err)
		}
	})
	owner := pgtest.OwnerPool(t)
	for _, rel := range []string{"api_credentials", "customer_portal_links"} {
		var exists bool
		if err := owner.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, rel).Scan(&exists); err != nil || exists {
			t.Fatalf("%s permanece após o down: %v", rel, err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	var fn bool
	if err := owner.QueryRow(ctx, `SELECT to_regprocedure('app_find_api_credential(text)') IS NOT NULL`).Scan(&fn); err != nil || !fn {
		t.Fatalf("função recriada: %v", err)
	}
}
