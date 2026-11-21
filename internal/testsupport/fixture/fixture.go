//go:build integration

// Package fixture monta os serviços reais sobre PostgreSQL (papel da
// aplicação) para testes de integração, com relógio manual e dados mínimos.
package fixture

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// Env reúne serviços reais ligados ao banco do teste.
type Env struct {
	DB            *postgres.DB
	Keyring       *secrets.Keyring
	Clock         *clock.Manual
	Store         *pgstore.Store
	Tenancy       *tenancy.Service
	Platform      *platformbilling.Service
	Customers     *customers.Service
	Catalog       *catalog.Service
	Subscriptions *subscriptions.Service
	Engine        *billing.Engine
	Billing       *billing.Service
}

// New cria o ambiente. maxConns controla o tamanho do pool.
func New(t testing.TB, maxConns int32, now time.Time) *Env {
	t.Helper()
	return NewOn(t, pgtest.AppDB(t, maxConns), now)
}

// NewOn cria o ambiente sobre um pool já aberto (por exemplo, o de um banco
// criado por pgtest.Isolated).
func NewOn(t testing.TB, db *postgres.DB, now time.Time) *Env {
	t.Helper()
	kr, err := secrets.NewKeyring(map[string][]byte{"t1": bytes.Repeat([]byte{7}, 32)}, "t1")
	if err != nil {
		t.Fatal(err)
	}
	c := clock.NewManual(now)
	store := pgstore.New(db, kr, c, nil)
	engine := billing.NewEngine(store.Billing(), c)
	return &Env{DB: db, Keyring: kr, Clock: c, Store: store,
		Tenancy: tenancy.NewService(store.Tenancy(), c), Platform: platformbilling.NewService(store.Platform(), c),
		Customers: customers.NewService(store.Customers(), c), Catalog: catalog.NewService(store.Catalog(), c),
		Subscriptions: subscriptions.NewService(store.Subscriptions(), c), Engine: engine,
		Billing: billing.NewService(store.Billing(), engine, c)}
}

// Payments cria o serviço de pagamentos com o resolvedor informado.
func (e *Env) Payments(r payments.GatewayResolver, cfg payments.Config) *payments.Service {
	return payments.NewService(e.Store.Payments(), r, e.Clock, cfg)
}

// Tenant cria um usuário e uma organização ativa; retorna tenant e ator.
type Tenant struct {
	ID    uuid.UUID
	Owner identity.User
	Actor audit.Actor
}

// NewTenant cria uma organização com proprietário.
func (e *Env) NewTenant(t testing.TB, slugPrefix string) Tenant {
	t.Helper()
	ctx := context.Background()
	uid := ids.New()
	u, err := e.Store.Users().UpsertFromClaims(ctx, uid, identity.Claims{Issuer: "https://idp.test", Subject: uid.String(),
		Email: uid.String()[24:] + "@empresa.test", EmailVerified: true, Name: "Dono " + slugPrefix}, e.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	slug := fmt.Sprintf("%s-%s", slugPrefix, uid.String()[24:])
	tn, err := e.Tenancy.Onboard(ctx, tenancy.UserRef{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName},
		tenancy.TenantInput{Name: "Org " + slugPrefix, Slug: slug, Timezone: "America/Sao_Paulo"})
	if err != nil {
		t.Fatal(err)
	}
	return Tenant{ID: tn.ID, Owner: u, Actor: audit.User(u.ID, u.DisplayName)}
}

var docSeq = 10000000

// NewCustomer cria um cliente PJ com CNPJ estruturalmente válido.
func (e *Env) NewCustomer(t testing.TB, tn Tenant) customers.Customer {
	t.Helper()
	docSeq++
	base := fmt.Sprintf("%08d0001", docSeq)
	dv, err := brdoc.CNPJCheckDigits(base)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.Customers.Create(context.Background(), tn.ID, tn.Actor, customers.Input{Kind: customers.Company,
		Name: "Cliente " + base, DocumentNumber: base + dv, Email: "financeiro" + base + "@cliente.test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// NewFlatPlan cria produto e plano mensal de valor fixo.
func (e *Env) NewFlatPlan(t testing.TB, tn Tenant, amount int64) catalog.PlanDetails {
	t.Helper()
	ctx := context.Background()
	prod, err := e.Catalog.CreateProduct(ctx, tn.ID, tn.Actor, catalog.ProductInput{Name: "Produto de teste"})
	if err != nil {
		t.Fatal(err)
	}
	uid := tn.Owner.ID
	// O código usa um UUID aleatório: os 8 primeiros caracteres de um UUIDv7
	// vêm do relógio e se repetem entre planos criados no mesmo minuto.
	p, err := e.Catalog.CreatePlan(ctx, tn.ID, tn.Actor, &uid, catalog.PlanInput{ProductID: prod.ID, Code: "plano-" + uuid.NewString()[:8],
		Name: "Plano de teste", Interval: calendar.Monthly}, catalog.PriceInput{Model: catalog.Flat, FlatAmount: &amount})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// NewSubscription cria uma assinatura opcionalmente retroativa.
func (e *Env) NewSubscription(t testing.TB, tn Tenant, customerID, planID uuid.UUID, start *calendar.Date) subscriptions.Subscription {
	t.Helper()
	zero := 0
	s, err := e.Subscriptions.Create(context.Background(), tn.ID, tn.Actor, subscriptions.CreateInput{CustomerID: customerID, PlanID: planID,
		Quantity: 1, StartDate: start, TrialDays: &zero})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
