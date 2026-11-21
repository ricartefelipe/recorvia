package tenancy

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var testNow = time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)

func essencialPlan() platformbilling.Plan {
	return platformbilling.Plan{Code: "essencial", MonthlyPrice: money.New(19900, money.BRL), MaxUsers: ptr(3),
		MaxActiveSubscriptions: ptr(100), TrialDays: 14, IsDefault: true, Active: true}
}

// env é uma organização ativa com um proprietário.
type env struct {
	store    *fakeStore
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	owner    UserRef
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := newFakeStore()
	store.defaultPlan = essencialPlan()
	c := clock.NewManual(testNow)
	e := &env{store: store, clock: c, svc: NewService(store, c), tenantID: uuid.New()}
	tn, err := NewTenant(e.tenantID, TenantInput{Name: "Acme", Slug: "acme"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	d := store.data(e.tenantID)
	d.tenant = &tn
	d.plan = essencialPlan()
	e.owner = e.addMember(RoleOwner, MembershipActive, "dona@acme.com")
	return e
}

func (e *env) d() *tenantData { return e.store.tenants[e.tenantID] }

func (e *env) addMember(role Role, status MembershipStatus, email string) UserRef {
	u := UserRef{ID: uuid.New(), Email: email, DisplayName: strings.Split(email, "@")[0]}
	d := e.store.data(e.tenantID)
	d.members[u.ID] = Membership{TenantID: e.tenantID, UserID: u.ID, Role: role, Status: status, CreatedAt: testNow, Version: 1}
	d.emails[u.ID] = email
	return u
}

func (e *env) access(u UserRef) Access {
	return Access{TenantID: e.tenantID, UserID: u.ID, Role: e.d().members[u.ID].Role}
}

func (e *env) invite(t *testing.T, email string, role Role) (Invitation, string) {
	t.Helper()
	inv, token, err := e.svc.Invite(context.Background(), e.tenantID, e.access(e.owner), e.owner, email, role)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	return inv, token
}

func (e *env) lastAudit(t *testing.T) audit.Entry {
	t.Helper()
	a := e.d().audits
	if len(a) == 0 {
		t.Fatal("nenhuma entrada de auditoria")
	}
	return a[len(a)-1]
}

// --- Onboard ---

func TestOnboard(t *testing.T) {
	store := newFakeStore()
	store.defaultPlan = essencialPlan()
	svc := NewService(store, clock.NewManual(testNow))
	user := UserRef{ID: uuid.New(), Email: "ana@acme.com", DisplayName: "Ana"}

	tn, err := svc.Onboard(context.Background(), user, TenantInput{Name: "Acme", Slug: " ACME "})
	if err != nil {
		t.Fatal(err)
	}
	if store.checkedSlug != "acme" {
		t.Errorf("disponibilidade deve ser verificada com o slug normalizado, verificado %q", store.checkedSlug)
	}
	if tn.ID == uuid.Nil || tn.Slug != "acme" || tn.Status != TenantActive || tn.Version != 1 {
		t.Fatalf("organização inesperada: %+v", tn)
	}
	d := store.tenants[tn.ID]
	if d == nil || d.tenant == nil || d.tenant.ID != tn.ID {
		t.Fatal("organização não persistida")
	}
	m, ok := d.members[user.ID]
	if !ok || m.Role != RoleOwner || m.Status != MembershipActive || m.Version != 1 || m.TenantID != tn.ID {
		t.Fatalf("criador deve ser proprietário ativo: %+v", m)
	}
	sub := d.platformSub
	if sub == nil || sub.PlanCode != "essencial" || sub.Status != platformbilling.StatusTrialing || sub.TenantID != tn.ID {
		t.Fatalf("assinatura da plataforma deve iniciar em avaliação: %+v", sub)
	}
	// 14 dias de avaliação a partir de 16/11 (data local) terminam no início de 30/11 em São Paulo.
	if want := time.Date(2026, time.November, 30, 3, 0, 0, 0, time.UTC); sub.TrialEndsAt == nil || !sub.TrialEndsAt.Equal(want) {
		t.Errorf("fim da avaliação = %v, esperado %v", sub.TrialEndsAt, want)
	}
	if !d.initialized {
		t.Error("recursos iniciais do tenant não foram criados")
	}
	if len(d.audits) != 1 {
		t.Fatalf("esperada uma entrada de auditoria, obtidas %d", len(d.audits))
	}
	a := d.audits[0]
	if a.Action != "organization.created" || a.TenantID != tn.ID || a.Actor != audit.User(user.ID, "Ana") ||
		a.Details["slug"] != "acme" || a.Details["platform_plan"] != "essencial" {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
}

func TestOnboardWithoutTrial(t *testing.T) {
	store := newFakeStore()
	plan := essencialPlan()
	plan.TrialDays = 0
	store.defaultPlan = plan
	svc := NewService(store, clock.NewManual(testNow))
	tn, err := svc.Onboard(context.Background(), UserRef{ID: uuid.New()}, TenantInput{Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if sub := store.tenants[tn.ID].platformSub; sub.Status != platformbilling.StatusActive || sub.TrialEndsAt != nil {
		t.Fatalf("plano sem avaliação deve iniciar ativo: %+v", sub)
	}
}

func TestOnboardRejected(t *testing.T) {
	t.Run("dados inválidos não consultam o armazenamento", func(t *testing.T) {
		store := newFakeStore()
		_, err := NewService(store, clock.NewManual(testNow)).Onboard(context.Background(), UserRef{ID: uuid.New()}, TenantInput{Name: "A"})
		assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
		if store.calls["SlugAvailable"] != 0 || store.calls["InTenant"] != 0 {
			t.Fatalf("armazenamento consultado: %v", store.calls)
		}
	})
	t.Run("slug ocupado", func(t *testing.T) {
		store := newFakeStore()
		store.takenSlugs["acme"] = true
		_, err := NewService(store, clock.NewManual(testNow)).Onboard(context.Background(), UserRef{ID: uuid.New()}, TenantInput{Name: "Acme", Slug: "Acme"})
		assertAppErr(t, err, apperr.KindConflict, "slug_taken")
		if store.calls["InTenant"] != 0 {
			t.Fatal("nenhuma transação deveria ser aberta")
		}
	})
	t.Run("falha ao verificar o slug", func(t *testing.T) {
		store := newFakeStore()
		store.fail["SlugAvailable"] = errBoom
		_, err := NewService(store, clock.NewManual(testNow)).Onboard(context.Background(), UserRef{ID: uuid.New()}, TenantInput{Name: "Acme", Slug: "acme"})
		if !errors.Is(err, errBoom) {
			t.Fatalf("esperado erro propagado, obtido %v", err)
		}
	})
}

func TestOnboardRollsBackOnFailure(t *testing.T) {
	for _, op := range []string{"InsertTenant", "InsertMembership", "DefaultPlatformPlan", "InsertPlatformSubscription", "InitTenantResources", "Audit"} {
		t.Run(op, func(t *testing.T) {
			store := newFakeStore()
			store.defaultPlan = essencialPlan()
			store.fail[op] = errBoom
			tn, err := NewService(store, clock.NewManual(testNow)).Onboard(context.Background(), UserRef{ID: uuid.New()}, TenantInput{Name: "Acme", Slug: "acme"})
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if tn != (Tenant{}) {
				t.Errorf("nenhuma organização deveria ser devolvida: %+v", tn)
			}
			for _, d := range store.tenants {
				if d.tenant != nil || len(d.members) != 0 || d.platformSub != nil {
					t.Fatal("nada deveria ser persistido após falha")
				}
			}
		})
	}
	t.Run("fuso inválido é rejeitado antes da transação", func(t *testing.T) {
		store := newFakeStore()
		_, err := NewService(store, clock.NewManual(testNow)).Onboard(context.Background(), UserRef{ID: uuid.New()},
			TenantInput{Name: "Acme", Slug: "acme", Timezone: "Etc/Nada"})
		assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
	})
}

// --- Organização ---

func TestOrganization(t *testing.T) {
	e := newEnv(t)
	tn, err := e.svc.Organization(context.Background(), e.tenantID)
	if err != nil || tn.Slug != "acme" {
		t.Fatalf("Organization = %+v, %v", tn, err)
	}
	e.store.fail["Tenant"] = errBoom
	if _, err := e.svc.Organization(context.Background(), e.tenantID); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
}

func TestUpdateOrganization(t *testing.T) {
	e := newEnv(t)
	actor := audit.User(e.owner.ID, "dona")
	tn, err := e.svc.UpdateOrganization(context.Background(), e.tenantID, actor,
		TenantInput{Name: "Acme Brasil", Slug: "ignorado", InvoicePrefix: "acm", PaymentTermsDays: ptr(15)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if tn.Name != "Acme Brasil" || tn.Slug != "acme" || tn.InvoicePrefix != "ACM" || tn.PaymentTermsDays != 15 || tn.Version != 2 {
		t.Fatalf("organização inesperada: %+v", tn)
	}
	if stored := e.d().tenant; stored.Version != 2 || stored.Name != "Acme Brasil" {
		t.Fatalf("alteração não persistida: %+v", stored)
	}
	if a := e.lastAudit(t); a.Action != "organization.updated" || a.Actor != actor || !a.OccurredAt.Equal(testNow) {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	// Segunda alteração com a versão antiga é rejeitada.
	_, err = e.svc.UpdateOrganization(context.Background(), e.tenantID, actor, TenantInput{Name: "Outro nome"}, 1)
	assertAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if e.d().tenant.Name != "Acme Brasil" {
		t.Fatal("versão divergente não pode alterar a organização")
	}
}

func orgAddress() address.Address {
	return address.Address{Street: " Av. Paulista ", Number: "1000", Complement: "10º andar", District: "Bela Vista", City: "São Paulo",
		State: "sp", PostalCode: "01310-100"}
}

// O endereço da organização é gravado com o perfil, normalizado como o de
// cobrança do cliente, e a auditoria informa só os nomes dos campos
// alterados e se o endereço foi cadastrado agora. Sem endereço no pedido, o
// atual é mantido; sem mudança, nada é gravado nem auditado como alteração
// do endereço.
func TestUpdateOrganizationAddress(t *testing.T) {
	e := newEnv(t)
	actor := audit.User(e.owner.ID, "dona")
	ctx := context.Background()
	a := orgAddress()
	tn, err := e.svc.UpdateOrganization(ctx, e.tenantID, actor, TenantInput{Name: "Acme", Address: &a}, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := address.Address{Street: "Av. Paulista", Number: "1000", Complement: "10º andar", District: "Bela Vista", City: "São Paulo",
		State: "SP", PostalCode: "01310100", Country: "BR"}
	if tn.Address == nil || *tn.Address != want || tn.Version != 2 {
		t.Fatalf("organização devolvida: %+v %+v", tn, tn.Address)
	}
	if stored := e.d().tenant.Address; stored == nil || *stored != want || e.store.calls["UpsertTenantAddress"] != 1 {
		t.Fatalf("endereço gravado: %+v (gravações: %d)", stored, e.store.calls["UpsertTenantAddress"])
	}
	created := e.lastAudit(t)
	if created.Action != "organization.address_updated" || created.ResourceType != "organization" || created.Actor != actor ||
		created.Details["created"] != true || len(created.Details["fields"].([]string)) != 8 {
		t.Fatalf("auditoria do cadastro do endereço: %+v", created)
	}

	// Sem endereço no pedido, o atual fica; nenhuma auditoria de endereço.
	audits := len(e.d().audits)
	if tn, err = e.svc.UpdateOrganization(ctx, e.tenantID, actor, TenantInput{Name: "Acme Brasil"}, 2); err != nil || tn.Address == nil || *tn.Address != want {
		t.Fatalf("sem endereço no pedido: %+v %v", tn.Address, err)
	}
	if got := e.d().audits[audits:]; len(got) != 1 || got[0].Action != "organization.updated" || e.store.calls["UpsertTenantAddress"] != 1 {
		t.Fatalf("auditorias sem mudança de endereço: %+v", got)
	}

	// O mesmo endereço, só com outra grafia, também não é alteração.
	same := want
	same.State, same.PostalCode = "sp", "01310-100"
	audits = len(e.d().audits)
	if _, err = e.svc.UpdateOrganization(ctx, e.tenantID, actor, TenantInput{Name: "Acme Brasil", Address: &same}, 3); err != nil {
		t.Fatal(err)
	}
	if got := e.d().audits[audits:]; len(got) != 1 || e.store.calls["UpsertTenantAddress"] != 1 {
		t.Fatalf("endereço igual não deveria ser regravado: %+v", got)
	}

	// Mudança: somente os nomes dos campos, nunca os valores.
	moved := want
	moved.Number, moved.PostalCode = "2000", "01311000"
	if tn, err = e.svc.UpdateOrganization(ctx, e.tenantID, actor, TenantInput{Name: "Acme Brasil", Address: &moved}, 4); err != nil {
		t.Fatal(err)
	}
	updated := e.lastAudit(t)
	if updated.Action != "organization.address_updated" || updated.Details["created"] != false ||
		fmt.Sprint(updated.Details["fields"]) != "[number postal_code]" || strings.Contains(fmt.Sprint(updated.Details), "01311000") {
		t.Fatalf("auditoria da alteração do endereço: %+v", updated)
	}
	if *e.d().tenant.Address != moved || tn.Version != 5 {
		t.Fatalf("endereço alterado: %+v (versão %d)", e.d().tenant.Address, tn.Version)
	}
}

func TestUpdateOrganizationAddressRejected(t *testing.T) {
	t.Run("endereço inválido", func(t *testing.T) {
		e := newEnv(t)
		bad := orgAddress()
		bad.State, bad.PostalCode = "São Paulo", "123"
		_, err := e.svc.UpdateOrganization(context.Background(), e.tenantID, audit.System("t"), TenantInput{Name: "Acme", Address: &bad}, 1)
		fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
		if fmt.Sprint(fields) != "[address.state address.postal_code]" {
			t.Fatalf("campos: %v", fields)
		}
		if e.d().tenant.Address != nil || len(e.d().audits) != 0 || e.d().tenant.Version != 1 {
			t.Fatal("nada deveria ser gravado")
		}
	})
	t.Run("falha ao gravar o endereço desfaz o perfil", func(t *testing.T) {
		e := newEnv(t)
		e.store.fail["UpsertTenantAddress"] = errBoom
		a := orgAddress()
		_, err := e.svc.UpdateOrganization(context.Background(), e.tenantID, audit.System("t"), TenantInput{Name: "Acme 2", Address: &a}, 1)
		if !errors.Is(err, errBoom) {
			t.Fatalf("esperado erro propagado, obtido %v", err)
		}
		if d := e.d(); d.tenant.Name != "Acme" || d.tenant.Address != nil || len(d.audits) != 0 {
			t.Fatalf("a alteração deveria ser desfeita: %+v", d.tenant)
		}
	})
}

func TestUpdateOrganizationRejected(t *testing.T) {
	t.Run("dados inválidos", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.svc.UpdateOrganization(context.Background(), e.tenantID, audit.System("t"), TenantInput{Name: "A"}, 1)
		assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
		if e.d().tenant.Name != "Acme" || len(e.d().audits) != 0 {
			t.Fatal("nada deveria ser alterado")
		}
	})
	for _, op := range []string{"Tenant", "UpdateTenant", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newEnv(t)
			e.store.fail[op] = errBoom
			_, err := e.svc.UpdateOrganization(context.Background(), e.tenantID, audit.System("t"), TenantInput{Name: "Acme 2"}, 1)
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.d().tenant.Name != "Acme" {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

// --- ResolveAccess ---

func TestResolveAccess(t *testing.T) {
	e := newEnv(t)
	finance := e.addMember(RoleFinance, MembershipActive, "fin@acme.com")
	a, err := e.svc.ResolveAccess(context.Background(), e.tenantID, finance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a != (Access{TenantID: e.tenantID, UserID: finance.ID, Role: RoleFinance}) {
		t.Fatalf("acesso inesperado: %+v", a)
	}
}

// Falha de infraestrutura não é confundida com ausência de acesso: o erro
// original é propagado (indisponibilidade, não 403).
func TestResolveAccessPropagatesInfrastructureErrors(t *testing.T) {
	for _, op := range []string{"Tenant", "Membership"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.store.fail[op] = errBoom
			_, err := e.svc.ResolveAccess(context.Background(), e.tenantID, e.owner.ID)
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro de infraestrutura, obtido %v", err)
			}
		})
	}
}

func TestResolveAccessDenied(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*env) uuid.UUID
		code  string
	}{
		{"organização inexistente", func(e *env) uuid.UUID { e.d().tenant = nil; return e.owner.ID }, "no_membership"},
		{"organização suspensa", func(e *env) uuid.UUID { e.d().tenant.Status = TenantSuspended; return e.owner.ID }, "organization_inactive"},
		{"organização encerrada", func(e *env) uuid.UUID { e.d().tenant.Status = TenantClosed; return e.owner.ID }, "organization_inactive"},
		{"usuário sem associação", func(*env) uuid.UUID { return uuid.New() }, "no_membership"},
		{"membro removido", func(e *env) uuid.UUID {
			return e.addMember(RoleAdmin, MembershipRemoved, "ex@acme.com").ID
		}, "no_membership"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			uid := tc.setup(e)
			a, err := e.svc.ResolveAccess(context.Background(), e.tenantID, uid)
			assertAppErr(t, err, apperr.KindForbidden, tc.code)
			if a != (Access{}) {
				t.Fatalf("nenhum acesso deveria ser devolvido: %+v", a)
			}
		})
	}
}

func TestListings(t *testing.T) {
	e := newEnv(t)
	e.addMember(RoleAuditor, MembershipActive, "aud@acme.com")
	e.invite(t, "nova@acme.com", RoleOperator)

	members, err := e.svc.Members(context.Background(), e.tenantID)
	if err != nil || len(members) != 2 {
		t.Fatalf("Members = %v, %v", members, err)
	}
	invs, err := e.svc.Invitations(context.Background(), e.tenantID)
	if err != nil || len(invs) != 1 || invs[0].Email != "nova@acme.com" {
		t.Fatalf("Invitations = %v, %v", invs, err)
	}
	e.store.userMemberships[e.owner.ID] = []UserMembership{{TenantID: e.tenantID, TenantSlug: "acme", Role: RoleOwner}}
	orgs, err := e.svc.UserOrganizations(context.Background(), e.owner.ID)
	if err != nil || len(orgs) != 1 || orgs[0].TenantSlug != "acme" {
		t.Fatalf("UserOrganizations = %v, %v", orgs, err)
	}

	for _, op := range []string{"Members", "Invitations", "UserMemberships"} {
		e.store.fail[op] = errBoom
	}
	if _, err := e.svc.Members(context.Background(), e.tenantID); !errors.Is(err, errBoom) {
		t.Errorf("Members deveria propagar o erro: %v", err)
	}
	if _, err := e.svc.Invitations(context.Background(), e.tenantID); !errors.Is(err, errBoom) {
		t.Errorf("Invitations deveria propagar o erro: %v", err)
	}
	if _, err := e.svc.UserOrganizations(context.Background(), e.owner.ID); !errors.Is(err, errBoom) {
		t.Errorf("UserOrganizations deveria propagar o erro: %v", err)
	}
}

// --- Convites ---

func TestInvite(t *testing.T) {
	e := newEnv(t)
	inv, token, err := e.svc.Invite(context.Background(), e.tenantID, e.access(e.owner), e.owner, "  Nova.Pessoa@ACME.com ", RoleFinance)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "nova.pessoa@acme.com" {
		t.Errorf("e-mail deve ser normalizado, obtido %q", inv.Email)
	}
	if inv.ID == uuid.Nil || inv.TenantID != e.tenantID || inv.Role != RoleFinance || inv.InvitedBy != e.owner.ID ||
		!inv.CreatedAt.Equal(testNow) || !inv.ExpiresAt.Equal(testNow.Add(7*24*time.Hour)) {
		t.Fatalf("convite inesperado: %+v", inv)
	}
	if inv.State(testNow) != InvitationPending {
		t.Fatalf("convite novo deve estar pendente: %s", inv.State(testNow))
	}

	// Somente o hash do token é persistido.
	stored := e.d().invitations[inv.ID]
	if token == "" || !bytes.Equal(stored.TokenHash, HashToken(token)) {
		t.Fatal("o convite deve guardar o hash do token devolvido")
	}
	if bytes.Contains(stored.TokenHash, []byte(token)) {
		t.Fatal("o token em claro não pode ser persistido")
	}

	if len(e.d().events) != 1 {
		t.Fatalf("esperado um evento, obtidos %d", len(e.d().events))
	}
	ev := e.d().events[0]
	if ev.Type != outbox.EventMemberInvited || ev.AggregateType != "invitation" || ev.AggregateID != inv.ID || ev.TenantID != e.tenantID {
		t.Fatalf("evento inesperado: %+v", ev)
	}
	a := e.lastAudit(t)
	if a.Action != "member.invited" || a.ResourceID != inv.ID.String() || a.Details["role"] != RoleFinance {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	// Nem o token nem o e-mail do convidado vão para o evento ou a auditoria.
	for name, v := range map[string]any{"evento": ev.Payload, "auditoria": a.Details} {
		s := fmt.Sprint(v)
		if strings.Contains(s, token) || strings.Contains(s, "nova.pessoa") {
			t.Errorf("%s expõe dados sensíveis: %s", name, s)
		}
	}
}

func TestInviteRejected(t *testing.T) {
	cases := []struct {
		name    string
		inviter Role
		email   string
		role    Role
		kind    apperr.Kind
		code    string
	}{
		{"e-mail inválido", RoleOwner, "nao-e-email", RoleAdmin, apperr.KindInvalid, "invalid_email"},
		{"papel desconhecido", RoleOwner, "x@acme.com", Role("root"), apperr.KindInvalid, "invalid_role"},
		{"admin não convida proprietário", RoleAdmin, "x@acme.com", RoleOwner, apperr.KindForbidden, "owner_only"},
		{"finance não convida proprietário", RoleFinance, "x@acme.com", RoleOwner, apperr.KindForbidden, "owner_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			inviter := e.addMember(tc.inviter, MembershipActive, "convidador@acme.com")
			inv, token, err := e.svc.Invite(context.Background(), e.tenantID, e.access(inviter), inviter, tc.email, tc.role)
			assertAppErr(t, err, tc.kind, tc.code)
			if token != "" || inv.ID != uuid.Nil || len(e.d().invitations) != 0 {
				t.Fatal("nenhum convite deveria ser criado")
			}
			if e.store.calls["InTenant"] != 0 {
				t.Fatal("validação deve ocorrer antes de abrir a transação")
			}
		})
	}
}

func TestOwnerInvitesOwner(t *testing.T) {
	e := newEnv(t)
	inv, _ := e.invite(t, "socia@acme.com", RoleOwner)
	if inv.Role != RoleOwner {
		t.Fatalf("papel = %s", inv.Role)
	}
}

func TestInviteUserLimitCountsPendingInvitations(t *testing.T) {
	e := newEnv(t) // plano com até 3 usuários e 1 membro ativo
	e.invite(t, "a@acme.com", RoleOperator)
	e.invite(t, "b@acme.com", RoleOperator)

	_, _, err := e.svc.Invite(context.Background(), e.tenantID, e.access(e.owner), e.owner, "c@acme.com", RoleOperator)
	assertAppErr(t, err, apperr.KindLimitExceeded, "platform_user_limit")
	if len(e.d().invitations) != 2 || len(e.d().events) != 2 {
		t.Fatal("convite acima do limite não pode ser gravado")
	}

	// Convites revogados deixam de ocupar lugar.
	var revoke uuid.UUID
	for id := range e.d().invitations {
		revoke = id
		break
	}
	if err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, revoke); err != nil {
		t.Fatal(err)
	}
	e.invite(t, "c@acme.com", RoleOperator)

	// Convites expirados também deixam de ocupar lugar.
	e.clock.Advance(InvitationTTL)
	e.invite(t, "d@acme.com", RoleOperator)
	e.invite(t, "e@acme.com", RoleOperator)
}

func TestInviteUnlimitedPlan(t *testing.T) {
	e := newEnv(t)
	e.d().plan.MaxUsers = nil
	for i := range 10 {
		e.invite(t, fmt.Sprintf("p%d@acme.com", i), RoleAuditor)
	}
}

func TestInviteStoreErrors(t *testing.T) {
	for _, op := range []string{"LockPlatformPlan", "PlatformUsage", "PendingInvitations", "InsertInvitation", "Emit", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.store.fail[op] = errBoom
			_, token, err := e.svc.Invite(context.Background(), e.tenantID, e.access(e.owner), e.owner, "x@acme.com", RoleAdmin)
			if !errors.Is(err, errBoom) || token != "" {
				t.Fatalf("esperado erro propagado sem token, obtido %v", err)
			}
			if len(e.d().invitations) != 0 {
				t.Fatal("convite deveria ser desfeito")
			}
		})
	}
}

func TestRevokeInvitation(t *testing.T) {
	e := newEnv(t)
	inv, token := e.invite(t, "x@acme.com", RoleAdmin)
	e.clock.Advance(time.Hour)
	if err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, inv.ID); err != nil {
		t.Fatal(err)
	}
	stored := e.d().invitations[inv.ID]
	if stored.RevokedAt == nil || !stored.RevokedAt.Equal(testNow.Add(time.Hour)) || stored.RevokedBy == nil || *stored.RevokedBy != e.owner.ID {
		t.Fatalf("revogação não registrada: %+v", stored)
	}
	if a := e.lastAudit(t); a.Action != "member.invitation_revoked" || a.ResourceID != inv.ID.String() {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	// Convite revogado não pode ser aceito.
	_, err := e.svc.AcceptInvitation(context.Background(), UserRef{ID: uuid.New(), Email: "x@acme.com"}, token)
	assertAppErr(t, err, apperr.KindInvalid, "invitation_invalid")
}

func TestRevokeInvitationNotPending(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *env, Invitation)
	}{
		{"já revogado", func(t *testing.T, e *env, inv Invitation) {
			if err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, inv.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"expirado", func(_ *testing.T, e *env, _ Invitation) { e.clock.Advance(InvitationTTL + time.Second) }},
		{"aceito", func(_ *testing.T, e *env, inv Invitation) {
			stored := e.d().invitations[inv.ID]
			at := testNow
			stored.AcceptedAt = &at
			e.d().invitations[inv.ID] = stored
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			inv, _ := e.invite(t, "x@acme.com", RoleAdmin)
			tc.setup(t, e, inv)
			err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, inv.ID)
			assertAppErr(t, err, apperr.KindConflict, "invitation_not_pending")
		})
	}
	t.Run("inexistente", func(t *testing.T) {
		e := newEnv(t)
		err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, uuid.New())
		assertAppErr(t, err, apperr.KindNotFound, "invitation_not_found")
	})
	for _, op := range []string{"UpdateInvitation", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newEnv(t)
			inv, _ := e.invite(t, "x@acme.com", RoleAdmin)
			e.store.fail[op] = errBoom
			if err := e.svc.RevokeInvitation(context.Background(), e.tenantID, e.owner, inv.ID); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.d().invitations[inv.ID].RevokedAt != nil {
				t.Fatal("revogação deveria ser desfeita")
			}
		})
	}
}

