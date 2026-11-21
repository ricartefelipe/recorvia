package apicredentials

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

func apiPlan() platformbilling.Plan {
	return platformbilling.Plan{Code: "profissional", Active: true,
		Features: []platformbilling.Feature{platformbilling.FeatureRecurringBilling, platformbilling.FeatureAPIAccess}}
}

type env struct {
	store    *fakeStore
	counter  *fakeCounter
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	owner    Grantor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{store: newFakeStore(), counter: newFakeCounter(), clock: clock.NewManual(testNow), tenantID: uuid.New()}
	e.svc = NewService(e.store, e.counter, e.clock, DefaultPolicy())
	e.store.tenants[e.tenantID] = &tenantData{status: tenancy.TenantActive, plan: apiPlan(), members: map[uuid.UUID]member{},
		credentials: map[uuid.UUID]Credential{}}
	e.owner = e.addMember(tenancy.RoleOwner)
	return e
}

func (e *env) d() *tenantData { return e.store.tenants[e.tenantID] }

func (e *env) addMember(role tenancy.Role) Grantor {
	id := uuid.New()
	e.d().members[id] = member{role: role, active: true}
	return Grantor{UserID: id, Role: role, Actor: audit.User(id, string(role))}
}

func (e *env) create(t *testing.T, g Grantor, scopes ...string) Issued {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{"customers.read", "customers.write"}
	}
	is, err := e.svc.Create(context.Background(), e.tenantID, g, CreateInput{Name: "ERP interno", Scopes: scopes})
	if err != nil {
		t.Fatal(err)
	}
	return is
}

func (e *env) lastAudit(t *testing.T) audit.Entry {
	t.Helper()
	a := e.d().audits
	if len(a) == 0 {
		t.Fatal("nenhuma auditoria registrada")
	}
	return a[len(a)-1]
}

func assertErr(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok || e.Kind != kind || (code != "" && e.Code != code) {
		t.Fatalf("erro = %v, esperado %v/%s", err, kind, code)
	}
}

func ptr[T any](v T) *T { return &v }

func TestCreateIssuesTokenStoringOnlyTheHash(t *testing.T) {
	e := newEnv(t)
	is, err := e.svc.Create(context.Background(), e.tenantID, e.owner, CreateInput{Name: "  ERP interno  ",
		Scopes: []string{"invoices.read", "customers.read", "invoices.read"}, ExpiresInDays: ptr(30)})
	if err != nil {
		t.Fatal(err)
	}
	keyID, secret, ok := ParseToken(is.Token)
	if !ok || keyID != is.Credential.KeyID {
		t.Fatalf("token devolvido fora do formato: %q", is.Token)
	}
	stored := e.d().credentials[is.Credential.ID]
	if !bytes.Equal(stored.SecretHash, HashSecret(secret)) || bytes.Contains(stored.SecretHash, []byte(secret)) {
		t.Fatal("somente o hash do segredo deve ser guardado")
	}
	if stored.Name != "ERP interno" || stored.CreatedBy != e.owner.UserID || stored.TenantID != e.tenantID {
		t.Fatalf("credencial gravada: %+v", stored)
	}
	if !slices.Equal(stored.Scopes, []tenancy.Permission{tenancy.PermCustomersRead, tenancy.PermInvoicesRead}) {
		t.Fatalf("escopos normalizados: %v", stored.Scopes)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(testNow.Add(30*24*time.Hour)) {
		t.Fatalf("expiração: %v", stored.ExpiresAt)
	}
	a := e.lastAudit(t)
	if a.Action != "api_credential.created" || a.Actor != e.owner.Actor || a.ResourceID != stored.ID.String() || a.Details["prefix"] != stored.Prefix() {
		t.Fatalf("auditoria: %+v", a)
	}
	for k, v := range a.Details {
		if s, ok := v.(string); ok && (s == is.Token || s == secret) {
			t.Fatalf("auditoria contém o segredo em %s", k)
		}
	}
	noExpiry := e.create(t, e.owner)
	if noExpiry.Credential.ExpiresAt != nil {
		t.Fatal("sem validade informada, a credencial não expira")
	}
}

func TestCreateValidation(t *testing.T) {
	cases := []struct {
		name  string
		in    CreateInput
		setup func(*env)
		kind  apperr.Kind
		code  string
	}{
		{"nome curto", CreateInput{Name: "ab", Scopes: []string{"customers.read"}}, nil, apperr.KindInvalid, "invalid_api_credential"},
		{"nome com controle", CreateInput{Name: "ERP\ninterno", Scopes: []string{"customers.read"}}, nil, apperr.KindInvalid, "invalid_api_credential"},
		{"validade zero", CreateInput{Name: "ERP", Scopes: []string{"customers.read"}, ExpiresInDays: ptr(0)}, nil, apperr.KindInvalid, "invalid_api_credential"},
		{"validade acima do máximo", CreateInput{Name: "ERP", Scopes: []string{"customers.read"}, ExpiresInDays: ptr(MaxExpiresInDays + 1)}, nil,
			apperr.KindInvalid, "invalid_api_credential"},
		{"escopo não delegável", CreateInput{Name: "ERP", Scopes: []string{"members.manage"}}, nil, apperr.KindInvalid, "invalid_api_credential"},
		{"plano sem acesso à API", CreateInput{Name: "ERP", Scopes: []string{"customers.read"}},
			func(e *env) { e.d().plan.Features = []platformbilling.Feature{platformbilling.FeatureRecurringBilling} },
			apperr.KindForbidden, "platform_feature_unavailable"},
		{"limite de credenciais ativas", CreateInput{Name: "ERP", Scopes: []string{"customers.read"}},
			func(e *env) {
				for range MaxActivePerTenant {
					id := uuid.New()
					e.d().credentials[id] = Credential{ID: id, TenantID: e.tenantID, CreatedAt: testNow}
				}
			}, apperr.KindConflict, "api_credential_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			before := len(e.d().credentials)
			_, err := e.svc.Create(context.Background(), e.tenantID, e.owner, tc.in)
			assertErr(t, err, tc.kind, tc.code)
			if len(e.d().credentials) != before || len(e.d().audits) != 0 {
				t.Fatal("falha não pode gravar credencial nem auditoria")
			}
		})
	}
}

