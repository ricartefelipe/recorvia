package subscriptions

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

type historyWrite struct {
	subscriptionID uuid.UUID
	actor          audit.Actor
	events         []HistoryEvent
}

// fakeStore implementa Store e Tx em memória. Cada InTenant é atômico: se a
// função falhar, as gravações feitas na transação são descartadas.
type fakeStore struct {
	// onInsert, quando definido, decide se outra transação já confirmou a
	// assinatura com a mesma chave (true: o INSERT não grava nada).
	onInsert     func(Subscription) bool
	loc          *time.Location
	customers    map[uuid.UUID]customers.Customer
	plans        map[uuid.UUID]catalog.Plan
	prices       map[uuid.UUID]catalog.PriceVersion
	platformPlan platformbilling.Plan
	usage        platformbilling.Usage

	subs      map[uuid.UUID]Subscription
	updates   int
	histories []historyWrite
	history   map[uuid.UUID][]HistoryEntry
	audits    []audit.Entry
	events    []outbox.Event

	errs       map[string]error
	tenants    []uuid.UUID
	listFilter ListFilter
	forUpdate  []bool

	termsCanceled []uuid.UUID
	// pendingItems indica assinaturas com itens pendentes ainda não faturados.
	pendingItems  map[uuid.UUID]bool
	pendingChecks int
}

func (s *fakeStore) CancelPendingTerms(_ context.Context, id uuid.UUID, _ time.Time) error {
	if err := s.errs["CancelPendingTerms"]; err != nil {
		return err
	}
	s.termsCanceled = append(s.termsCanceled, id)
	return nil
}

func (s *fakeStore) HasOpenPendingItems(_ context.Context, id uuid.UUID) (bool, error) {
	s.pendingChecks++
	if err := s.errs["HasOpenPendingItems"]; err != nil {
		return false, err
	}
	return s.pendingItems[id], nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{loc: saoPaulo, customers: map[uuid.UUID]customers.Customer{}, plans: map[uuid.UUID]catalog.Plan{},
		prices: map[uuid.UUID]catalog.PriceVersion{}, subs: map[uuid.UUID]Subscription{}, history: map[uuid.UUID][]HistoryEntry{},
		errs: map[string]error{}, pendingItems: map[uuid.UUID]bool{}}
}

func (s *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	s.tenants = append(s.tenants, tenantID)
	subs, updates := maps.Clone(s.subs), s.updates
	nh, na, ne := len(s.histories), len(s.audits), len(s.events)
	if err := fn(s); err != nil {
		s.subs, s.updates = subs, updates
		s.histories, s.audits, s.events = s.histories[:nh], s.audits[:na], s.events[:ne]
		return err
	}
	return nil
}

func (s *fakeStore) fail(method string) error { return s.errs[method] }

func (s *fakeStore) TenantLocation(context.Context) (*time.Location, error) {
	return s.loc, s.fail("TenantLocation")
}

func (s *fakeStore) Customer(_ context.Context, id uuid.UUID) (customers.Customer, error) {
	if err := s.fail("Customer"); err != nil {
		return customers.Customer{}, err
	}
	c, ok := s.customers[id]
	if !ok {
		return customers.Customer{}, apperr.NotFound("customer_not_found", "Cliente não encontrado.")
	}
	return c, nil
}

func (s *fakeStore) Plan(_ context.Context, id uuid.UUID) (catalog.Plan, error) {
	if err := s.fail("Plan"); err != nil {
		return catalog.Plan{}, err
	}
	p, ok := s.plans[id]
	if !ok {
		return catalog.Plan{}, apperr.NotFound("plan_not_found", "Plano não encontrado.")
	}
	return p, nil
}

func (s *fakeStore) PriceVersion(_ context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	if err := s.fail("PriceVersion"); err != nil {
		return catalog.PriceVersion{}, err
	}
	p, ok := s.prices[id]
	if !ok {
		return catalog.PriceVersion{}, apperr.NotFound("price_not_found", "Preço não encontrado.")
	}
	return p, nil
}

