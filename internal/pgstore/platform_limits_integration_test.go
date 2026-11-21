//go:build integration

package pgstore_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// limitedPlan cria um plano da plataforma do teste (inativo, para não
// aparecer na lista de planos de outros testes) e o atribui ao tenant pelo
// dono das tabelas, como faria um rebaixamento anterior às regras atuais.
func limitedPlan(t *testing.T, tn fixture.Tenant, maxUsers, maxSubscriptions *int) {
	t.Helper()
	ctx := context.Background()
	owner := pgtest.OwnerPool(t)
	code := "teste_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := owner.Exec(ctx, `INSERT INTO platform_plans (code, name, monthly_price_amount, max_users, max_active_subscriptions,
		features, trial_days, active, sort_order) VALUES ($1, 'Plano de teste', 9900, $2, $3, ARRAY['recurring_billing'], 0, false, 99)`,
		code, maxUsers, maxSubscriptions); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE tenant_platform_subscriptions SET plan_code = $1 WHERE tenant_id = $2`, code, tn.ID); err != nil {
		t.Fatal(err)
	}
}

func intp(n int) *int { return &n }

func ownerAccess(tn fixture.Tenant) (tenancy.Access, tenancy.UserRef) {
	return tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner},
		tenancy.UserRef{ID: tn.Owner.ID, Email: tn.Owner.Email, DisplayName: tn.Owner.DisplayName}
}

