package catalog

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

type catEnv struct {
	store    *catalogStore
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	actor    audit.Actor
	userID   uuid.UUID
}

func newCatEnv() *catEnv {
	store := newCatalogStore()
	c := clock.NewManual(testNow)
	uid := uuid.New()
	return &catEnv{store: store, clock: c, svc: NewService(store, c), tenantID: uuid.New(), actor: audit.User(uid, "Ana"), userID: uid}
}

func (e *catEnv) st() *catalogState { return e.store.state(e.tenantID) }

func (e *catEnv) seedProduct(t *testing.T, status Status) Product {
	t.Helper()
	p, err := NewProduct(uuid.New(), e.tenantID, ProductInput{Name: "Plataforma"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	p.Status = status
	e.st().products[p.ID] = p
	return p
}

func (e *catEnv) seedMetric(status Status) Metric {
	m := Metric{ID: uuid.New(), TenantID: e.tenantID, Code: "api.requests", Aggregation: AggSum, Status: status}
	e.st().metrics[m.ID] = m
	return m
}

func (e *catEnv) createPlan(t *testing.T, price PriceInput) PlanDetails {
	t.Helper()
	prod := e.seedProduct(t, Active)
	d, err := e.svc.CreatePlan(context.Background(), e.tenantID, e.actor, &e.userID,
		PlanInput{ProductID: prod.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly, TrialDays: 7}, price)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	return d
}

func flatPrice(cents int64) PriceInput { return PriceInput{Model: Flat, FlatAmount: ptr(cents)} }

func (e *catEnv) lastAudit(t *testing.T) audit.Entry {
	t.Helper()
	a := e.st().audits
	if len(a) == 0 {
		t.Fatal("nenhuma entrada de auditoria")
	}
	return a[len(a)-1]
}

// --- Produtos ---

func TestCreateProduct(t *testing.T) {
	e := newCatEnv()
	p, err := e.svc.CreateProduct(context.Background(), e.tenantID, e.actor, ProductInput{Name: "Plataforma"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.st().products[p.ID]; !ok || p.TenantID != e.tenantID {
		t.Fatal("produto não persistido")
	}
	if a := e.lastAudit(t); a.Action != "catalog.product_created" || a.ResourceID != p.ID.String() || a.Actor != e.actor {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	_, err = e.svc.CreateProduct(context.Background(), e.tenantID, e.actor, ProductInput{Name: "P"})
	assertAppErr(t, err, apperr.KindInvalid, "invalid_product")
	if e.store.calls["InTenant"] != 1 {
		t.Fatal("produto inválido não deve abrir transação")
	}
	for _, op := range []string{"InsertProduct", "Audit"} {
		e := newCatEnv()
		e.store.fail[op] = errBoom
		if _, err := e.svc.CreateProduct(context.Background(), e.tenantID, e.actor, ProductInput{Name: "Plataforma"}); !errors.Is(err, errBoom) {
			t.Errorf("falha em %s deveria ser propagada: %v", op, err)
		}
		if len(e.st().products) != 0 {
			t.Errorf("falha em %s deveria desfazer o produto", op)
		}
	}
}

func TestUpdateProduct(t *testing.T) {
	e := newCatEnv()
	p := e.seedProduct(t, Active)
	e.clock.Advance(time.Hour)
	got, err := e.svc.UpdateProduct(context.Background(), e.tenantID, e.actor, p.ID,
		ProductInput{Name: " Plataforma 2 ", Description: "Nova"}, Archived, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Plataforma 2" || got.Description != "Nova" || got.Status != Archived || got.Version != 2 || !got.CreatedAt.Equal(testNow) {
		t.Fatalf("produto inesperado: %+v", got)
	}
	if stored := e.st().products[p.ID]; stored.Version != 2 || stored.Status != Archived {
		t.Fatalf("alteração não persistida: %+v", stored)
	}
	if a := e.lastAudit(t); a.Action != "catalog.product_updated" || a.Details["status"] != Archived || !a.OccurredAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
}

func TestUpdateProductRejected(t *testing.T) {
	cases := []struct {
		name    string
		in      ProductInput
		status  Status
		version int
		kind    apperr.Kind
		code    string
	}{
		{"versão divergente", ProductInput{Name: "Outro"}, Active, 2, apperr.KindPreconditionFailed, "version_mismatch"},
		{"dados inválidos", ProductInput{Name: "X"}, Active, 1, apperr.KindInvalid, "invalid_product"},
		{"situação desconhecida", ProductInput{Name: "Outro"}, "deleted", 1, apperr.KindInvalid, "invalid_status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCatEnv()
			p := e.seedProduct(t, Active)
			_, err := e.svc.UpdateProduct(context.Background(), e.tenantID, e.actor, p.ID, tc.in, tc.status, tc.version)
			assertAppErr(t, err, tc.kind, tc.code)
			if e.st().products[p.ID] != p {
				t.Fatal("produto não pode ser alterado")
			}
		})
	}
	t.Run("produto inexistente", func(t *testing.T) {
		e := newCatEnv()
		_, err := e.svc.UpdateProduct(context.Background(), e.tenantID, e.actor, uuid.New(), ProductInput{Name: "Outro"}, Active, 1)
		assertAppErr(t, err, apperr.KindNotFound, "product_not_found")
	})
	for _, op := range []string{"UpdateProduct", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newCatEnv()
			p := e.seedProduct(t, Active)
			e.store.fail[op] = errBoom
			if _, err := e.svc.UpdateProduct(context.Background(), e.tenantID, e.actor, p.ID, ProductInput{Name: "Outro"}, Active, 1); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.st().products[p.ID].Name != "Plataforma" {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

func TestProductsAndMetricsListing(t *testing.T) {
	e := newCatEnv()
	e.seedProduct(t, Active)
	e.seedProduct(t, Archived)
	e.seedMetric(Active)

	ps, err := e.svc.Products(context.Background(), e.tenantID, Archived)
	if err != nil || len(ps) != 1 || ps[0].Status != Archived || e.st().productFilter != Archived {
		t.Fatalf("Products = %+v, %v", ps, err)
	}
	ms, err := e.svc.Metrics(context.Background(), e.tenantID)
	if err != nil || len(ms) != 1 {
		t.Fatalf("Metrics = %+v, %v", ms, err)
	}
	e.store.fail["Products"], e.store.fail["Metrics"] = errBoom, errBoom
	if _, err := e.svc.Products(context.Background(), e.tenantID, ""); !errors.Is(err, errBoom) {
		t.Errorf("Products deveria propagar o erro: %v", err)
	}
	if _, err := e.svc.Metrics(context.Background(), e.tenantID); !errors.Is(err, errBoom) {
		t.Errorf("Metrics deveria propagar o erro: %v", err)
	}
}

// --- Métricas ---

func TestCreateMetric(t *testing.T) {
	e := newCatEnv()
	m, err := e.svc.CreateMetric(context.Background(), e.tenantID, e.actor, MetricInput{Code: "storage.gb", Name: "Armazenamento",
		Unit: "gb", Aggregation: AggLast, MaxQuantityPerEvent: "10000"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.st().metrics[m.ID]; !ok {
		t.Fatal("métrica não persistida")
	}
	if a := e.lastAudit(t); a.Action != "catalog.metric_created" || a.Details["code"] != "storage.gb" || a.Details["aggregation"] != AggLast {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	_, err = e.svc.CreateMetric(context.Background(), e.tenantID, e.actor, MetricInput{Code: "storage.gb", Name: "Armazenamento",
		Unit: "gb", Aggregation: "avg", MaxQuantityPerEvent: "1"})
	fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_metric")
	if !slices.Equal(fields, []string{"aggregation"}) {
		t.Fatalf("campos = %v", fields)
	}
	for _, op := range []string{"InsertMetric", "Audit"} {
		e := newCatEnv()
		e.store.fail[op] = errBoom
		if _, err := e.svc.CreateMetric(context.Background(), e.tenantID, e.actor, validMetricInput()); !errors.Is(err, errBoom) {
			t.Errorf("falha em %s deveria ser propagada: %v", op, err)
		}
		if len(e.st().metrics) != 0 {
			t.Errorf("falha em %s deveria desfazer a métrica", op)
		}
	}
}

// --- Planos ---

func TestCreatePlan(t *testing.T) {
	e := newCatEnv()
	prod := e.seedProduct(t, Active)
	metric := e.seedMetric(Active)
	price := PriceInput{Model: FlatPlusOverage, FlatAmount: ptr(int64(5000)), UnitAmount: "2", MetricID: &metric.ID, IncludedQuantity: "1000"}

	d, err := e.svc.CreatePlan(context.Background(), e.tenantID, e.actor, &e.userID,
		PlanInput{ProductID: prod.ID, Code: "pro", Name: "Pro", Interval: calendar.Yearly}, price)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Prices) != 1 {
		t.Fatalf("esperada uma versão de preço, obtidas %d", len(d.Prices))
	}
	pv := d.Prices[0]
	if pv.Version != 1 || pv.PlanID != d.ID || pv.TenantID != e.tenantID || pv.CreatedBy == nil || *pv.CreatedBy != e.userID ||
		pv.Currency != money.BRL || *pv.MetricID != metric.ID {
		t.Fatalf("versão de preço inesperada: %+v", pv)
	}
	if d.CurrentPriceVersionID == nil || *d.CurrentPriceVersionID != pv.ID {
		t.Fatal("a primeira versão deve ser a vigente")
	}
	st := e.st()
	if stored, ok := st.plans[d.ID]; !ok || *stored.CurrentPriceVersionID != pv.ID {
		t.Fatal("plano não persistido com a versão vigente")
	}
	if _, ok := st.prices[pv.ID]; !ok {
		t.Fatal("versão de preço não persistida")
	}
	a := e.lastAudit(t)
	if a.Action != "catalog.plan_created" || a.Details["code"] != "pro" || a.Details["pricing_model"] != FlatPlusOverage || a.Details["price_version"] != 1 {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
}

func TestCreatePlanRejected(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*testing.T, *catEnv) (PlanInput, PriceInput)
		kind   apperr.Kind
		code   string
		fields []string
	}{
		{"produto arquivado", func(t *testing.T, e *catEnv) (PlanInput, PriceInput) {
			p := e.seedProduct(t, Archived)
			return PlanInput{ProductID: p.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly}, flatPrice(100)
		}, apperr.KindConflict, "product_archived", nil},
		{"produto inexistente", func(*testing.T, *catEnv) (PlanInput, PriceInput) {
			return PlanInput{ProductID: uuid.New(), Code: "pro", Name: "Pro", Interval: calendar.Monthly}, flatPrice(100)
		}, apperr.KindNotFound, "product_not_found", nil},
		{"métrica inexistente", func(t *testing.T, e *catEnv) (PlanInput, PriceInput) {
			p := e.seedProduct(t, Active)
			id := uuid.New()
			return PlanInput{ProductID: p.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly},
				PriceInput{Model: Usage, UnitAmount: "1", MetricID: &id}
		}, apperr.KindInvalid, "invalid_price", []string{"metric_id"}},
		{"métrica arquivada", func(t *testing.T, e *catEnv) (PlanInput, PriceInput) {
			p := e.seedProduct(t, Active)
			m := e.seedMetric(Archived)
			return PlanInput{ProductID: p.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly},
				PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID}
		}, apperr.KindInvalid, "invalid_price", []string{"metric_id"}},
		{"preço inválido", func(t *testing.T, e *catEnv) (PlanInput, PriceInput) {
			p := e.seedProduct(t, Active)
			return PlanInput{ProductID: p.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly}, PriceInput{Model: Flat}
		}, apperr.KindInvalid, "invalid_price", []string{"flat_amount"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCatEnv()
			plan, price := tc.setup(t, e)
			_, err := e.svc.CreatePlan(context.Background(), e.tenantID, e.actor, nil, plan, price)
			fields := assertAppErr(t, err, tc.kind, tc.code)
			if tc.fields != nil && !slices.Equal(fields, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", fields, tc.fields)
			}
			if len(e.st().plans) != 0 || len(e.st().prices) != 0 {
				t.Fatal("nenhum plano deveria ser gravado")
			}
		})
	}
	t.Run("plano inválido não abre transação", func(t *testing.T) {
		e := newCatEnv()
		_, err := e.svc.CreatePlan(context.Background(), e.tenantID, e.actor, nil, PlanInput{Code: "X"}, flatPrice(1))
		assertAppErr(t, err, apperr.KindInvalid, "invalid_plan")
		if e.store.calls["InTenant"] != 0 {
			t.Fatal("validação deve ocorrer antes da transação")
		}
	})
}

func TestCreatePlanStoreErrors(t *testing.T) {
	for _, op := range []string{"Product", "Metric", "InsertPlan", "InsertPriceVersion", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newCatEnv()
			prod := e.seedProduct(t, Active)
			m := e.seedMetric(Active)
			e.store.fail[op] = errBoom
			_, err := e.svc.CreatePlan(context.Background(), e.tenantID, e.actor, nil,
				PlanInput{ProductID: prod.ID, Code: "pro", Name: "Pro", Interval: calendar.Monthly},
				PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID})
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if len(e.st().plans) != 0 || len(e.st().prices) != 0 {
				t.Fatal("criação deveria ser desfeita")
			}
		})
	}
}

func TestUpdatePlan(t *testing.T) {
	e := newCatEnv()
	d := e.createPlan(t, flatPrice(9990))
	before := e.st().plans[d.ID]

	got, err := e.svc.UpdatePlan(context.Background(), e.tenantID, e.actor, d.ID,
		UpdatePlanInput{Name: " Pro 2 ", Description: "Atualizado", TrialDays: 30, Status: Archived}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Pro 2" || got.Description != "Atualizado" || got.TrialDays != 30 || got.Status != Archived || got.Version != 2 {
		t.Fatalf("plano inesperado: %+v", got)
	}
	// Código, periodicidade, produto e preço vigente não mudam por esta operação.
	if got.Code != before.Code || got.Interval != before.Interval || got.ProductID != before.ProductID ||
		*got.CurrentPriceVersionID != *before.CurrentPriceVersionID {
		t.Fatalf("campos imutáveis alterados: %+v", got)
	}
	if a := e.lastAudit(t); a.Action != "catalog.plan_updated" || a.Details["status"] != Archived {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
}

func TestUpdatePlanRejected(t *testing.T) {
	cases := []struct {
		name    string
		in      UpdatePlanInput
		version int
		kind    apperr.Kind
		code    string
	}{
		{"versão divergente", UpdatePlanInput{Name: "Pro", Status: Active}, 5, apperr.KindPreconditionFailed, "version_mismatch"},
		{"nome inválido", UpdatePlanInput{Name: "P", Status: Active}, 1, apperr.KindInvalid, "invalid_plan"},
		{"trial inválido", UpdatePlanInput{Name: "Pro", TrialDays: 400, Status: Active}, 1, apperr.KindInvalid, "invalid_plan"},
		{"situação desconhecida", UpdatePlanInput{Name: "Pro", Status: "draft"}, 1, apperr.KindInvalid, "invalid_status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCatEnv()
			d := e.createPlan(t, flatPrice(9990))
			before := e.st().plans[d.ID]
			_, err := e.svc.UpdatePlan(context.Background(), e.tenantID, e.actor, d.ID, tc.in, tc.version)
			assertAppErr(t, err, tc.kind, tc.code)
			if after := e.st().plans[d.ID]; after.Version != before.Version || after.Name != before.Name {
				t.Fatal("plano não pode ser alterado")
			}
		})
	}
	t.Run("plano inexistente", func(t *testing.T) {
		e := newCatEnv()
		_, err := e.svc.UpdatePlan(context.Background(), e.tenantID, e.actor, uuid.New(), UpdatePlanInput{Name: "Pro", Status: Active}, 1)
		assertAppErr(t, err, apperr.KindNotFound, "plan_not_found")
	})
	for _, op := range []string{"UpdatePlan", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newCatEnv()
			d := e.createPlan(t, flatPrice(9990))
			e.store.fail[op] = errBoom
			if _, err := e.svc.UpdatePlan(context.Background(), e.tenantID, e.actor, d.ID, UpdatePlanInput{Name: "Outro", Status: Active}, 1); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.st().plans[d.ID].Name != "Pro" {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

// --- Versões de preço ---

func TestAddPriceVersion(t *testing.T) {
	e := newCatEnv()
	d := e.createPlan(t, flatPrice(9990))
	first := d.Prices[0]
	e.clock.Advance(24 * time.Hour)

	pv, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, &e.userID, d.ID, PriceInput{Model: PerUnit, UnitAmount: "4990.5"})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Version != 2 || pv.PlanID != d.ID || pv.Model != PerUnit || pv.Currency != money.BRL || !pv.CreatedAt.Equal(e.clock.Now()) {
		t.Fatalf("versão inesperada: %+v", pv)
	}
	plan := e.st().plans[d.ID]
	if plan.CurrentPriceVersionID == nil || *plan.CurrentPriceVersionID != pv.ID {
		t.Fatal("a nova versão deve passar a ser a vigente")
	}
	// A versão anterior permanece intacta.
	old := e.st().prices[first.ID]
	if old.Version != 1 || old.Model != Flat || old.FlatAmount.Amount() != 9990 || !old.CreatedAt.Equal(testNow) {
		t.Fatalf("versão anterior alterada: %+v", old)
	}
	if a := e.lastAudit(t); a.Action != "catalog.price_version_created" || a.Details["price_version"] != 2 || a.Details["pricing_model"] != PerUnit {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	details, err := e.svc.Plan(context.Background(), e.tenantID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Prices) != 2 || details.Prices[0].Version != 1 || details.Prices[1].Version != 2 {
		t.Fatalf("o plano deve listar todas as versões: %+v", details.Prices)
	}

	third, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, nil, d.ID, flatPrice(12990))
	if err != nil || third.Version != 3 {
		t.Fatalf("terceira versão = %+v, %v", third, err)
	}
}

func TestAddPriceVersionContinuesFromHighestVersion(t *testing.T) {
	e := newCatEnv()
	d := e.createPlan(t, flatPrice(9990))
	e.store.maxVersionOverride = ptr(7)
	pv, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, nil, d.ID, flatPrice(1))
	if err != nil || pv.Version != 8 {
		t.Fatalf("versão = %+v, %v; esperado 8", pv, err)
	}
}

func TestAddPriceVersionWithMetric(t *testing.T) {
	e := newCatEnv()
	d := e.createPlan(t, flatPrice(9990))
	m := e.seedMetric(Active)
	pv, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, nil, d.ID, PriceInput{Model: Usage, UnitAmount: "0.35", MetricID: &m.ID})
	if err != nil || pv.MetricID == nil || *pv.MetricID != m.ID {
		t.Fatalf("versão com métrica = %+v, %v", pv, err)
	}
}

func TestAddPriceVersionRejected(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *catEnv) (uuid.UUID, PriceInput)
		kind  apperr.Kind
		code  string
	}{
		{"plano arquivado", func(t *testing.T, e *catEnv) (uuid.UUID, PriceInput) {
			d := e.createPlan(t, flatPrice(1))
			p := e.st().plans[d.ID]
			p.Status = Archived
			e.st().plans[d.ID] = p
			return d.ID, flatPrice(2)
		}, apperr.KindConflict, "plan_archived"},
		{"plano inexistente", func(*testing.T, *catEnv) (uuid.UUID, PriceInput) { return uuid.New(), flatPrice(2) },
			apperr.KindNotFound, "plan_not_found"},
		{"métrica inexistente", func(t *testing.T, e *catEnv) (uuid.UUID, PriceInput) {
			d := e.createPlan(t, flatPrice(1))
			id := uuid.New()
			return d.ID, PriceInput{Model: Usage, UnitAmount: "1", MetricID: &id}
		}, apperr.KindInvalid, "invalid_price"},
		{"métrica arquivada", func(t *testing.T, e *catEnv) (uuid.UUID, PriceInput) {
			d := e.createPlan(t, flatPrice(1))
			m := e.seedMetric(Archived)
			return d.ID, PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID}
		}, apperr.KindInvalid, "invalid_price"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCatEnv()
			planID, in := tc.setup(t, e)
			before := len(e.st().prices)
			_, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, nil, planID, in)
			assertAppErr(t, err, tc.kind, tc.code)
			if len(e.st().prices) != before {
				t.Fatal("nenhuma versão deveria ser gravada")
			}
		})
	}
}