// --- Aceite ---

func TestAcceptInvitationCreatesMembership(t *testing.T) {
	e := newEnv(t)
	inv, token := e.invite(t, "nova@acme.com", RoleOperator)
	e.clock.Advance(2 * time.Hour)
	user := UserRef{ID: uuid.New(), Email: "NOVA@acme.com", DisplayName: "Nova"}

	m, err := e.svc.AcceptInvitation(context.Background(), user, token)
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt := testNow.Add(2 * time.Hour)
	if m.TenantID != e.tenantID || m.UserID != user.ID || m.Role != RoleOperator || m.Status != MembershipActive ||
		m.InvitedBy == nil || *m.InvitedBy != e.owner.ID || m.Version != 1 || !m.CreatedAt.Equal(acceptedAt) {
		t.Fatalf("associação inesperada: %+v", m)
	}
	if _, ok := e.d().members[user.ID]; !ok {
		t.Fatal("associação não persistida")
	}
	stored := e.d().invitations[inv.ID]
	if stored.State(acceptedAt) != InvitationAccepted || stored.AcceptedBy == nil || *stored.AcceptedBy != user.ID {
		t.Fatalf("convite deveria ficar aceito: %+v", stored)
	}
	if a := e.lastAudit(t); a.Action != "member.joined" || a.ResourceID != user.ID.String() || a.Details["invitation_id"] != inv.ID {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	// Uso único: o segundo aceite é rejeitado e não cria outra associação.
	_, err = e.svc.AcceptInvitation(context.Background(), user, token)
	assertAppErr(t, err, apperr.KindInvalid, "invitation_invalid")
	if len(e.d().members) != 2 {
		t.Fatalf("aceite repetido não pode criar associações, membros=%d", len(e.d().members))
	}
}

func TestAcceptInvitationReactivatesRemovedMember(t *testing.T) {
	e := newEnv(t)
	former := e.addMember(RoleAuditor, MembershipRemoved, "volta@acme.com")
	_, token := e.invite(t, "volta@acme.com", RoleFinance)

	m, err := e.svc.AcceptInvitation(context.Background(), former, token)
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != MembershipActive || m.Role != RoleFinance {
		t.Fatalf("membro removido deve ser reativado com o papel do convite: %+v", m)
	}
	if e.store.calls["InsertMembership"] != 0 {
		t.Fatal("reativação não deve inserir nova associação")
	}
	stored := e.d().members[former.ID]
	if stored.Status != MembershipActive || stored.Role != RoleFinance {
		t.Fatalf("reativação não persistida: %+v", stored)
	}
	if _, err := e.svc.ResolveAccess(context.Background(), e.tenantID, former.ID); err != nil {
		t.Fatalf("membro reativado deveria ter acesso: %v", err)
	}
}

func TestAcceptInvitationRejected(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *env, token string) (UserRef, string)
		kind  apperr.Kind
		code  string
	}{
		{"token curto", func(_ *env, _ string) (UserRef, string) {
			return UserRef{ID: uuid.New(), Email: "x@acme.com"}, strings.Repeat("a", 19)
		}, apperr.KindInvalid, "invitation_invalid"},
		{"token longo", func(_ *env, _ string) (UserRef, string) {
			return UserRef{ID: uuid.New(), Email: "x@acme.com"}, strings.Repeat("a", 201)
		}, apperr.KindInvalid, "invitation_invalid"},
		{"token desconhecido", func(_ *env, _ string) (UserRef, string) {
			other, _ := NewToken()
			return UserRef{ID: uuid.New(), Email: "x@acme.com"}, other
		}, apperr.KindInvalid, "invitation_invalid"},
		{"convite expirado", func(e *env, token string) (UserRef, string) {
			e.clock.Advance(InvitationTTL)
			return UserRef{ID: uuid.New(), Email: "x@acme.com"}, token
		}, apperr.KindInvalid, "invitation_invalid"},
		{"e-mail divergente", func(_ *env, token string) (UserRef, string) {
			return UserRef{ID: uuid.New(), Email: "outra@acme.com"}, token
		}, apperr.KindForbidden, "invitation_email_mismatch"},
		{"já é membro ativo", func(e *env, token string) (UserRef, string) {
			return e.addMember(RoleAuditor, MembershipActive, "x@acme.com"), token
		}, apperr.KindConflict, "already_member"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			inv, token := e.invite(t, "x@acme.com", RoleAdmin)
			user, tok := tc.setup(e, token)
			membersBefore := len(e.d().members)
			m, err := e.svc.AcceptInvitation(context.Background(), user, tok)
			assertAppErr(t, err, tc.kind, tc.code)
			if m != (Membership{}) {
				t.Errorf("nenhuma associação deveria ser devolvida: %+v", m)
			}
			if e.d().invitations[inv.ID].AcceptedAt != nil || len(e.d().members) != membersBefore {
				t.Fatal("convite e associações não podem mudar")
			}
		})
	}
}