// newUser cria um usuário sem organização.
func newUser(t *testing.T, env *fixture.Env) tenancy.UserRef {
	t.Helper()
	uid := ids.New()
	u, err := env.Store.Users().UpsertFromClaims(context.Background(), uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@convidado.test", EmailVerified: true, Name: "Convidado"}, env.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}
}

// invite convida o usuário pelo proprietário e devolve o token.
func invite(t *testing.T, env *fixture.Env, tn fixture.Tenant, u tenancy.UserRef, role tenancy.Role) string {
	t.Helper()
	acc, ref := ownerAccess(tn)
	_, token, err := env.Tenancy.Invite(context.Background(), tn.ID, acc, ref, u.Email, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if e, ok := apperr.As(err); !ok || e.Code != code {
		t.Fatalf("esperado %s, obtido %v", code, err)
	}
}

// holdingTenancy interrompe a primeira operação logo depois do passo
// indicado (com a transação ainda aberta) até release ser fechado.
type holdingTenancy struct {
	*pgstore.Tenancy
	after   string
	reached chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func newHoldingTenancy(env *fixture.Env, after string) *holdingTenancy {
	return &holdingTenancy{Tenancy: env.Store.Tenancy(), after: after, reached: make(chan struct{}), release: make(chan struct{})}
}

func (s *holdingTenancy) hold(step string) {
	if step == s.after && s.held.CompareAndSwap(false, true) {
		close(s.reached)
		<-s.release
	}
}

func (s *holdingTenancy) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(tenancy.Tx) error) error {
	return s.Tenancy.InTenant(ctx, tenantID, func(tx tenancy.Tx) error { return fn(holdingTenancyTx{Tx: tx, s: s}) })
}

type holdingTenancyTx struct {
	tenancy.Tx
	s *holdingTenancy
}

func (tx holdingTenancyTx) PlatformUsage(ctx context.Context) (platformbilling.Usage, error) {
	u, err := tx.Tx.PlatformUsage(ctx)
	tx.s.hold("PlatformUsage")
	return u, err
}

func (tx holdingTenancyTx) PendingInvitations(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.Tx.PendingInvitations(ctx, now)
	tx.s.hold("PendingInvitations")
	return n, err
}

func (tx holdingTenancyTx) CountActiveOwners(ctx context.Context) (int, error) {
	n, err := tx.Tx.CountActiveOwners(ctx)
	tx.s.hold("CountActiveOwners")
	return n, err
}

// concurrently executa first até o ponto de parada, inicia second e confere
// que second aguarda um bloqueio no banco antes de liberar first. Sem a
// serialização, second termina sozinho e o teste falha.
func concurrently(t *testing.T, reached chan struct{}, release func(), first, second func() error) (error, error) {
	t.Helper()
	firstErr := make(chan error, 1)
	go func() { firstErr <- first() }()
	select {
	case <-reached:
	case err := <-firstErr:
		t.Fatalf("a primeira operação terminou antes do ponto de parada: %v", err)
	}
	var secondErr error
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		secondErr = second()
	}()
	blocked := waitLockWaiters(t, pgtest.OwnerPool(t), 1, secondDone)
	release()
	err1 := <-firstErr
	<-secondDone
	if !blocked {
		t.Fatalf("a segunda operação não aguardou a primeira (resultado: %v)", secondErr)
	}
	return err1, secondErr
}

func pendingInvitations(t *testing.T, tn fixture.Tenant) int {
	t.Helper()
	var n int
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT count(*)::int FROM invitations
		WHERE tenant_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, tn.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func activeMembers(t *testing.T, tn fixture.Tenant) int {
	t.Helper()
	var n int
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT count(*)::int FROM memberships WHERE tenant_id = $1 AND status = 'active'`,
		tn.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Dois convites simultâneos com uma vaga restante no plano: o segundo
// aguarda o bloqueio da assinatura da plataforma, conta o convite do
// primeiro e é recusado.
func TestConcurrentInvitationsRespectUserLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "convite-limite") // plano essencial: até 3 usuários
	invite(t, env, tn, newUser(t, env), tenancy.RoleOperator)

	store := newHoldingTenancy(env, "PendingInvitations")
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	svc := tenancy.NewService(store, env.Clock)
	acc, ref := ownerAccess(tn)
	a, b := newUser(t, env), newUser(t, env)
	err1, err2 := concurrently(t, store.reached, release,
		func() error { _, _, err := svc.Invite(ctx, tn.ID, acc, ref, a.Email, tenancy.RoleAuditor); return err },
		func() error { _, _, err := svc.Invite(ctx, tn.ID, acc, ref, b.Email, tenancy.RoleAuditor); return err })
	if err1 != nil {
		t.Fatalf("o primeiro convite ocupa a última vaga: %v", err1)
	}
	wantCode(t, err2, "platform_user_limit")
	if n := pendingInvitations(t, tn); n != 2 {
		t.Fatalf("convites pendentes = %d, esperado 2 (com o proprietário, 3 vagas)", n)
	}
}

// Dois aceites simultâneos de convites diferentes depois de um rebaixamento
// que deixou uma vaga: o segundo aguarda o primeiro e é recusado.
func TestConcurrentAcceptsRespectUserLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "aceite-limite")
	a, b := newUser(t, env), newUser(t, env)
	tokA, tokB := invite(t, env, tn, a, tenancy.RoleOperator), invite(t, env, tn, b, tenancy.RoleOperator)
	limitedPlan(t, tn, intp(2), nil)

	store := newHoldingTenancy(env, "PlatformUsage")
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	svc := tenancy.NewService(store, env.Clock)
	err1, err2 := concurrently(t, store.reached, release,
		func() error { _, err := svc.AcceptInvitation(ctx, a, tokA); return err },
		func() error { _, err := svc.AcceptInvitation(ctx, b, tokB); return err })
	if err1 != nil {
		t.Fatalf("o primeiro aceite ocupa a última vaga: %v", err1)
	}
	wantCode(t, err2, "platform_user_limit")
	if n := activeMembers(t, tn); n != 2 {
		t.Fatalf("membros ativos = %d, limite 2", n)
	}
}

// Rebaixamento: convites pendentes contam no limite do plano de destino, e o
// aceite confere o limite de novo (o plano pode ter mudado por outro
// caminho depois do convite).
func TestDowngradeCountsPendingInvitationsAndAcceptRechecksLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "rebaixamento")
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	first := newUser(t, env)
	if _, err := env.Tenancy.AcceptInvitation(ctx, first, invite(t, env, tn, first, tenancy.RoleOperator)); err != nil {
		t.Fatal(err)
	}
	b, c := newUser(t, env), newUser(t, env)
	tokB, tokC := invite(t, env, tn, b, tenancy.RoleOperator), invite(t, env, tn, c, tenancy.RoleOperator)

	// 2 membros + 2 convites pendentes não cabem nos 3 usuários do essencial.
	_, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "essencial")
	wantCode(t, err, "platform_downgrade_users")
	ov, err := env.Platform.Overview(ctx, tn.ID)
	if err != nil || ov.Plan.Code != "profissional" || ov.Usage.ActiveUsers != 2 || ov.Usage.PendingInvitations != 2 {
		t.Fatalf("o plano não pode mudar: %+v %v", ov, err)
	}

	// Plano rebaixado por fora das regras (dado anterior a elas): o aceite
	// recusa a entrada além do limite.
	limitedPlan(t, tn, intp(3), nil)
	if _, err := env.Tenancy.AcceptInvitation(ctx, b, tokB); err != nil {
		t.Fatalf("o terceiro usuário cabe: %v", err)
	}
	_, err = env.Tenancy.AcceptInvitation(ctx, c, tokC)
	wantCode(t, err, "platform_user_limit")
	if n := activeMembers(t, tn); n != 3 {
		t.Fatalf("membros ativos = %d, limite 3", n)
	}
}

// holdingSubscriptions interrompe a primeira criação depois da contagem de
// contratos ativos, com a transação aberta.
type holdingSubscriptions struct {
	*pgstore.Subscriptions
	reached chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func (s *holdingSubscriptions) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(subscriptions.Tx) error) error {
	return s.Subscriptions.InTenant(ctx, tenantID, func(tx subscriptions.Tx) error { return fn(holdingSubscriptionsTx{Tx: tx, s: s}) })
}

type holdingSubscriptionsTx struct {
	subscriptions.Tx
	s *holdingSubscriptions
}

func (tx holdingSubscriptionsTx) PlatformUsage(ctx context.Context) (platformbilling.Usage, error) {
	u, err := tx.Tx.PlatformUsage(ctx)
	if tx.s.held.CompareAndSwap(false, true) {
		close(tx.s.reached)
		<-tx.s.release
	}
	return u, err
}

func activeSubscriptions(t *testing.T, tn fixture.Tenant) int {
	t.Helper()
	var n int
	if err := pgtest.OwnerPool(t).QueryRow(context.Background(), `SELECT count(*)::int FROM subscriptions
		WHERE tenant_id = $1 AND status IN ('trialing', 'active', 'past_due')`, tn.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Duas criações simultâneas de assinatura com uma vaga de contrato: a
// segunda aguarda o bloqueio da assinatura da plataforma e é recusada.
func TestConcurrentSubscriptionCreationRespectsLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "assinatura-limite")
	plan := env.NewFlatPlan(t, tn, 10000)
	c1, c2 := env.NewCustomer(t, tn), env.NewCustomer(t, tn)
	limitedPlan(t, tn, nil, intp(1))

	store := &holdingSubscriptions{Subscriptions: env.Store.Subscriptions(), reached: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	svc := subscriptions.NewService(store, env.Clock)
	zero := 0
	create := func(customerID uuid.UUID) func() error {
		return func() error {
			_, err := svc.Create(ctx, tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: customerID, PlanID: plan.ID, Quantity: 1, TrialDays: &zero})
			return err
		}
	}
	err1, err2 := concurrently(t, store.reached, release, create(c1.ID), create(c2.ID))
	if err1 != nil {
		t.Fatalf("a primeira criação ocupa a vaga: %v", err1)
	}
	wantCode(t, err2, "platform_subscription_limit")
	if n := activeSubscriptions(t, tn); n != 1 {
		t.Fatalf("contratos ativos = %d, limite 1", n)
	}
}

// Suspender, criar outra e reativar não passa do limite de contratos: a
// reativação ocupa vaga como a criação.
func TestReactivationRespectsSubscriptionLimit(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "reativacao-limite")
	plan := env.NewFlatPlan(t, tn, 10000)
	limitedPlan(t, tn, nil, intp(1))
	first := env.NewSubscription(t, tn, env.NewCustomer(t, tn).ID, plan.ID, nil)
	if _, err := env.Subscriptions.Suspend(ctx, tn.ID, tn.Actor, first.ID, "pausa combinada", 0); err != nil {
		t.Fatal(err)
	}
	env.NewSubscription(t, tn, env.NewCustomer(t, tn).ID, plan.ID, nil) // a vaga liberada é ocupada

	_, err := env.Subscriptions.Reactivate(ctx, tn.ID, tn.Actor, first.ID, 0)
	wantCode(t, err, "platform_subscription_limit")
	got, err := env.Subscriptions.Get(ctx, tn.ID, first.ID)
	if err != nil || got.Status != subscriptions.Suspended || activeSubscriptions(t, tn) != 1 {
		t.Fatalf("a assinatura deve continuar suspensa: %s %v", got.Status, err)
	}
}

// Convites contra o banco: expirado, revogado e já usado são recusados, e
// aceites simultâneos do mesmo convite criam uma única associação.
func TestInvitationLifecycleAgainstDatabase(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 6, now)
	tn := env.NewTenant(t, "convite-ciclo")
	acc, ref := ownerAccess(tn)

	expired := newUser(t, env)
	tokExpired := invite(t, env, tn, expired, tenancy.RoleAuditor)
	revoked := newUser(t, env)
	inv, tokRevoked, err := env.Tenancy.Invite(ctx, tn.ID, acc, ref, revoked.Email, tenancy.RoleAuditor)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Tenancy.RevokeInvitation(ctx, tn.ID, ref, inv.ID); err != nil {
		t.Fatal(err)
	}
	_, err = env.Tenancy.AcceptInvitation(ctx, revoked, tokRevoked)
	wantCode(t, err, "invitation_invalid")

	used := newUser(t, env)
	tokUsed := invite(t, env, tn, used, tenancy.RoleFinance)
	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			_, errs[i] = env.Tenancy.AcceptInvitation(ctx, used, tokUsed)
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		if e, isApp := apperr.As(err); !isApp || (e.Code != "invitation_invalid" && e.Code != "already_member") {
			t.Fatalf("aceite concorrente com erro inesperado: %v", err)
		}
	}
	var memberships int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT count(*)::int FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tn.ID, used.ID).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || memberships != 1 {
		t.Fatalf("aceites bem-sucedidos = %d, associações = %d; esperado 1 e 1", ok, memberships)
	}
	_, err = env.Tenancy.AcceptInvitation(ctx, used, tokUsed)
	wantCode(t, err, "invitation_invalid")

	env.Clock.Advance(tenancy.InvitationTTL)
	_, err = env.Tenancy.AcceptInvitation(ctx, expired, tokExpired)
	wantCode(t, err, "invitation_invalid")
}

// Dois proprietários rebaixando um ao outro ao mesmo tempo: o bloqueio das
// associações de proprietários serializa as alterações e a organização
// mantém um proprietário.
func TestConcurrentDemotionKeepsLastOwner(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "ultimo-dono")
	second := newUser(t, env)
	if _, err := env.Tenancy.AcceptInvitation(ctx, second, invite(t, env, tn, second, tenancy.RoleOwner)); err != nil {
		t.Fatal(err)
	}
	_, firstRef := ownerAccess(tn)
	firstAcc := tenancy.Access{TenantID: tn.ID, UserID: tn.Owner.ID, Role: tenancy.RoleOwner}
	secondAcc := tenancy.Access{TenantID: tn.ID, UserID: second.ID, Role: tenancy.RoleOwner}

	store := newHoldingTenancy(env, "CountActiveOwners")
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	svc := tenancy.NewService(store, env.Clock)
	err1, err2 := concurrently(t, store.reached, release,
		func() error {
			_, err := svc.ChangeRole(ctx, tn.ID, firstAcc, firstRef, second.ID, tenancy.RoleAdmin)
			return err
		},
		func() error {
			_, err := svc.ChangeRole(ctx, tn.ID, secondAcc, second, tn.Owner.ID, tenancy.RoleAdmin)
			return err
		})
	if err1 != nil {
		t.Fatalf("o primeiro rebaixamento deveria passar: %v", err1)
	}
	wantCode(t, err2, "last_owner")
	var owners int
	if err := pgtest.OwnerPool(t).QueryRow(ctx, `SELECT count(*)::int FROM memberships WHERE tenant_id = $1 AND role = 'owner' AND status = 'active'`,
		tn.ID).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("proprietários ativos = %d %v", owners, err)
	}
}

// Um usuário em duas organizações tem papel e acesso próprios em cada uma;
// sair de uma não afeta a outra.
func TestUserInTwoOrganizations(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	a, b := env.NewTenant(t, "duas-a"), env.NewTenant(t, "duas-b")
	u := newUser(t, env)
	if _, err := env.Tenancy.AcceptInvitation(ctx, u, invite(t, env, a, u, tenancy.RoleFinance)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Tenancy.AcceptInvitation(ctx, u, invite(t, env, b, u, tenancy.RoleAuditor)); err != nil {
		t.Fatal(err)
	}
	orgs, err := env.Tenancy.UserOrganizations(ctx, u.ID)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("organizações: %+v %v", orgs, err)
	}
	roles := map[uuid.UUID]tenancy.Role{}
	for _, o := range orgs {
		roles[o.TenantID] = o.Role
	}
	if roles[a.ID] != tenancy.RoleFinance || roles[b.ID] != tenancy.RoleAuditor {
		t.Fatalf("papéis por organização: %v", roles)
	}
	for tenantID, role := range roles {
		acc, err := env.Tenancy.ResolveAccess(ctx, tenantID, u.ID)
		if err != nil || acc.Role != role {
			t.Fatalf("acesso em %s: %+v %v", tenantID, acc, err)
		}
	}
	accA, refA := ownerAccess(a)
	if err := env.Tenancy.RemoveMember(ctx, a.ID, accA, refA, u.ID); err != nil {
		t.Fatal(err)
	}
	_, err = env.Tenancy.ResolveAccess(ctx, a.ID, u.ID)
	wantCode(t, err, "no_membership")
	if acc, err := env.Tenancy.ResolveAccess(ctx, b.ID, u.ID); err != nil || acc.Role != tenancy.RoleAuditor {
		t.Fatalf("a outra organização continua acessível: %+v %v", acc, err)
	}
	if orgs, err := env.Tenancy.UserOrganizations(ctx, u.ID); err != nil || len(orgs) != 1 || orgs[0].TenantID != b.ID {
		t.Fatalf("organizações depois da remoção: %+v %v", orgs, err)
	}
}

// O pedido de implantação assistida é atendido pela equipe da plataforma
// (agendado e concluído com auditoria na trilha do tenant) e cobrado uma vez
// no demonstrativo do período da conclusão; o banco impede que o pedido
// encerrado mude ou que a cobrança seja refeita.
func TestAddonOrderHandlingAndStatement(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "implantacao")
	other := env.NewTenant(t, "implantacao-outra")
	order, err := env.Platform.RequestAddon(ctx, tn.ID, tn.Actor, tn.Owner.ID, "implantacao_assistida", "Migrar 300 contratos")
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := env.Platform.RequestAddon(ctx, other.ID, other.Actor, other.Owner.ID, "implantacao_assistida", "")
	if err != nil {
		t.Fatal(err)
	}
	open := staffOrders(t, env, "", 50)
	var found *platformbilling.StaffAddonOrder
	for i := range open {
		if open[i].ID == order.ID {
			found = &open[i]
		}
	}
	if found == nil || found.TenantID != tn.ID || found.TenantName != "Org implantacao" || found.AddonName != "Implantação assistida" ||
		found.Price.Amount() != 250000 || found.Notes != "Migrar 300 contratos" {
		t.Fatalf("pedido em aberto na lista da equipe: %+v", found)
	}

	staffUser := newUser(t, env)
	staff := platformbilling.Staff{UserID: staffUser.ID, DisplayName: "Equipe Recorvia"}
	at := env.Clock.Now().Add(72 * time.Hour)
	if _, err := env.Platform.ScheduleAddonOrder(ctx, staff, tn.ID, order.ID, at); err != nil {
		t.Fatal(err)
	}
	// O tenant do caminho delimita o pedido: outro tenant não o encontra.
	_, err = env.Platform.CompleteAddonOrder(ctx, staff, other.ID, order.ID)
	wantCode(t, err, "addon_order_not_found")
	if _, err := env.Platform.CancelAddonOrder(ctx, staff, other.ID, canceled.ID, "Cliente desistiu."); err != nil {
		t.Fatal(err)
	}
	env.Clock.Advance(time.Hour)
	done, err := env.Platform.CompleteAddonOrder(ctx, staff, tn.ID, order.ID)
	if err != nil || done.Status != platformbilling.AddonCompleted || done.ScheduledFor == nil || !done.ScheduledFor.Equal(at) {
		t.Fatalf("conclusão: %+v %v", done, err)
	}
	owner := pgtest.OwnerPool(t)
	var actions []string
	rows, err := owner.Query(ctx, `SELECT action FROM audit_log WHERE tenant_id = $1 AND resource_id = $2 AND actor_id = $3 ORDER BY occurred_at, action`,
		tn.ID, order.ID.String(), staffUser.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	rows.Close()
	if strings.Join(actions, ",") != "platform.addon_scheduled,platform.addon_completed" {
		t.Fatalf("auditoria do atendimento: %v", actions)
	}

	// Pedido encerrado não volta a abrir nem por SQL direto.
	if _, err := owner.Exec(ctx, `UPDATE platform_addon_orders SET status = 'scheduled', completed_at = NULL WHERE id = $1`, order.ID); err == nil {
		t.Fatal("o banco deveria recusar a reabertura do pedido concluído")
	}

	// Fecha o período em que a conclusão ocorreu (a avaliação do plano não
	// isenta o serviço adicional).
	ov, err := env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Set(ov.Subscription.PeriodEnd.Add(time.Minute))
	if _, err := env.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	sts, err := env.Store.Platform().Statements(ctx, tn.ID)
	if err != nil || len(sts) != 1 {
		t.Fatalf("demonstrativos: %+v %v", sts, err)
	}
	if sts[0].AddonsAmount.Amount() != 250000 || !sts[0].PeriodStart.Equal(ov.Subscription.PeriodStart) {
		t.Fatalf("o serviço deve entrar no demonstrativo do período da conclusão: %+v", sts[0])
	}
	orders, err := env.Store.Platform().AddonOrders(ctx, tn.ID)
	if err != nil || len(orders) != 1 || orders[0].StatementID == nil || *orders[0].StatementID != sts[0].ID {
		t.Fatalf("o pedido deve ficar vinculado ao demonstrativo: %+v %v", orders, err)
	}
	if _, err := owner.Exec(ctx, `UPDATE platform_addon_orders SET statement_id = NULL WHERE id = $1`, order.ID); err == nil {
		t.Fatal("o banco deveria recusar desfazer a cobrança")
	}
	ov, err = env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Set(ov.Subscription.PeriodEnd.Add(time.Minute))
	if _, err := env.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	if sts, err := env.Store.Platform().Statements(ctx, tn.ID); err != nil || len(sts) != 2 || sts[0].AddonsAmount.Amount() != 0 {
		t.Fatalf("o serviço não pode ser cobrado de novo: %+v %v", sts, err)
	}
	otherSts, err := env.Store.Platform().Statements(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range otherSts {
		if s.AddonsAmount.Amount() != 0 {
			t.Fatalf("pedido cancelado não é cobrado: %+v", s)
		}
	}
}

// staffOrders percorre todas as páginas da lista da equipe com o filtro e o
// tamanho de página informados, conferindo que nenhum pedido se repete.
func staffOrders(t *testing.T, env *fixture.Env, status string, limit int) []platformbilling.StaffAddonOrder {
	t.Helper()
	var out []platformbilling.StaffAddonOrder
	seen := map[uuid.UUID]bool{}
	cursor := ""
	for range 1000 {
		pg, err := env.Platform.StaffAddonOrders(context.Background(), status, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(pg.Items) > limit {
			t.Fatalf("página maior que o limite: %d", len(pg.Items))
		}
		for _, o := range pg.Items {
			if seen[o.ID] {
				t.Fatalf("pedido repetido entre páginas: %s", o.ID)
			}
			seen[o.ID] = true
		}
		out = append(out, pg.Items...)
		if pg.NextCursor == "" {
			return out
		}
		cursor = pg.NextCursor
	}
	t.Fatal("paginação sem fim")
	return nil
}

// positions devolve a ordem relativa dos pedidos informados na lista.
func positions(list []platformbilling.StaffAddonOrder, ids ...uuid.UUID) []int {
	idx := map[uuid.UUID]int{}
	for i, o := range list {
		idx[o.ID] = i
	}
	out := make([]int, len(ids))
	for i, id := range ids {
		p, ok := idx[id]
		if !ok {
			p = -1
		}
		out[i] = p
	}
	return out
}

func ascending(p []int) bool {
	for i := range p {
		if p[i] < 0 || (i > 0 && p[i] <= p[i-1]) {
			return false
		}
	}
	return true
}

// A lista da equipe é paginada por cursor sem perder nem repetir pedidos:
// em aberto pela solicitação, agendados pela data do atendimento,
// concluídos e cancelados do encerramento mais recente para o mais antigo.
func TestStaffAddonOrdersPagination(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "pagina-equipe")
	staff := platformbilling.Staff{UserID: newUser(t, env).ID, DisplayName: "Equipe"}
	var ids []uuid.UUID
	for range 4 {
		o, err := env.Platform.RequestAddon(ctx, tn.ID, tn.Actor, tn.Owner.ID, "implantacao_assistida", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
		env.Clock.Advance(time.Minute)
	}
	if p := positions(staffOrders(t, env, "", 1), ids...); !ascending(p) {
		t.Fatalf("em aberto pela solicitação: %v", p)
	}
	// Agendados em ordem inversa à da solicitação.
	for i, id := range ids {
		if _, err := env.Platform.ScheduleAddonOrder(ctx, staff, tn.ID, id, env.Clock.Now().Add(time.Duration(10-i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if p := positions(staffOrders(t, env, "scheduled", 2), ids[3], ids[2], ids[1], ids[0]); !ascending(p) {
		t.Fatalf("agendados pela data do atendimento: %v", p)
	}
	// Concluídos (0, 2) e cancelados (1, 3), cada um num instante.
	for i, id := range ids {
		env.Clock.Advance(time.Minute)
		var err error
		if i%2 == 0 {
			_, err = env.Platform.CompleteAddonOrder(ctx, staff, tn.ID, id)
		} else {
			_, err = env.Platform.CancelAddonOrder(ctx, staff, tn.ID, id, "Cliente desistiu.")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if p := positions(staffOrders(t, env, "completed", 1), ids[2], ids[0]); !ascending(p) {
		t.Fatalf("concluídos do mais recente para o mais antigo: %v", p)
	}
	if p := positions(staffOrders(t, env, "canceled", 1), ids[3], ids[1]); !ascending(p) {
		t.Fatalf("cancelados do mais recente para o mais antigo: %v", p)
	}
	if p := positions(staffOrders(t, env, "", 50), ids...); p[0] != -1 || p[1] != -1 || p[2] != -1 || p[3] != -1 {
		t.Fatalf("encerrados não estão em aberto: %v", p)
	}
}

// holdingPlatform interrompe o fechamento do demonstrativo do tenant
// indicado logo depois de somar os pedidos não cobrados, com a transação
// aberta (os demais tenants do banco compartilhado seguem normalmente).
type holdingPlatform struct {
	*pgstore.Platform
	tenant  uuid.UUID
	reached chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func (s *holdingPlatform) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(platformbilling.Tx) error) error {
	return s.Platform.InTenant(ctx, tenantID, func(tx platformbilling.Tx) error {
		return fn(holdingPlatformTx{Tx: tx, s: s, target: tenantID == s.tenant})
	})
}

type holdingPlatformTx struct {
	platformbilling.Tx
	s      *holdingPlatform
	target bool
}

func (tx holdingPlatformTx) UnbilledAddonsTotal(ctx context.Context, before time.Time) (money.Money, error) {
	m, err := tx.Tx.UnbilledAddonsTotal(ctx, before)
	if tx.target && tx.s.held.CompareAndSwap(false, true) {
		close(tx.s.reached)
		<-tx.s.release
	}
	return m, err
}

func billedAddons(t *testing.T, env *fixture.Env, tn fixture.Tenant) (int64, []platformbilling.Statement) {
	t.Helper()
	sts, err := env.Store.Platform().Statements(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, s := range sts {
		total += s.AddonsAmount.Amount()
	}
	return total, sts
}

// Conclusão simultânea ao fechamento, com o relógio da API atrás do relógio
// do worker (a conclusão registra um instante anterior ao fim do período já
// em fechamento): a conclusão espera o bloqueio da assinatura da
// plataforma, e o pedido é cobrado uma vez, no demonstrativo seguinte. Sem
// a espera, o fechamento vincularia ao demonstrativo um pedido que não
// somou, e a cobrança se perderia.
func TestAddonCompletionConcurrentWithStatementClose(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "conclusao-fechamento")
	order, err := env.Platform.RequestAddon(ctx, tn.ID, tn.Actor, tn.Owner.ID, "implantacao_assistida", "")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	end := ov.Subscription.PeriodEnd
	workerClock := clock.NewManual(end.Add(time.Minute))
	apiClock := clock.NewManual(end.Add(-time.Minute))
	store := &holdingPlatform{Platform: env.Store.Platform(), tenant: tn.ID, reached: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(release)
	worker := platformbilling.NewService(store, workerClock)
	api := platformbilling.NewService(env.Store.Platform(), apiClock)
	staff := platformbilling.Staff{UserID: newUser(t, env).ID, DisplayName: "Equipe"}

	errClose, errComplete := concurrently(t, store.reached, release,
		func() error { _, err := worker.CloseDueStatements(ctx); return err },
		func() error { _, err := api.CompleteAddonOrder(ctx, staff, tn.ID, order.ID); return err })
	if errClose != nil || errComplete != nil {
		t.Fatalf("fechamento: %v; conclusão: %v", errClose, errComplete)
	}
	if total, _ := billedAddons(t, env, tn); total != 0 {
		t.Fatalf("o demonstrativo em fechamento não inclui o pedido concluído durante ele: %d", total)
	}
	ov, err = env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerClock.Set(ov.Subscription.PeriodEnd.Add(time.Minute))
	if _, err := worker.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	total, sts := billedAddons(t, env, tn)
	if total != 250000 || sts[0].AddonsAmount.Amount() != 250000 {
		t.Fatalf("o pedido é cobrado uma vez, no demonstrativo seguinte: %d %+v", total, sts)
	}
}

// Pedido concluído depois do fim do período e antes do fechamento: entra no
// demonstrativo do período em que a conclusão ocorreu (o seguinte), não no
// que estava por fechar.
func TestAddonCompletedAfterPeriodEndBeforeClose(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "conclusao-atrasada")
	order, err := env.Platform.RequestAddon(ctx, tn.ID, tn.Actor, tn.Owner.ID, "implantacao_assistida", "")
	if err != nil {
		t.Fatal(err)
	}
	ov, err := env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Set(ov.Subscription.PeriodEnd.Add(time.Hour)) // o fechamento ainda não rodou
	if _, err := env.Platform.CompleteAddonOrder(ctx, platformbilling.Staff{UserID: newUser(t, env).ID, DisplayName: "Equipe"}, tn.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	if total, sts := billedAddons(t, env, tn); total != 0 || len(sts) != 1 {
		t.Fatalf("o período encerrado antes da conclusão não cobra o pedido: %d %+v", total, sts)
	}
	ov, err = env.Platform.Overview(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.Clock.Set(ov.Subscription.PeriodEnd.Add(time.Minute))
	if _, err := env.Platform.CloseDueStatements(ctx); err != nil {
		t.Fatal(err)
	}
	if total, sts := billedAddons(t, env, tn); total != 250000 || len(sts) != 2 || sts[0].AddonsAmount.Amount() != 250000 {
		t.Fatalf("cobrado no demonstrativo do período da conclusão: %d %+v", total, sts)
	}
}