func (s *fakeStore) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	return s.platformPlan, s.fail("PlatformPlan")
}

func (s *fakeStore) PlatformUsage(context.Context) (platformbilling.Usage, error) {
	return s.usage, s.fail("PlatformUsage")
}

func (s *fakeStore) Insert(_ context.Context, sub Subscription) (bool, error) {
	if err := s.fail("Insert"); err != nil {
		return false, err
	}
	if s.onInsert != nil {
		// Simula outra transação que confirmou a mesma chave antes deste
		// INSERT ... ON CONFLICT DO NOTHING.
		if s.onInsert(sub) {
			return false, nil
		}
	}
	sub.pending = nil
	s.subs[sub.ID] = sub
	return true, nil
}

func (s *fakeStore) ByRequestKey(_ context.Context, key string) (Subscription, error) {
	if err := s.fail("ByRequestKey"); err != nil {
		return Subscription{}, err
	}
	for _, sub := range s.subs {
		if sub.RequestKey == key {
			return sub, nil
		}
	}
	return Subscription{}, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
}

func (s *fakeStore) Get(_ context.Context, id uuid.UUID, forUpdate bool) (Subscription, error) {
	s.forUpdate = append(s.forUpdate, forUpdate)
	if err := s.fail("Get"); err != nil {
		return Subscription{}, err
	}
	sub, ok := s.subs[id]
	if !ok {
		return Subscription{}, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
	}
	return sub, nil
}

func (s *fakeStore) Update(_ context.Context, sub Subscription) error {
	if err := s.fail("Update"); err != nil {
		return err
	}
	cur := s.subs[sub.ID]
	if cur.Version != sub.Version {
		return apperr.PreconditionFailed("version_mismatch", "versão divergente na gravação")
	}
	sub.Version++
	sub.pending = nil
	s.subs[sub.ID] = sub
	s.updates++
	return nil
}

func (s *fakeStore) List(_ context.Context, f ListFilter) (Page, error) {
	s.listFilter = f
	if err := s.fail("List"); err != nil {
		return Page{}, err
	}
	var p Page
	for _, sub := range s.subs {
		if f.Status == "" || sub.Status == f.Status {
			p.Items = append(p.Items, sub)
		}
	}
	return p, nil
}

func (s *fakeStore) InsertHistory(_ context.Context, id uuid.UUID, actor audit.Actor, ev []HistoryEvent) error {
	if err := s.fail("InsertHistory"); err != nil {
		return err
	}
	s.histories = append(s.histories, historyWrite{id, actor, ev})
	return nil
}

func (s *fakeStore) History(_ context.Context, id uuid.UUID) ([]HistoryEntry, error) {
	return s.history[id], s.fail("History")
}

func (s *fakeStore) Audit(_ context.Context, e audit.Entry) error {
	if err := s.fail("Audit"); err != nil {
		return err
	}
	s.audits = append(s.audits, e)
	return nil
}

func (s *fakeStore) Emit(_ context.Context, e outbox.Event) error {
	if err := s.fail("Emit"); err != nil {
		return err
	}
	s.events = append(s.events, e)
	return nil
}

type serviceFixture struct {
	store    *fakeStore
	svc      *Service
	clock    *clock.Manual
	tenantID uuid.UUID
	actor    audit.Actor
	customer customers.Customer
	plan     catalog.Plan
	price    catalog.PriceVersion
}

var fixtureNow = local(2026, time.November, 16, 14, 30)

func newServiceFixture(t *testing.T, model catalog.PricingModel) *serviceFixture {
	t.Helper()
	st := newFakeStore()
	f := &serviceFixture{store: st, clock: clock.NewManual(fixtureNow), tenantID: uuid.New(), actor: audit.User(uuid.New(), "Ana Souza")}
	f.customer = customers.Customer{ID: uuid.New(), TenantID: f.tenantID, Name: "Padaria Central", Status: customers.Active}
	f.plan = fixturePlan(calendar.Monthly)
	f.price = fixturePrice(f.plan, model)
	st.customers[f.customer.ID] = f.customer
	st.plans[f.plan.ID] = f.plan
	st.prices[f.price.ID] = f.price
	st.platformPlan = platformbilling.Plan{Code: "growth", Features: []platformbilling.Feature{platformbilling.FeatureUsageBilling}, Active: true}
	f.svc = NewService(st, f.clock)
	return f
}