// O aceite confere o limite de usuários de novo: depois de um rebaixamento do
// plano (ou de qualquer mudança que reduza as vagas), convites antigos não
// levam a organização além do limite.
func TestAcceptInvitationRechecksUserLimit(t *testing.T) {
	e := newEnv(t) // plano com até 3 usuários e 1 membro ativo
	_, first := e.invite(t, "a@acme.com", RoleOperator)
	_, second := e.invite(t, "b@acme.com", RoleOperator)
	one := 2
	e.d().plan.MaxUsers = &one

	if _, err := e.svc.AcceptInvitation(context.Background(), UserRef{ID: uuid.New(), Email: "a@acme.com"}, first); err != nil {
		t.Fatalf("o primeiro aceite cabe no limite: %v", err)
	}
	if e.store.calls["LockPlatformPlan"] < 3 {
		t.Fatal("o aceite deve ler o plano com bloqueio")
	}
	before := len(e.d().members)
	_, err := e.svc.AcceptInvitation(context.Background(), UserRef{ID: uuid.New(), Email: "b@acme.com"}, second)
	assertAppErr(t, err, apperr.KindLimitExceeded, "platform_user_limit")
	if len(e.d().members) != before {
		t.Fatal("aceite acima do limite não pode criar associação")
	}

	// Membro removido que volta também ocupa vaga.
	former := e.addMember(RoleAuditor, MembershipRemoved, "volta@acme.com")
	e.d().plan.MaxUsers = nil
	_, back := e.invite(t, "volta@acme.com", RoleFinance)
	e.d().plan.MaxUsers = &one
	_, err = e.svc.AcceptInvitation(context.Background(), former, back)
	assertAppErr(t, err, apperr.KindLimitExceeded, "platform_user_limit")
	if e.d().members[former.ID].Status != MembershipRemoved {
		t.Fatal("membro removido não pode ser reativado acima do limite")
	}
}