func TestCreateScopeBeyondGrantorIsForbidden(t *testing.T) {
	e := newEnv(t)
	admin := e.addMember(tenancy.RoleAdmin)
	_, err := e.svc.Create(context.Background(), e.tenantID, admin, CreateInput{Name: "ERP", Scopes: []string{"customers.read", "platform_billing.read"}})
	if err != nil {
		t.Fatalf("admin pode delegar o que tem: %v", err)
	}
	finance := e.addMember(tenancy.RoleFinance)
	_, err = e.svc.Create(context.Background(), e.tenantID, finance, CreateInput{Name: "ERP", Scopes: []string{"customers.write"}})
	assertErr(t, err, apperr.KindForbidden, "scope_exceeds_permissions")
}

// A contagem de credenciais ativas só acontece depois do bloqueio que
// serializa as emissões da organização; sem ele, criações simultâneas leriam a
// mesma contagem e ultrapassariam o limite.
func TestCreateCountsActiveCredentialsUnderIssuanceLock(t *testing.T) {
	e := newEnv(t)
	e.create(t, e.owner)
	lock := slices.Index(e.store.order, "LockIssuance")
	count := slices.Index(e.store.order, "CountActiveCredentials")
	if lock < 0 || count < 0 || lock > count || slices.Index(e.store.order, "InsertCredential") < count {
		t.Fatalf("ordem das operações na criação: %v", e.store.order)
	}
}

func TestCreatePropagatesStoreFailures(t *testing.T) {
	for _, op := range []string{"InTenant", "PlatformPlan", "LockIssuance", "CountActiveCredentials", "InsertCredential", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.store.fail[op] = errBoom
			if _, err := e.svc.Create(context.Background(), e.tenantID, e.owner, CreateInput{Name: "ERP", Scopes: []string{"customers.read"}}); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v", err)
			}
			if len(e.d().credentials) != 0 {
				t.Fatal("transação com falha não pode gravar")
			}
		})
	}
}

func TestListReturnsCredentials(t *testing.T) {
	e := newEnv(t)
	e.create(t, e.owner)
	e.clock.Advance(time.Minute)
	second := e.create(t, e.owner)
	got, err := e.svc.List(context.Background(), e.tenantID, 10, "cursor-da-pagina")
	if err != nil || len(got.Items) != 2 || got.Items[0].ID != second.Credential.ID {
		t.Fatalf("lista: %v %v", got, err)
	}
	// O instante da listagem vem do relógio do serviço: é ele que decide
	// quais credenciais estão ativas e vêm primeiro. Limite e cursor chegam
	// ao armazenamento como informados.
	if !e.store.listedAt.Equal(e.clock.Now()) || e.store.listedLimit != 10 || e.store.listedCursor != "cursor-da-pagina" {
		t.Fatalf("listagem: instante %v (esperado %v), limite %d, cursor %q", e.store.listedAt, e.clock.Now(), e.store.listedLimit,
			e.store.listedCursor)
	}
	e.store.fail["Credentials"] = errBoom
	if _, err := e.svc.List(context.Background(), e.tenantID, 0, ""); !errors.Is(err, errBoom) {
		t.Fatalf("falha da listagem: %v", err)
	}
}