func TestAddPriceVersionStoreErrors(t *testing.T) {
	for _, op := range []string{"Plan", "Metric", "MaxPriceVersion", "InsertPriceVersion", "UpdatePlan", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newCatEnv()
			d := e.createPlan(t, flatPrice(1))
			m := e.seedMetric(Active)
			e.store.fail[op] = errBoom
			_, err := e.svc.AddPriceVersion(context.Background(), e.tenantID, e.actor, nil, d.ID, PriceInput{Model: Usage, UnitAmount: "1", MetricID: &m.ID})
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if len(e.st().prices) != 1 || *e.st().plans[d.ID].CurrentPriceVersionID != d.Prices[0].ID {
				t.Fatal("nova versão deveria ser desfeita e a vigente mantida")
			}
		})
	}
}

func TestPlanQueries(t *testing.T) {
	e := newCatEnv()
	d := e.createPlan(t, flatPrice(1))

	details, err := e.svc.Plan(context.Background(), e.tenantID, d.ID)
	if err != nil || details.ID != d.ID || len(details.Prices) != 1 {
		t.Fatalf("Plan = %+v, %v", details, err)
	}
	_, err = e.svc.Plan(context.Background(), e.tenantID, uuid.New())
	assertAppErr(t, err, apperr.KindNotFound, "plan_not_found")

	plans, err := e.svc.Plans(context.Background(), e.tenantID, Active, &d.ProductID)
	if err != nil || len(plans) != 1 || e.st().planFilter != Active || *e.st().planProduct != d.ProductID {
		t.Fatalf("Plans = %+v, %v", plans, err)
	}
	other := uuid.New()
	if plans, _ := e.svc.Plans(context.Background(), e.tenantID, "", &other); len(plans) != 0 {
		t.Fatalf("filtro por produto não aplicado: %+v", plans)
	}

	e.store.fail["PriceVersions"] = errBoom
	if _, err := e.svc.Plan(context.Background(), e.tenantID, d.ID); !errors.Is(err, errBoom) {
		t.Errorf("Plan deveria propagar o erro das versões: %v", err)
	}
	e.store.fail["Plans"] = errBoom
	if _, err := e.svc.Plans(context.Background(), e.tenantID, "", nil); !errors.Is(err, errBoom) {
		t.Errorf("Plans deveria propagar o erro: %v", err)
	}
}