func (f *serviceFixture) create(t *testing.T) Subscription {
	t.Helper()
	sub, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.store.histories, f.store.audits, f.store.events = nil, nil, nil
	return sub
}

func (f *serviceFixture) assertNothingWritten(t *testing.T) {
	t.Helper()
	if len(f.store.subs) != 0 || len(f.store.histories) != 0 || len(f.store.audits) != 0 || len(f.store.events) != 0 {
		t.Fatalf("nada deveria ser gravado: subs=%d hist=%d audit=%d eventos=%d",
			len(f.store.subs), len(f.store.histories), len(f.store.audits), len(f.store.events))
	}
}

func intPtr(v int) *int { return &v }

func TestServiceCreate(t *testing.T) {
	f := newServiceFixture(t, catalog.Flat)
	sub, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID,
		PaymentTermsDays: intPtr(10)})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != Active || sub.Quantity != 1 || sub.PriceVersionID != f.price.ID || sub.TenantID != f.tenantID {
		t.Fatalf("assinatura criada: %+v", sub)
	}
	if !sub.StartedAt.Equal(fixtureNow) || *sub.PaymentTermsDays != 10 {
		t.Fatalf("início deve ser o instante atual: %v", sub.StartedAt)
	}
	if stored, ok := f.store.subs[sub.ID]; !ok || stored.Status != Active {
		t.Fatal("assinatura não persistida")
	}
	if f.store.tenants[0] != f.tenantID {
		t.Fatal("transação deve ser aberta no tenant informado")
	}
	if len(f.store.histories) != 1 || f.store.histories[0].actor != f.actor || eventNames(f.store.histories[0].events)[0] != "created" {
		t.Fatalf("histórico: %+v", f.store.histories)
	}
	if len(f.store.events) != 1 {
		t.Fatalf("eventos: %+v", f.store.events)
	}
	ev := f.store.events[0]
	payload := ev.Payload.(map[string]any)
	if ev.Type != outbox.EventSubscriptionCreated || ev.AggregateID != sub.ID || ev.TenantID != f.tenantID ||
		payload["customer_id"] != f.customer.ID || payload["status"] != Active {
		t.Fatalf("evento de criação: %+v", ev)
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	a := f.store.audits[0]
	if a.Action != "subscription.created" || a.ResourceID != sub.ID.String() || a.Actor != f.actor ||
		a.Details["plan_id"] != f.plan.ID || a.Details["price_version"] != 1 || a.Details["quantity"] != 1 {
		t.Fatalf("auditoria de criação: %+v", a)
	}
	if len(sub.PopEvents()) != 0 {
		t.Fatal("eventos de histórico devem ter sido consumidos na gravação")
	}
}

func TestServiceCreateTrialAndStartDate(t *testing.T) {
	t.Run("trial do plano", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		f.plan.TrialDays = 7
		f.store.plans[f.plan.ID] = f.plan
		sub := f.create(t)
		if sub.Status != Trialing || localDate(*sub.TrialEndsAt) != "2026-11-23" {
			t.Fatalf("trial do plano: %s %v", sub.Status, sub.TrialEndsAt)
		}
	})
	t.Run("trial informado substitui o do plano", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		f.plan.TrialDays = 7
		f.store.plans[f.plan.ID] = f.plan
		sub, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, TrialDays: intPtr(0)})
		if err != nil || sub.Status != Active || sub.TrialEndsAt != nil {
			t.Fatalf("sem trial: %v %+v", err, sub)
		}
	})
	t.Run("início retroativo", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		d := calendar.Date{Year: 2026, Month: time.September, Day: 1}
		sub, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, StartDate: &d})
		if err != nil {
			t.Fatal(err)
		}
		if !sub.StartedAt.Equal(d.StartIn(saoPaulo)) || sub.Anchor != d {
			t.Fatalf("início retroativo à meia-noite local: %v %v", sub.StartedAt, sub.Anchor)
		}
	})
	t.Run("início no futuro", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		d := calendar.Date{Year: 2026, Month: time.November, Day: 17}
		_, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, StartDate: &d})
		wantKind(t, err, apperr.KindInvalid, "invalid_subscription")
		f.assertNothingWritten(t)
	})
	t.Run("quantidade em plano por unidade", func(t *testing.T) {
		f := newServiceFixture(t, catalog.PerUnit)
		sub, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, Quantity: 12})
		if err != nil || sub.Quantity != 12 {
			t.Fatalf("quantidade: %v %d", err, sub.Quantity)
		}
		if f.store.audits[0].Details["quantity"] != 12 {
			t.Fatalf("auditoria deve registrar a quantidade: %+v", f.store.audits[0].Details)
		}
	})
}