func (e *env) authenticate(token string) (Principal, error) {
	return e.svc.Authenticate(context.Background(), token, "ip:203.0.113.7")
}

func TestAuthenticateValidCredential(t *testing.T) {
	e := newEnv(t)
	is := e.create(t, e.owner)
	p, err := e.authenticate(is.Token)
	if err != nil {
		t.Fatal(err)
	}
	if p.CredentialID != is.Credential.ID || p.TenantID != e.tenantID || p.Name != "ERP interno" || p.KeyID != is.Credential.KeyID {
		t.Fatalf("principal: %+v", p)
	}
	if !p.Can(tenancy.PermCustomersWrite) || p.Can(tenancy.PermInvoicesRead) {
		t.Fatalf("escopos: %v", p.Scopes)
	}
	if lu := e.d().credentials[is.Credential.ID].LastUsedAt; lu == nil || !lu.Equal(testNow) {
		t.Fatalf("último uso não registrado: %v", lu)
	}
	// Dentro da resolução, o último uso não é regravado.
	e.clock.Advance(30 * time.Second)
	if _, err := e.authenticate(is.Token); err != nil {
		t.Fatal(err)
	}
	if e.d().touches != 1 || e.store.calls["TouchCredential"] != 1 {
		t.Fatalf("último uso regravado dentro da resolução: %d", e.d().touches)
	}
	e.clock.Advance(30 * time.Second)
	if _, err := e.authenticate(is.Token); err != nil {
		t.Fatal(err)
	}
	if e.d().touches != 2 {
		t.Fatalf("último uso deveria ser atualizado após %s", LastUsedResolution)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(e *env, c *Credential) string // devolve o token a apresentar ("" usa o emitido)
		kind   apperr.Kind
		code   string
	}{
		{"token malformado", func(*env, *Credential) string { return "rcv_curto" }, apperr.KindUnauthenticated, "api_credential_invalid"},
		{"identificador desconhecido", func(*env, *Credential) string { return FormatToken(newKeyID(), newSecret()) },
			apperr.KindUnauthenticated, "api_credential_invalid"},
		{"segredo errado", func(_ *env, c *Credential) string { return FormatToken(c.KeyID, newSecret()) },
			apperr.KindUnauthenticated, "api_credential_invalid"},
		{"revogada", func(_ *env, c *Credential) string { c.RevokedAt = &testNow; return "" }, apperr.KindUnauthenticated, "api_credential_invalid"},
		{"expirada", func(_ *env, c *Credential) string { c.ExpiresAt = &testNow; return "" }, apperr.KindUnauthenticated, "api_credential_invalid"},
		{"organização suspensa", func(e *env, _ *Credential) string { e.d().status = tenancy.TenantSuspended; return "" },
			apperr.KindForbidden, "organization_inactive"},
		{"plano sem acesso à API", func(e *env, _ *Credential) string { e.d().plan.Features = nil; return "" },
			apperr.KindForbidden, "platform_feature_unavailable"},
		{"emissor removido", func(e *env, c *Credential) string {
			e.d().members[c.CreatedBy] = member{role: tenancy.RoleOwner}
			return ""
		},
			apperr.KindForbidden, "api_credential_issuer_inactive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			is := e.create(t, e.owner)
			c := e.d().credentials[is.Credential.ID]
			token := tc.mutate(e, &c)
			e.d().credentials[c.ID] = c
			if token == "" {
				token = is.Token
			}
			_, err := e.authenticate(token)
			assertErr(t, err, tc.kind, tc.code)
			if e.d().credentials[c.ID].LastUsedAt != nil {
				t.Fatal("tentativa recusada não registra uso")
			}
			failures := e.counter.counts["api_credential:ip:203.0.113.7"]
			if (tc.kind == apperr.KindUnauthenticated) != (failures == 1) {
				t.Fatalf("falhas registradas: %d", failures)
			}
		})
	}
}