func TestAcceptInvitationValidatesTokenBeforeLookup(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.AcceptInvitation(context.Background(), UserRef{ID: uuid.New()}, "")
	assertAppErr(t, err, apperr.KindInvalid, "invitation_invalid")
	if e.store.calls["FindInvitation"] != 0 {
		t.Fatal("token com tamanho inválido não deve ser consultado")
	}
}

func TestAcceptInvitationStoreErrors(t *testing.T) {
	t.Run("falha na busca pelo token", func(t *testing.T) {
		e := newEnv(t)
		_, token := e.invite(t, "x@acme.com", RoleAdmin)
		e.store.fail["FindInvitation"] = errBoom
		_, err := e.svc.AcceptInvitation(context.Background(), UserRef{ID: uuid.New(), Email: "x@acme.com"}, token)
		if !errors.Is(err, errBoom) || apperr.IsKind(err, apperr.KindInvalid) {
			t.Fatalf("falha de infraestrutura não pode virar convite inválido: %v", err)
		}
	})
	newcomer := func(*env) UserRef { return UserRef{ID: uuid.New(), Email: "x@acme.com"} }
	removed := func(e *env) UserRef { return e.addMember(RoleAuditor, MembershipRemoved, "x@acme.com") }
	cases := []struct {
		op   string
		user func(*env) UserRef
	}{
		{"LockPlatformPlan", newcomer},
		{"Invitation", newcomer},
		{"Membership", newcomer},
		{"PlatformUsage", newcomer},
		{"InsertMembership", newcomer},
		{"UpdateMembership", removed},
		{"UpdateInvitation", newcomer},
		{"Audit", newcomer},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			e := newEnv(t)
			inv, token := e.invite(t, "x@acme.com", RoleAdmin)
			user := tc.user(e)
			e.store.fail[tc.op] = errBoom
			if _, err := e.svc.AcceptInvitation(context.Background(), user, token); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.d().invitations[inv.ID].AcceptedAt != nil {
				t.Fatal("aceite deveria ser desfeito")
			}
		})
	}
}