func TestServiceCreateBusinessRules(t *testing.T) {
	cases := []struct {
		name  string
		model catalog.PricingModel
		setup func(*serviceFixture)
		in    func(*serviceFixture) CreateInput
		kind  apperr.Kind
		code  string
	}{
		{"cliente arquivado", catalog.Flat, func(f *serviceFixture) {
			c := f.customer
			c.Status = customers.Archived
			f.store.customers[c.ID] = c
		}, nil, apperr.KindConflict, "customer_archived"},
		{"plano sem preço vigente", catalog.Flat, func(f *serviceFixture) {
			p := f.plan
			p.CurrentPriceVersionID = nil
			f.store.plans[p.ID] = p
		}, nil, apperr.KindConflict, "plan_without_price"},
		{"limite de contratos do plano da plataforma", catalog.Flat, func(f *serviceFixture) {
			f.store.platformPlan.MaxActiveSubscriptions = intPtr(50)
			f.store.usage.ActiveSubscriptions = 50
		}, nil, apperr.KindLimitExceeded, "platform_subscription_limit"},
		{"plano com consumo sem o recurso usage_billing", catalog.Usage, func(f *serviceFixture) {
			f.store.platformPlan.Features = nil
		}, nil, apperr.KindForbidden, "platform_feature_unavailable"},
		{"plano fixo com excedente sem o recurso usage_billing", catalog.FlatPlusOverage, func(f *serviceFixture) {
			f.store.platformPlan.Features = []platformbilling.Feature{platformbilling.FeatureRecurringBilling}
		}, nil, apperr.KindForbidden, "platform_feature_unavailable"},
		{"cliente inexistente", catalog.Flat, nil, func(f *serviceFixture) CreateInput {
			return CreateInput{CustomerID: uuid.New(), PlanID: f.plan.ID}
		}, apperr.KindNotFound, "customer_not_found"},
		{"quantidade inválida para plano fixo", catalog.Flat, nil, func(f *serviceFixture) CreateInput {
			return CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, Quantity: 3}
		}, apperr.KindInvalid, "invalid_subscription"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t, c.model)
			if c.setup != nil {
				c.setup(f)
			}
			in := CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID}
			if c.in != nil {
				in = c.in(f)
			}
			_, err := f.svc.Create(context.Background(), f.tenantID, f.actor, in)
			wantKind(t, err, c.kind, c.code)
			f.assertNothingWritten(t)
		})
	}
}

func TestServiceCreateAllowsUsageWithFeatureAndBelowLimit(t *testing.T) {
	f := newServiceFixture(t, catalog.Usage)
	f.store.platformPlan.MaxActiveSubscriptions = intPtr(50)
	f.store.usage.ActiveSubscriptions = 49
	if _, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID}); err != nil {
		t.Fatalf("contrato 50 de 50 deve ser aceito: %v", err)
	}
}

func TestServiceCreatePropagatesStoreFailures(t *testing.T) {
	for _, method := range []string{"TenantLocation", "Customer", "Plan", "PriceVersion", "PlatformPlan", "PlatformUsage",
		"Insert", "InsertHistory", "Emit", "Audit"} {
		t.Run(method, func(t *testing.T) {
			f := newServiceFixture(t, catalog.Flat)
			boom := errors.New("falha em " + method)
			f.store.errs[method] = boom
			_, err := f.svc.Create(context.Background(), f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID})
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			f.assertNothingWritten(t)
		})
	}
}