// Credencial de outro tenant: o tenant vem da própria credencial; o mesmo
// identificador nunca é procurado no contexto de outra organização.
func TestAuthenticateUsesTenantOfCredential(t *testing.T) {
	e := newEnv(t)
	other := uuid.New()
	e.store.tenants[other] = &tenantData{status: tenancy.TenantActive, plan: apiPlan(), members: map[uuid.UUID]member{},
		credentials: map[uuid.UUID]Credential{}}
	is := e.create(t, e.owner)
	p, err := e.authenticate(is.Token)
	if err != nil || p.TenantID != e.tenantID {
		t.Fatalf("tenant do principal: %v %v", p.TenantID, err)
	}
	// Uma credencial movida para outro tenant (dados inconsistentes) não é
	// encontrada no contexto do tenant indicado pela busca.
	c := e.d().credentials[is.Credential.ID]
	delete(e.d().credentials, c.ID)
	e.store.tenants[other].credentials[c.ID] = c
	e.store.tenants[other].members[c.CreatedBy] = member{role: tenancy.RoleOwner, active: true}
	p, err = e.authenticate(is.Token)
	if err != nil || p.TenantID != other {
		t.Fatalf("o principal deve carregar o tenant em que a credencial foi localizada: %v %v", p.TenantID, err)
	}
}

func TestAuthenticateEffectiveScopesFollowIssuerRole(t *testing.T) {
	e := newEnv(t)
	admin := e.addMember(tenancy.RoleAdmin)
	is := e.create(t, admin, "customers.write", "invoices.void", "customers.read")
	e.d().members[admin.UserID] = member{role: tenancy.RoleOperator, active: true}
	p, err := e.authenticate(is.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Scopes, []tenancy.Permission{tenancy.PermCustomersRead, tenancy.PermCustomersWrite}) {
		t.Fatalf("escopos efetivos após rebaixamento: %v", p.Scopes)
	}
}

func TestAuthenticateFailureLimit(t *testing.T) {
	e := newEnv(t)
	is := e.create(t, e.owner)
	bad := FormatToken(is.Credential.KeyID, newSecret())
	for i := range DefaultPolicy().MaxFailures {
		_, err := e.authenticate(bad)
		assertErr(t, err, apperr.KindUnauthenticated, "api_credential_invalid")
		if i == 0 && e.counter.windows["api_credential:ip:203.0.113.7"] != DefaultPolicy().FailureWindow {
			t.Fatal("janela de falhas não aplicada")
		}
	}
	_, err := e.authenticate(is.Token)
	assertErr(t, err, apperr.KindRateLimited, "authentication_attempts_exceeded")
	// Outra origem não é afetada.
	if _, err := e.svc.Authenticate(context.Background(), is.Token, "ip:198.51.100.1"); err != nil {
		t.Fatalf("outra origem bloqueada: %v", err)
	}
	// Falha do contador libera a verificação (proteção auxiliar).
	e.counter.readErr, e.counter.recErr = errBoom, errBoom
	if _, err := e.authenticate(is.Token); err != nil {
		t.Fatalf("falha do contador não deve bloquear: %v", err)
	}
	if _, err := e.authenticate(bad); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("segredo errado continua recusado: %v", err)
	}
	// Sem contador ou com limite desligado, não há bloqueio.
	for _, svc := range []*Service{NewService(e.store, nil, e.clock, DefaultPolicy()), NewService(e.store, e.counter, e.clock, Policy{})} {
		if _, err := svc.Authenticate(context.Background(), bad, "ip:203.0.113.7"); !apperr.IsKind(err, apperr.KindUnauthenticated) {
			t.Fatalf("sem limitação: %v", err)
		}
	}
}

func TestAuthenticateStoreFailures(t *testing.T) {
	for _, op := range []string{"FindCredential", "InTenant", "AuthContext", "TouchCredential"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			is := e.create(t, e.owner)
			e.store.fail[op] = errBoom
			if _, err := e.authenticate(is.Token); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v", err)
			}
			if len(e.counter.counts) != 0 {
				t.Fatal("falha de infraestrutura não conta como tentativa inválida")
			}
		})
	}
	// Localizada pela busca global, mas ausente no contexto do tenant: inválida.
	e := newEnv(t)
	is := e.create(t, e.owner)
	e.store.fail["AuthContext"] = apperr.NotFound("api_credential_not_found", "Credencial não encontrada.")
	_, err := e.authenticate(is.Token)
	assertErr(t, err, apperr.KindUnauthenticated, "api_credential_invalid")
}