// --- Papéis e remoção ---

func TestChangeRole(t *testing.T) {
	e := newEnv(t)
	admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
	fin := e.addMember(RoleFinance, MembershipActive, "fin@acme.com")

	m, err := e.svc.ChangeRole(context.Background(), e.tenantID, e.access(admin), admin, fin.ID, RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleOperator || m.Version != 2 {
		t.Fatalf("associação inesperada: %+v", m)
	}
	if stored := e.d().members[fin.ID]; stored.Role != RoleOperator || stored.Version != 2 {
		t.Fatalf("alteração não persistida: %+v", stored)
	}
	a := e.lastAudit(t)
	if a.Action != "member.role_changed" || a.ResourceID != fin.ID.String() || a.Details["from"] != RoleFinance || a.Details["to"] != RoleOperator ||
		a.Actor != audit.User(admin.ID, admin.DisplayName) {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
}

func TestChangeRoleOwnerRules(t *testing.T) {
	cases := []struct {
		name   string
		run    func(e *env) (Membership, error)
		kind   apperr.Kind
		code   string
		wantOK Role
	}{
		{
			name: "admin não rebaixa proprietário",
			run: func(e *env) (Membership, error) {
				admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(admin), admin, e.owner.ID, RoleAdmin)
			},
			kind: apperr.KindForbidden, code: "owner_only",
		},
		{
			name: "admin não promove a proprietário",
			run: func(e *env) (Membership, error) {
				admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(admin), admin, admin.ID, RoleOwner)
			},
			kind: apperr.KindForbidden, code: "owner_only",
		},
		{
			name: "último proprietário não pode ser rebaixado",
			run: func(e *env) (Membership, error) {
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, e.owner.ID, RoleAdmin)
			},
			kind: apperr.KindConflict, code: "last_owner",
		},
		{
			name: "proprietário removido não conta como outro proprietário",
			run: func(e *env) (Membership, error) {
				e.addMember(RoleOwner, MembershipRemoved, "ex@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, e.owner.ID, RoleAdmin)
			},
			kind: apperr.KindConflict, code: "last_owner",
		},
		{
			name: "proprietário rebaixa outro proprietário",
			run: func(e *env) (Membership, error) {
				other := e.addMember(RoleOwner, MembershipActive, "socia@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, other.ID, RoleFinance)
			},
			wantOK: RoleFinance,
		},
		{
			name: "proprietário promove admin a proprietário",
			run: func(e *env) (Membership, error) {
				admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, admin.ID, RoleOwner)
			},
			wantOK: RoleOwner,
		},
		{
			name: "manter o papel de proprietário não exige outro proprietário",
			run: func(e *env) (Membership, error) {
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, e.owner.ID, RoleOwner)
			},
			wantOK: RoleOwner,
		},
		{
			name: "papel desconhecido",
			run: func(e *env) (Membership, error) {
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, e.owner.ID, Role("deus"))
			},
			kind: apperr.KindInvalid, code: "invalid_role",
		},
		{
			name: "membro inexistente",
			run: func(e *env) (Membership, error) {
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, uuid.New(), RoleAdmin)
			},
			kind: apperr.KindNotFound, code: "member_not_found",
		},
		{
			name: "membro removido",
			run: func(e *env) (Membership, error) {
				ex := e.addMember(RoleOperator, MembershipRemoved, "ex@acme.com")
				return e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, ex.ID, RoleAdmin)
			},
			kind: apperr.KindNotFound, code: "member_not_found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			m, err := tc.run(e)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("esperado sucesso, obtido %v", err)
				}
				if m.Role != tc.wantOK {
					t.Fatalf("papel = %s, esperado %s", m.Role, tc.wantOK)
				}
				return
			}
			assertAppErr(t, err, tc.kind, tc.code)
			if e.store.calls["UpdateMembership"] != 0 {
				t.Fatal("nenhuma associação deveria ser alterada")
			}
		})
	}
}