// A chave de negócio do pedido (derivada do Idempotency-Key) torna a criação
// repetível mesmo depois de um processamento interrompido: a repetição
// devolve a assinatura já criada, sem nova assinatura, histórico, evento ou
// auditoria. A mesma chave para outro cliente ou plano é recusada.
func TestServiceCreateWithRequestKeyIsRepeatable(t *testing.T) {
	f := newServiceFixture(t, catalog.Flat)
	ctx := context.Background()
	in := CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, RequestKey: "chave-de-negocio-1"}
	first, err := f.svc.Create(ctx, f.tenantID, f.actor, in)
	if err != nil || first.RequestKey != in.RequestKey {
		t.Fatalf("criação: %+v %v", first, err)
	}
	f.store.histories, f.store.audits, f.store.events = nil, nil, nil
	again, err := f.svc.Create(ctx, f.tenantID, f.actor, in)
	if err != nil || again.ID != first.ID || len(f.store.subs) != 1 {
		t.Fatalf("repetição devolve a assinatura criada: %+v %v (%d assinaturas)", again, err, len(f.store.subs))
	}
	if len(f.store.histories) != 0 || len(f.store.audits) != 0 || len(f.store.events) != 0 {
		t.Fatal("repetição não grava histórico, auditoria nem evento")
	}

	other := customers.Customer{ID: uuid.New(), TenantID: f.tenantID, Name: "Mercado Bom Preço", Status: customers.Active}
	f.store.customers[other.ID] = other
	_, err = f.svc.Create(ctx, f.tenantID, f.actor, CreateInput{CustomerID: other.ID, PlanID: f.plan.ID, RequestKey: in.RequestKey})
	if e, ok := apperr.As(err); !ok || e.Code != "request_key_reused" || e.Kind != apperr.KindConflict || len(f.store.subs) != 1 {
		t.Fatalf("chave reutilizada para outro cliente: %v", err)
	}
	if _, err := f.svc.Create(ctx, f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, RequestKey: "outra-chave-2"}); err != nil ||
		len(f.store.subs) != 2 {
		t.Fatalf("outra chave cria outra assinatura: %v", err)
	}

	// Mesma chave com o mesmo cliente e plano, mas outro conteúdo de negócio
	// (por exemplo, depois da validade do registro HTTP), é recusada.
	for name, changed := range map[string]CreateInput{
		"quantidade":   {CustomerID: f.customer.ID, PlanID: f.plan.ID, Quantity: 2, RequestKey: in.RequestKey},
		"avaliação":    {CustomerID: f.customer.ID, PlanID: f.plan.ID, TrialDays: intPtr(7), RequestKey: in.RequestKey},
		"prazo":        {CustomerID: f.customer.ID, PlanID: f.plan.ID, PaymentTermsDays: intPtr(0), RequestKey: in.RequestKey},
		"data inicial": {CustomerID: f.customer.ID, PlanID: f.plan.ID, StartDate: &calendar.Date{Year: 2026, Month: 11, Day: 1}, RequestKey: in.RequestKey},
	} {
		_, err := f.svc.Create(ctx, f.tenantID, f.actor, changed)
		if e, ok := apperr.As(err); !ok || e.Code != "request_key_reused" {
			t.Fatalf("chave reutilizada com outra %s: %v", name, err)
		}
	}
	// Quantidade omitida e quantidade 1 são o mesmo pedido.
	if got, err := f.svc.Create(ctx, f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, Quantity: 1,
		RequestKey: in.RequestKey}); err != nil || got.ID != first.ID {
		t.Fatalf("quantidade 1 explícita: %v", err)
	}

	boom := errors.New("falha na consulta da chave")
	f.store.errs["ByRequestKey"] = boom
	if _, err := f.svc.Create(ctx, f.tenantID, f.actor, in); !errors.Is(err, boom) {
		t.Fatalf("falha na consulta da chave: %v", err)
	}
	// Sem chave, a consulta não é feita.
	if _, err := f.svc.Create(ctx, f.tenantID, f.actor, CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID}); err != nil {
		t.Fatalf("criação sem chave: %v", err)
	}
}

