package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// loadIssuer é o emissor fictício dos usuários sintéticos (nenhum deles faz
// login: a carga usa credenciais de API).
const loadIssuer = "https://carga.recorvia.invalid/realms/recorvia"

// scopes são os escopos das credenciais de API dos cenários.
var scopes = []string{"customers.read", "customers.write", "subscriptions.read", "subscriptions.write", "usage.read", "usage.write",
	"invoices.read", "payments.read", "payments.record_manual", "dashboard.read", "catalog.read", "operations.read"}

// orgSizes distribui os clientes das organizações: 10% grandes, 30% médias e
// 60% pequenas (o perfil típico de uma base B2B), multiplicado por scale.
func orgSizes(n int, scale float64) []int {
	out := make([]int, n)
	for i := range n {
		base := 100
		switch {
		case i < max(1, n/10):
			base = 1500
		case i < max(1, n/10)+max(1, 3*n/10):
			base = 400
		}
		out[i] = max(1, int(float64(base)*scale+0.5))
	}
	return out
}

// runPrepare cria as organizações pelos serviços da aplicação, como o seed:
// usuário proprietário, organização, plano Escala da plataforma (contratos
// sem limite e acesso à API), catálogo (três planos de preço fixo e um com
// consumo medido) e uma credencial de API. O provedor de pagamentos é
// configurado depois do histórico (subcomando provedor).
func runPrepare(ctx context.Context, log *slog.Logger, e env, args []string) error {
	fs := newFlags("preparar")
	n := fs.Int("orgs", 20, "número de organizações")
	scale := fs.Float64("escala", 1, "multiplicador do número de clientes por organização")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *n < 1 || *n > 500 {
		return fmt.Errorf("-orgs deve estar entre 1 e 500")
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	core, err := app.Open(ctx, cfg, log, "recorvia-carga")
	if err != nil {
		return err
	}
	defer core.Close()
	creds := apicredentials.NewService(core.Store.APICredentials(), nil, core.Clock, apicredentials.DefaultPolicy())
	st := &state{CreatedAt: time.Now().UTC()}
	for i, size := range orgSizes(*n, *scale) {
		o, err := prepareOrg(ctx, core, creds, i+1, size)
		if err != nil {
			return fmt.Errorf("organização %d: %w", i+1, err)
		}
		st.Orgs = append(st.Orgs, o)
		log.Info("organização preparada", slog.String("slug", o.Slug), slog.Int("clientes", size))
	}
	return writeState(e.StateFile, st)
}

func prepareOrg(ctx context.Context, core *app.Core, creds *apicredentials.Service, i, size int) (org, error) {
	slug := fmt.Sprintf("carga-%03d", i)
	uid := uuid.New()
	owner, err := core.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: loadIssuer, Subject: uid.String(),
		Email: fmt.Sprintf("proprietario@%s.carga.test", slug), EmailVerified: true, Name: fmt.Sprintf("Proprietário %s (carga)", slug)},
		core.Clock.Now())
	if err != nil {
		return org{}, err
	}
	ref := tenancy.UserRef{ID: owner.ID, Email: owner.Email, DisplayName: owner.DisplayName}
	t, err := core.Tenancy.Onboard(ctx, ref, tenancy.TenantInput{Name: fmt.Sprintf("Organização %03d (carga)", i), Slug: slug,
		Timezone: "America/Sao_Paulo", InvoicePrefix: fmt.Sprintf("C%03d", i)})
	if err != nil {
		return org{}, err
	}
	actor := audit.User(owner.ID, owner.DisplayName)
	ov, err := core.Platform.Overview(ctx, t.ID)
	if err != nil {
		return org{}, err
	}
	if _, err := core.Platform.ChangePlan(ctx, t.ID, actor, "escala", ov.Subscription.Version); err != nil {
		return org{}, fmt.Errorf("plano da plataforma: %w", err)
	}
	o := org{Slug: slug, TenantID: t.ID.String(), Customers: size, MetricCode: "chamadas-api", PaysimKey: "sim_" + slug + "_" + randomHex(8)}
	prod, err := core.Catalog.CreateProduct(ctx, t.ID, actor, catalog.ProductInput{Name: "Plataforma de gestão (carga)"})
	if err != nil {
		return org{}, err
	}
	for _, p := range []struct {
		code, name string
		amount     int64
	}{{"basico-mensal", "Básico mensal", 9900}, {"padrao-mensal", "Padrão mensal", 29900}, {"avancado-mensal", "Avançado mensal", 89900}} {
		amount := p.amount
		plan, err := core.Catalog.CreatePlan(ctx, t.ID, actor, &owner.ID, catalog.PlanInput{ProductID: prod.ID, Code: p.code, Name: p.name,
			Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.Flat, FlatAmount: &amount})
		if err != nil {
			return org{}, err
		}
		o.FlatPlans = append(o.FlatPlans, plan.ID.String())
	}
	metric, err := core.Catalog.CreateMetric(ctx, t.ID, actor, catalog.MetricInput{Code: o.MetricCode, Name: "Chamadas de API", Unit: "chamada",
		Aggregation: catalog.AggSum, MaxQuantityPerEvent: "100000"})
	if err != nil {
		return org{}, err
	}
	flat := int64(19900)
	usage, err := core.Catalog.CreatePlan(ctx, t.ID, actor, &owner.ID, catalog.PlanInput{ProductID: prod.ID, Code: "consumo-mensal",
		Name: "Consumo mensal", Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.FlatPlusOverage, FlatAmount: &flat,
		UnitAmount: "0.05", MetricID: &metric.ID, IncludedQuantity: "1000"})
	if err != nil {
		return org{}, err
	}
	o.UsagePlan = usage.ID.String()
	issued, err := creds.Create(ctx, t.ID, apicredentials.Grantor{UserID: owner.ID, Role: tenancy.RoleOwner, Actor: actor},
		apicredentials.CreateInput{Name: "Teste de carga", Scopes: scopes})
	if err != nil {
		return org{}, fmt.Errorf("credencial de API: %w", err)
	}
	o.Token = issued.Token
	return o, nil
}

// runProvider configura o simulador como provedor padrão de cada organização
// (como o seed). As faturas emitidas a partir daqui geram cobranças no
// simulador; o endereço e o segredo dos webhooks ficam no estado para o
// cenário de webhooks.
func runProvider(ctx context.Context, log *slog.Logger, e env, _ []string) error {
	st, err := readState(e.StateFile)
	if err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	core, err := app.Open(ctx, cfg, log, "recorvia-carga")
	if err != nil {
		return err
	}
	defer core.Close()
	for i := range st.Orgs {
		o := &st.Orgs[i]
		if o.WebhookURL != "" {
			continue
		}
		tid := uuid.MustParse(o.TenantID)
		secret := randomHex(24)
		acc, err := core.Payments.ConfigureAccount(ctx, tid, audit.System("carga"), payments.AccountInput{Provider: "simulator",
			Environment: "local", APIKey: o.PaysimKey, WebhookSecret: secret, MakeDefault: true})
		if err != nil {
			return fmt.Errorf("%s: provedor: %w", o.Slug, err)
		}
		o.WebhookURL, o.WebhookSecret = webhookPath(acc.EndpointURL), secret
		log.Info("provedor configurado", slog.String("slug", o.Slug))
	}
	return writeState(e.StateFile, st)
}

// webhookPath reduz o endereço registrado no simulador (rede interna do
// compose) ao caminho, para que o cenário de webhooks o chame pela porta
// publicada da API.
func webhookPath(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Path == "" {
		return endpoint
	}
	return u.Path
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(hex.EncodeToString(b))
}