func TestChangeRoleStoreErrors(t *testing.T) {
	for _, op := range []string{"Membership", "CountActiveOwners", "UpdateMembership", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			other := e.addMember(RoleOwner, MembershipActive, "socia@acme.com")
			e.store.fail[op] = errBoom
			_, err := e.svc.ChangeRole(context.Background(), e.tenantID, e.access(e.owner), e.owner, other.ID, RoleAdmin)
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.d().members[other.ID].Role != RoleOwner {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

func TestRemoveMember(t *testing.T) {
	e := newEnv(t)
	admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
	op := e.addMember(RoleOperator, MembershipActive, "op@acme.com")

	if err := e.svc.RemoveMember(context.Background(), e.tenantID, e.access(admin), admin, op.ID); err != nil {
		t.Fatal(err)
	}
	if e.d().members[op.ID].Status != MembershipRemoved {
		t.Fatal("associação deveria ficar removida")
	}
	if a := e.lastAudit(t); a.Action != "member.removed" || a.ResourceID != op.ID.String() {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	// O membro removido perde o acesso na próxima requisição.
	_, err := e.svc.ResolveAccess(context.Background(), e.tenantID, op.ID)
	assertAppErr(t, err, apperr.KindForbidden, "no_membership")
}

func TestRemoveMemberRules(t *testing.T) {
	cases := []struct {
		name string
		run  func(e *env) error
		kind apperr.Kind
		code string
	}{
		{"admin não remove proprietário", func(e *env) error {
			admin := e.addMember(RoleAdmin, MembershipActive, "admin@acme.com")
			return e.svc.RemoveMember(context.Background(), e.tenantID, e.access(admin), admin, e.owner.ID)
		}, apperr.KindForbidden, "owner_only"},
		{"último proprietário não pode ser removido", func(e *env) error {
			return e.svc.RemoveMember(context.Background(), e.tenantID, e.access(e.owner), e.owner, e.owner.ID)
		}, apperr.KindConflict, "last_owner"},
		{"membro inexistente", func(e *env) error {
			return e.svc.RemoveMember(context.Background(), e.tenantID, e.access(e.owner), e.owner, uuid.New())
		}, apperr.KindNotFound, "member_not_found"},
		{"membro já removido", func(e *env) error {
			ex := e.addMember(RoleOperator, MembershipRemoved, "ex@acme.com")
			return e.svc.RemoveMember(context.Background(), e.tenantID, e.access(e.owner), e.owner, ex.ID)
		}, apperr.KindNotFound, "member_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			assertAppErr(t, tc.run(e), tc.kind, tc.code)
			if e.store.calls["UpdateMembership"] != 0 {
				t.Fatal("nenhuma associação deveria ser alterada")
			}
		})
	}
	t.Run("proprietário remove outro proprietário", func(t *testing.T) {
		e := newEnv(t)
		other := e.addMember(RoleOwner, MembershipActive, "socia@acme.com")
		if err := e.svc.RemoveMember(context.Background(), e.tenantID, e.access(e.owner), e.owner, other.ID); err != nil {
			t.Fatal(err)
		}
		if e.d().members[other.ID].Status != MembershipRemoved {
			t.Fatal("proprietária deveria ser removida")
		}
	})
}

func TestRemoveMemberStoreErrors(t *testing.T) {
	for _, op := range []string{"Membership", "CountActiveOwners", "UpdateMembership", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			other := e.addMember(RoleOwner, MembershipActive, "socia@acme.com")
			e.store.fail[op] = errBoom
			if err := e.svc.RemoveMember(context.Background(), e.tenantID, e.access(e.owner), e.owner, other.ID); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.d().members[other.ID].Status != MembershipActive {
				t.Fatal("remoção deveria ser desfeita")
			}
		})
	}
}

// xorSealer cifra de forma reversível para o teste e registra o AAD.
type xorSealer struct {
	aad []byte
	err error
}

func (s *xorSealer) Encrypt(plaintext, aad []byte) ([]byte, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	s.aad = aad
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0x5a
	}
	return out, "k1", nil
}