// Duas transações com a mesma chave de negócio: a segunda não grava (ON
// CONFLICT) e devolve a assinatura confirmada pela primeira, em vez de
// responder um conflito que ficaria registrado na Idempotency-Key.
func TestServiceCreateConcurrentSameRequestKeyReturnsConfirmed(t *testing.T) {
	f := newServiceFixture(t, catalog.Flat)
	ctx := context.Background()
	in := CreateInput{CustomerID: f.customer.ID, PlanID: f.plan.ID, RequestKey: "chave-concorrente"}
	var winner Subscription
	f.store.onInsert = func(sub Subscription) bool {
		winner = sub
		winner.ID = uuid.New()
		f.store.subs[winner.ID] = winner
		return true
	}
	got, err := f.svc.Create(ctx, f.tenantID, f.actor, in)
	if err != nil || got.ID != winner.ID || len(f.store.subs) != 1 {
		t.Fatalf("devolve a assinatura confirmada pela outra transação: %+v %v", got, err)
	}
	if len(f.store.histories) != 0 || len(f.store.events) != 0 || len(f.store.audits) != 0 {
		t.Fatal("a transação que perdeu não grava histórico, evento nem auditoria")
	}

	g := newServiceFixture(t, catalog.Flat)
	g.store.onInsert = func(sub Subscription) bool {
		other := sub
		other.ID, other.RequestFingerprint = uuid.New(), RequestFingerprint(CreateInput{CustomerID: g.customer.ID, PlanID: g.plan.ID, Quantity: 3})
		g.store.subs[other.ID] = other
		return true
	}
	_, err = g.svc.Create(ctx, g.tenantID, g.actor, CreateInput{CustomerID: g.customer.ID, PlanID: g.plan.ID, RequestKey: "chave-concorrente"})
	if e, ok := apperr.As(err); !ok || e.Code != "request_key_reused" {
		t.Fatalf("a outra transação gravou outro conteúdo: %v", err)
	}

	h := newServiceFixture(t, catalog.Flat)
	h.store.onInsert = func(Subscription) bool { return true }
	_, err = h.svc.Create(ctx, h.tenantID, h.actor, CreateInput{CustomerID: h.customer.ID, PlanID: h.plan.ID, RequestKey: "chave-concorrente"})
	if e, ok := apperr.As(err); !ok || e.Code != "subscription_in_progress" {
		t.Fatalf("sem a assinatura confirmada visível: %v", err)
	}
}

func TestServiceGetListHistory(t *testing.T) {
	f := newServiceFixture(t, catalog.Flat)
	sub := f.create(t)
	ctx := context.Background()

	got, err := f.svc.Get(ctx, f.tenantID, sub.ID)
	if err != nil || got.ID != sub.ID {
		t.Fatalf("Get: %v", err)
	}
	if f.store.forUpdate[len(f.store.forUpdate)-1] {
		t.Fatal("consulta não deve bloquear a linha")
	}
	if _, err := f.svc.Get(ctx, f.tenantID, uuid.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("inexistente: %v", err)
	}

	cust := f.customer.ID
	page, err := f.svc.List(ctx, f.tenantID, ListFilter{Status: Active, CustomerID: &cust, Limit: 20})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("List: %v %+v", err, page)
	}
	if f.store.listFilter.Limit != 20 || f.store.listFilter.CustomerID != &cust {
		t.Fatalf("filtro repassado: %+v", f.store.listFilter)
	}
	if _, err := f.svc.List(ctx, f.tenantID, ListFilter{}); err != nil {
		t.Fatalf("filtro vazio é válido: %v", err)
	}
	_, err = f.svc.List(ctx, f.tenantID, ListFilter{Status: "pausada"})
	wantKind(t, err, apperr.KindInvalid, "invalid_filter")

	f.store.history[sub.ID] = []HistoryEntry{{ID: uuid.New(), Event: "created", ToStatus: Active}}
	h, err := f.svc.History(ctx, f.tenantID, sub.ID)
	if err != nil || len(h) != 1 || h[0].Event != "created" {
		t.Fatalf("History: %v %+v", err, h)
	}
	if _, err := f.svc.History(ctx, f.tenantID, uuid.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("histórico de assinatura inexistente: %v", err)
	}
}

