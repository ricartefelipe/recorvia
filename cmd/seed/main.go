// Comando seed: cria dados de DEMONSTRAÇÃO (dois tenants, membros, provedor
// simulado e catálogo/cliente de exemplo). É idempotente: pode ser executado
// várias vezes sem duplicar dados. Todos os nomes indicam "demonstração".
//
// Os usuários correspondem aos usuários de demonstração do realm local do
// Keycloak (deploy/keycloak/realm-recorvia.json), com os mesmos identificadores.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

type demoUser struct {
	ID    string
	Email string
	Name  string
}

var (
	ana   = demoUser{"7f3c6a2e-0b1d-4c55-9a8e-1a2b3c4d5e01", "ana.owner@acme.test", "Ana Proprietária (demonstração)"}
	bruno = demoUser{"7f3c6a2e-0b1d-4c55-9a8e-1a2b3c4d5e02", "bruno.finance@acme.test", "Bruno Financeiro (demonstração)"}
	davi  = demoUser{"7f3c6a2e-0b1d-4c55-9a8e-1a2b3c4d5e03", "davi.auditor@acme.test", "Davi Auditor (demonstração)"}
	carla = demoUser{"7f3c6a2e-0b1d-4c55-9a8e-1a2b3c4d5e04", "carla.owner@beta.test", "Carla Proprietária (demonstração)"}
)

func main() {
	log := logging.New(os.Stdout, os.Getenv("RECORVIA_LOG_LEVEL"), "seed")
	if err := run(log); err != nil {
		log.Error("seed falhou", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.OIDCIssuer == "" {
		return fmt.Errorf("RECORVIA_OIDC_ISSUER é obrigatório para vincular os usuários de demonstração")
	}
	core, err := app.Open(ctx, cfg, log, "recorvia-seed")
	if err != nil {
		return err
	}
	defer core.Close()
	s := &seeder{core: core, log: log, issuer: cfg.OIDCIssuer}

	users := map[string]identity.User{}
	for _, u := range []demoUser{ana, bruno, davi, carla} {
		usr, err := core.Store.Users().UpsertFromClaims(ctx, uuid.MustParse(u.ID), identity.Claims{Issuer: cfg.OIDCIssuer, Subject: u.ID,
			Email: u.Email, EmailVerified: true, Name: u.Name}, core.Clock.Now())
		if err != nil {
			return fmt.Errorf("usuário %s: %w", u.Email, err)
		}
		users[u.Email] = usr
	}
	acme, err := s.tenant(ctx, users[ana.Email], "acme", "Acme Software (demonstração)", "ACME")
	if err != nil {
		return err
	}
	for _, m := range []struct {
		u    demoUser
		role tenancy.Role
	}{{bruno, tenancy.RoleFinance}, {davi, tenancy.RoleAuditor}} {
		if err := s.member(ctx, acme, users[ana.Email], users[m.u.Email], m.role); err != nil {
			return err
		}
	}
	beta, err := s.tenant(ctx, users[carla.Email], "beta", "Beta Consultoria (demonstração)", "BETA")
	if err != nil {
		return err
	}
	if err := s.provider(ctx, acme, users[ana.Email], "sim_acme_demonstracao_0001"); err != nil {
		return err
	}
	if err := s.provider(ctx, beta, users[carla.Email], "sim_beta_demonstracao_0001"); err != nil {
		return err
	}
	if err := s.betaCatalog(ctx, beta, users[carla.Email]); err != nil {
		return err
	}
	log.Info("seed concluído", slog.String("tenant_a", acme.String()), slog.String("tenant_b", beta.String()))
	return nil
}

type seeder struct {
	core   *app.Core
	log    *slog.Logger
	issuer string
}

func ref(u identity.User) tenancy.UserRef {
	return tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName}
}

func (s *seeder) tenant(ctx context.Context, owner identity.User, slug, name, prefix string) (uuid.UUID, error) {
	orgs, err := s.core.Tenancy.UserOrganizations(ctx, owner.ID)
	if err != nil {
		return uuid.Nil, err
	}
	for _, o := range orgs {
		if o.TenantSlug == slug {
			return o.TenantID, nil
		}
	}
	t, err := s.core.Tenancy.Onboard(ctx, ref(owner), tenancy.TenantInput{Name: name, Slug: slug, Timezone: "America/Sao_Paulo", InvoicePrefix: prefix})
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant %s: %w", slug, err)
	}
	s.log.Info("tenant criado", slog.String("slug", slug))
	return t.ID, nil
}

// member associa o usuário pelo fluxo real de convite e aceite.
func (s *seeder) member(ctx context.Context, tenantID uuid.UUID, owner, user identity.User, role tenancy.Role) error {
	if _, err := s.core.Tenancy.ResolveAccess(ctx, tenantID, user.ID); err == nil {
		return nil
	}
	_, token, err := s.core.Tenancy.Invite(ctx, tenantID, tenancy.Access{TenantID: tenantID, UserID: owner.ID, Role: tenancy.RoleOwner}, ref(owner), user.Email, role)
	if err != nil {
		return err
	}
	_, err = s.core.Tenancy.AcceptInvitation(ctx, ref(user), token)
	return err
}

func (s *seeder) provider(ctx context.Context, tenantID uuid.UUID, owner identity.User, apiKey string) error {
	accounts, err := s.core.Payments.Accounts(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if a.Provider == "simulator" {
			return nil
		}
	}
	secret, _ := tenancy.NewToken()
	var lastErr error
	for range 30 {
		_, lastErr = s.core.Payments.ConfigureAccount(ctx, tenantID, audit.User(owner.ID, owner.DisplayName),
			payments.AccountInput{Provider: "simulator", Environment: "local", APIKey: apiKey, WebhookSecret: secret + secret[:8], MakeDefault: true})
		if lastErr == nil {
			return nil
		}
		s.log.Warn("simulador indisponível; nova tentativa", slog.String("error", lastErr.Error()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return lastErr
}

func (s *seeder) betaCatalog(ctx context.Context, tenantID uuid.UUID, owner identity.User) error {
	actor := audit.User(owner.ID, owner.DisplayName)
	page, err := s.core.Customers.List(ctx, tenantID, customers.ListFilter{Limit: 1})
	if err != nil || len(page.Items) > 0 {
		return err
	}
	prod, err := s.core.Catalog.CreateProduct(ctx, tenantID, actor, catalog.ProductInput{Name: "Consultoria em processos (demonstração)"})
	if err != nil {
		return err
	}
	flat := int64(250000)
	uid := owner.ID
	plan, err := s.core.Catalog.CreatePlan(ctx, tenantID, actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "consultoria-mensal",
		Name: "Consultoria mensal (demonstração)", Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.Flat, FlatAmount: &flat})
	if err != nil {
		return err
	}
	dv, err := brdoc.CNPJCheckDigits("114447770001")
	if err != nil {
		return err
	}
	cust, err := s.core.Customers.Create(ctx, tenantID, actor, customers.Input{Kind: customers.Company, Name: "Cliente Beta (demonstração)",
		DocumentNumber: "114447770001" + dv, Email: "financeiro@cliente-beta.test", ExternalID: "beta-demo-001"})
	switch {
	case apperr.IsKind(err, apperr.KindConflict):
		s.log.Info("cliente de demonstração já existe")
		return nil
	case err != nil:
		return err
	}
	_, err = s.core.Subscriptions.Create(ctx, tenantID, actor, subscriptions.CreateInput{CustomerID: cust.ID, PlanID: plan.ID, Quantity: 1})
	return err
}