// Com o cifrador, o evento leva o token cifrado (nunca em claro) e vinculado
// ao tenant e ao convite, para o e-mail de convite.
func TestInviteSealsTokenForInvitationEmail(t *testing.T) {
	e := newEnv(t)
	sealer := &xorSealer{}
	e.svc.WithTokenSealer(sealer)
	inv, token := e.invite(t, "nova.pessoa@acme.com", RoleFinance)
	ev := e.d().events[0]
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload inesperado: %T", ev.Payload)
	}
	ct, _ := base64.StdEncoding.DecodeString(p["token_ciphertext"].(string))
	plain := make([]byte, len(ct))
	for i, b := range ct {
		plain[i] = b ^ 0x5a
	}
	if string(plain) != token || p["token_key_id"] != "k1" || strings.Contains(fmt.Sprint(p), token) {
		t.Fatalf("token cifrado inesperado: %v", p)
	}
	if !bytes.Equal(sealer.aad, InvitationTokenAAD(e.tenantID, inv.ID)) {
		t.Fatalf("AAD deve vincular tenant e convite: %s", sealer.aad)
	}
	sealer.err = errors.New("chave indisponível")
	if _, _, err := e.svc.Invite(context.Background(), e.tenantID, e.access(e.owner), e.owner, "outra@acme.com", RoleAuditor); err == nil {
		t.Fatal("falha ao cifrar deve impedir o convite")
	}
}

func TestAcceptURL(t *testing.T) {
	if got := AcceptURL("https://app.test/", "a+b/c"); got != "https://app.test/convite?token=a%2Bb%2Fc" {
		t.Fatal(got)
	}
	if got := AcceptURL("", "x"); got != "/convite?token=x" {
		t.Fatal(got)
	}
}