func TestServiceMutationsEmitHistoryOutboxAndAudit(t *testing.T) {
	cases := []struct {
		name   string
		prep   func(*testing.T, *serviceFixture, Subscription)
		run    func(*serviceFixture, Subscription) (Subscription, error)
		event  string
		action string
		status Status
		hist   string
	}{
		{"cancelar imediatamente", nil, func(f *serviceFixture, s Subscription) (Subscription, error) {
			return f.svc.Cancel(context.Background(), f.tenantID, f.actor, s.ID, CancelImmediately, "fim do contrato", s.Version)
		}, outbox.EventSubscriptionCanceled, "subscription.canceled", Canceled, "canceled"},
		{"cancelar ao fim do período", nil, func(f *serviceFixture, s Subscription) (Subscription, error) {
			return f.svc.Cancel(context.Background(), f.tenantID, f.actor, s.ID, CancelAtPeriodEnd, "", s.Version)
		}, outbox.EventSubscriptionUpdated, "subscription.canceled", Active, "cancel_scheduled"},
		{"retomar renovação", func(t *testing.T, f *serviceFixture, s Subscription) {
			if _, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, s.ID, CancelAtPeriodEnd, "", 0); err != nil {
				t.Fatal(err)
			}
		}, func(f *serviceFixture, s Subscription) (Subscription, error) {
			return f.svc.ResumeRenewal(context.Background(), f.tenantID, f.actor, s.ID, 0)
		}, outbox.EventSubscriptionUpdated, "subscription.renewal_resumed", Active, "cancel_unscheduled"},
		{"suspender", nil, func(f *serviceFixture, s Subscription) (Subscription, error) {
			return f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "inadimplência", s.Version)
		}, outbox.EventSubscriptionSuspended, "subscription.suspended", Suspended, "suspended"},
		{"reativar", func(t *testing.T, f *serviceFixture, s Subscription) {
			if _, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "", 0); err != nil {
				t.Fatal(err)
			}
		}, func(f *serviceFixture, s Subscription) (Subscription, error) {
			return f.svc.Reactivate(context.Background(), f.tenantID, f.actor, s.ID, 0)
		}, outbox.EventSubscriptionReactivate, "subscription.reactivated", Active, "reactivated"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t, catalog.Flat)
			sub := f.create(t)
			if c.prep != nil {
				c.prep(t, f, sub)
				f.store.histories, f.store.audits, f.store.events = nil, nil, nil
			}
			before := f.store.subs[sub.ID].Version
			f.clock.Advance(time.Hour)
			got, err := c.run(f, f.store.subs[sub.ID])
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != c.status || got.Version != before+1 || f.store.subs[sub.ID].Version != before+1 {
				t.Fatalf("situação=%s versão=%d (antes %d)", got.Status, got.Version, before)
			}
			if f.store.forUpdate[len(f.store.forUpdate)-1] != true {
				t.Fatal("mutação deve bloquear a linha")
			}
			if len(f.store.histories) != 1 || f.store.histories[0].actor != f.actor || eventNames(f.store.histories[0].events)[0] != c.hist {
				t.Fatalf("histórico: %+v", f.store.histories)
			}
			if len(f.store.events) != 1 || f.store.events[0].Type != c.event || f.store.events[0].AggregateID != sub.ID {
				t.Fatalf("evento: %+v", f.store.events)
			}
			payload := f.store.events[0].Payload.(map[string]any)
			if payload["status"] != c.status || payload["cancel_at_period_end"] != got.CancelAtPeriodEnd {
				t.Fatalf("payload: %+v", payload)
			}
			if !f.store.events[0].OccurredAt.Equal(fixtureNow.Add(time.Hour)) {
				t.Fatalf("evento deve usar o relógio do serviço: %v", f.store.events[0].OccurredAt)
			}
			if len(f.store.audits) != 1 || f.store.audits[0].Action != c.action || f.store.audits[0].Details["status"] != c.status {
				t.Fatalf("auditoria: %+v", f.store.audits)
			}
		})
	}
}