func TestRotateKeepsPreviousSecretDuringGrace(t *testing.T) {
	e := newEnv(t)
	is := e.create(t, e.owner)
	e.clock.Advance(time.Hour)
	admin := e.addMember(tenancy.RoleAdmin)
	rot, err := e.svc.Rotate(context.Background(), e.tenantID, admin, is.Credential.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rot.Credential.ID != is.Credential.ID || rot.Credential.KeyID != is.Credential.KeyID || rot.Token == is.Token {
		t.Fatal("a rotação mantém o identificador e troca o segredo")
	}
	stored := e.d().credentials[is.Credential.ID]
	graceEnd := e.clock.Now().Add(RotationGrace)
	if stored.PreviousSecretExpiresAt == nil || !stored.PreviousSecretExpiresAt.Equal(graceEnd) || stored.Version != 2 ||
		stored.RotatedBy == nil || *stored.RotatedBy != admin.UserID {
		t.Fatalf("rotação gravada: %+v", stored)
	}
	a := e.lastAudit(t)
	if a.Action != "api_credential.rotated" || a.Actor != admin.Actor {
		t.Fatalf("auditoria: %+v", a)
	}
	for _, token := range []string{is.Token, rot.Token} {
		if _, err := e.authenticate(token); err != nil {
			t.Fatalf("token deveria valer na janela: %v", err)
		}
	}
	e.clock.Set(graceEnd)
	if _, err := e.authenticate(is.Token); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("segredo anterior após a janela: %v", err)
	}
	if _, err := e.authenticate(rot.Token); err != nil {
		t.Fatalf("segredo novo: %v", err)
	}
	// Segunda rotação: o segredo de antes da primeira é descartado de imediato.
	e.clock.Set(testNow.Add(2 * time.Hour))
	first := e.create(t, e.owner)
	r1, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, first.Credential.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, first.Credential.ID, r1.Credential.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.authenticate(first.Token); !apperr.IsKind(err, apperr.KindUnauthenticated) {
		t.Fatalf("segredo original após duas rotações: %v", err)
	}
	for _, token := range []string{r1.Token, r2.Token} {
		if _, err := e.authenticate(token); err != nil {
			t.Fatalf("segredos das rotações: %v", err)
		}
	}
}

func TestRotateMovesResponsibilityToRotator(t *testing.T) {
	e := newEnv(t)
	admin := e.addMember(tenancy.RoleAdmin)
	is := e.create(t, admin)
	if _, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, is.Credential.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Quem criou sai da organização; a credencial continua com quem rotacionou.
	e.d().members[admin.UserID] = member{role: tenancy.RoleAdmin}
	if _, err := e.authenticate(is.Token); err != nil {
		t.Fatalf("o segredo anterior segue o responsável atual: %v", err)
	}
}

func TestRotateGraceNeverExceedsExpiry(t *testing.T) {
	e := newEnv(t)
	is, err := e.svc.Create(context.Background(), e.tenantID, e.owner, CreateInput{Name: "ERP", Scopes: []string{"customers.read"}, ExpiresInDays: ptr(1)})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(20 * time.Hour)
	if _, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, is.Credential.ID, 1); err != nil {
		t.Fatal(err)
	}
	stored := e.d().credentials[is.Credential.ID]
	if !stored.PreviousSecretExpiresAt.Equal(*stored.ExpiresAt) {
		t.Fatalf("janela %v deveria terminar na validade %v", stored.PreviousSecretExpiresAt, stored.ExpiresAt)
	}
}

func TestRotateRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(e *env, c *Credential)
		grantor func(e *env) Grantor
		id      func(c Credential) uuid.UUID
		kind    apperr.Kind
		code    string
	}{
		{"revogada", func(_ *env, c *Credential) { c.RevokedAt = &testNow }, nil, nil, apperr.KindConflict, "api_credential_inactive"},
		{"expirada", func(_ *env, c *Credential) { c.ExpiresAt = &testNow }, nil, nil, apperr.KindConflict, "api_credential_inactive"},
		{"inexistente", nil, nil, func(Credential) uuid.UUID { return uuid.New() }, apperr.KindNotFound, "api_credential_not_found"},
		{"sem as permissões dos escopos", nil, func(e *env) Grantor { return e.addMember(tenancy.RoleFinance) }, nil,
			apperr.KindForbidden, "scope_exceeds_permissions"},
		{"plano sem acesso à API", func(e *env, _ *Credential) { e.d().plan.Features = nil }, nil, nil,
			apperr.KindForbidden, "platform_feature_unavailable"},
		// A versão do If-Match vai na condição da gravação.
		{"versão divergente", func(_ *env, c *Credential) { c.Version = 4 }, nil, nil, apperr.KindPreconditionFailed, "version_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			is := e.create(t, e.owner)
			c := e.d().credentials[is.Credential.ID]
			if tc.mutate != nil {
				tc.mutate(e, &c)
				e.d().credentials[c.ID] = c
			}
			g := e.owner
			if tc.grantor != nil {
				g = tc.grantor(e)
			}
			id := c.ID
			if tc.id != nil {
				id = tc.id(c)
			}
			_, err := e.svc.Rotate(context.Background(), e.tenantID, g, id, 1)
			assertErr(t, err, tc.kind, tc.code)
			if !bytes.Equal(e.d().credentials[c.ID].SecretHash, is.Credential.SecretHash) {
				t.Fatal("rotação recusada não pode trocar o segredo")
			}
		})
	}
	for _, op := range []string{"RotateCredential", "Audit"} {
		e := newEnv(t)
		is := e.create(t, e.owner)
		e.store.fail[op] = errBoom
		if _, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, is.Credential.ID, 1); !errors.Is(err, errBoom) {
			t.Fatalf("%s: %v", op, err)
		}
		if _, err := e.authenticate(is.Token); err != nil {
			t.Fatalf("%s: rotação desfeita deve manter o segredo: %v", op, err)
		}
	}
}

func TestRevokeIsImmediateAndIdempotent(t *testing.T) {
	e := newEnv(t)
	is := e.create(t, e.owner)
	rot, err := e.svc.Rotate(context.Background(), e.tenantID, e.owner, is.Credential.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	admin := e.addMember(tenancy.RoleAdmin)
	// Versão divergente (anterior à rotação): nada é revogado.
	assertErr(t, e.svc.Revoke(context.Background(), e.tenantID, admin, is.Credential.ID, 1), apperr.KindPreconditionFailed, "version_mismatch")
	if e.d().credentials[is.Credential.ID].RevokedAt != nil {
		t.Fatal("revogação com versão divergente não pode gravar")
	}
	if err := e.svc.Revoke(context.Background(), e.tenantID, admin, is.Credential.ID, rot.Credential.Version); err != nil {
		t.Fatal(err)
	}
	stored := e.d().credentials[is.Credential.ID]
	if stored.RevokedAt == nil || *stored.RevokedBy != admin.UserID {
		t.Fatalf("revogação gravada: %+v", stored)
	}
	for _, token := range []string{is.Token, rot.Token} {
		if _, err := e.authenticate(token); !apperr.IsKind(err, apperr.KindUnauthenticated) {
			t.Fatalf("credencial revogada aceita: %v", err)
		}
	}
	audits := len(e.d().audits)
	if a := e.lastAudit(t); a.Action != "api_credential.revoked" || a.Actor != admin.Actor {
		t.Fatalf("auditoria: %+v", a)
	}
	revoked := e.d().credentials[is.Credential.ID].Version
	if err := e.svc.Revoke(context.Background(), e.tenantID, e.owner, is.Credential.ID, revoked); err != nil {
		t.Fatalf("revogar de novo: %v", err)
	}
	if len(e.d().audits) != audits || e.store.calls["RevokeCredential"] != 2 {
		t.Fatal("segunda revogação não deve gravar nada")
	}
	// Já revogada, a versão do If-Match ainda é conferida.
	assertErr(t, e.svc.Revoke(context.Background(), e.tenantID, e.owner, is.Credential.ID, rot.Credential.Version), apperr.KindPreconditionFailed,
		"version_mismatch")
	assertErr(t, e.svc.Revoke(context.Background(), e.tenantID, e.owner, uuid.New(), 1), apperr.KindNotFound, "api_credential_not_found")
	e2 := newEnv(t)
	is2 := e2.create(t, e2.owner)
	e2.store.fail["RevokeCredential"] = errBoom
	if err := e2.svc.Revoke(context.Background(), e2.tenantID, e2.owner, is2.Credential.ID, 1); !errors.Is(err, errBoom) {
		t.Fatalf("falha da revogação: %v", err)
	}
	// Revogação não depende do recurso do plano.
	e2.store.fail["RevokeCredential"] = nil
	e2.d().plan.Features = nil
	if err := e2.svc.Revoke(context.Background(), e2.tenantID, e2.owner, is2.Credential.ID, 1); err != nil {
		t.Fatalf("revogação após troca de plano: %v", err)
	}
}