func TestServiceCancelUsesPriceUsageForFinalBilling(t *testing.T) {
	f := newServiceFixture(t, catalog.Usage)
	sub := f.create(t)
	stored := f.store.subs[sub.ID]
	if err := stored.ApplyStep(BillingStep{Kind: StepBill, Index: 0, Period: stored.CurrentPeriod}, fixtureNow); err != nil {
		t.Fatal(err)
	}
	stored.PopEvents()
	f.store.subs[sub.ID] = stored
	f.clock.Advance(48 * time.Hour)
	got, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelImmediately, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextBillingAt == nil || !got.NextBillingAt.Equal(fixtureNow.Add(48*time.Hour)) {
		t.Fatalf("plano com consumo agenda o fechamento final: %v", got.NextBillingAt)
	}
}

func TestServiceMutateVersionMismatch(t *testing.T) {
	f := newServiceFixture(t, catalog.Flat)
	sub := f.create(t)
	_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, sub.ID, "", sub.Version+1)
	wantKind(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if f.store.updates != 0 || f.store.subs[sub.ID].Status != Active || len(f.store.events) != 0 || len(f.store.audits) != 0 {
		t.Fatal("versão divergente não pode alterar nada")
	}
}

func TestServiceMutateRollsBackOnFailure(t *testing.T) {
	cases := []struct {
		name string
		errs map[string]error
		run  func(*serviceFixture, Subscription) error
		kind apperr.Kind
	}{
		{"assinatura inexistente", nil, func(f *serviceFixture, _ Subscription) error {
			_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, uuid.New(), "", 0)
			return err
		}, apperr.KindNotFound},
		{"regra de domínio", nil, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Reactivate(context.Background(), f.tenantID, f.actor, s.ID, 0)
			return err
		}, apperr.KindConflict},
		{"falha ao ler o preço no cancelamento", map[string]error{"PriceVersion": errors.New("preço indisponível")}, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, s.ID, CancelImmediately, "", 0)
			return err
		}, apperr.KindInternal},
		{"falha ao gravar", map[string]error{"Update": errors.New("deadlock")}, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "", 0)
			return err
		}, apperr.KindInternal},
		{"falha ao gravar histórico", map[string]error{"InsertHistory": errors.New("disco cheio")}, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "", 0)
			return err
		}, apperr.KindInternal},
		{"falha ao gravar evento", map[string]error{"Emit": errors.New("outbox indisponível")}, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "", 0)
			return err
		}, apperr.KindInternal},
		{"falha ao auditar", map[string]error{"Audit": errors.New("auditoria indisponível")}, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.Suspend(context.Background(), f.tenantID, f.actor, s.ID, "", 0)
			return err
		}, apperr.KindInternal},
		{"retomar sem cancelamento agendado", nil, func(f *serviceFixture, s Subscription) error {
			_, err := f.svc.ResumeRenewal(context.Background(), f.tenantID, f.actor, s.ID, 0)
			return err
		}, apperr.KindConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t, catalog.Flat)
			sub := f.create(t)
			for k, v := range c.errs {
				f.store.errs[k] = v
			}
			err := c.run(f, sub)
			if err == nil || apperr.KindOf(err) != c.kind {
				t.Fatalf("esperava erro %v, obtive %v", c.kind, err)
			}
			stored := f.store.subs[sub.ID]
			if stored.Status != Active || stored.Version != sub.Version || len(f.store.histories) != 0 || len(f.store.events) != 0 || len(f.store.audits) != 0 {
				t.Fatalf("falha deve desfazer a transação: %+v eventos=%d", stored, len(f.store.events))
			}
		})
	}
}
