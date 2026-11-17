package pgstore

import (
	"context"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Catalog implementa catalog.Store.
type Catalog struct{ s *Store }

// Catalog retorna o adaptador do catálogo.
func (s *Store) Catalog() *Catalog { return &Catalog{s: s} }

type catalogTx struct{ base }

// InTenant abre a transação do tenant.
func (c *Catalog) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(catalog.Tx) error) error {
	return c.s.inTenant(ctx, tenantID, func(b base) error { return fn(catalogTx{b}) })
}

func productFromRow(r dbgen.Product) catalog.Product {
	return catalog.Product{ID: r.ID, TenantID: r.TenantID, Name: r.Name, Description: r.Description, Status: catalog.Status(r.Status),
		CreatedAt: r.CreatedAt, Version: int(r.Version)}
}

func (tx catalogTx) InsertProduct(ctx context.Context, p catalog.Product) error {
	return tx.q.InsertProduct(ctx, dbgen.InsertProductParams{ID: p.ID, TenantID: tx.tenantID, Name: p.Name, Description: p.Description,
		Status: string(p.Status), CreatedAt: p.CreatedAt})
}

func (tx catalogTx) Product(ctx context.Context, id uuid.UUID, forUpdate bool) (catalog.Product, error) {
	var r dbgen.Product
	var err error
	if forUpdate {
		r, err = tx.q.GetProductForUpdate(ctx, dbgen.GetProductForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetProduct(ctx, dbgen.GetProductParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return catalog.Product{}, notFound(err, "product_not_found", "Produto não encontrado.")
	}
	return productFromRow(r), nil
}

func (tx catalogTx) UpdateProduct(ctx context.Context, p catalog.Product) error {
	n, err := tx.q.UpdateProduct(ctx, dbgen.UpdateProductParams{TenantID: tx.tenantID, ID: p.ID, Name: p.Name, Description: p.Description,
		Status: string(p.Status), Version: int32(p.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "O produto foi alterado por outra pessoa.")
}

func (tx catalogTx) Products(ctx context.Context, status catalog.Status) ([]catalog.Product, error) {
	rows, err := tx.q.ListProducts(ctx, dbgen.ListProductsParams{TenantID: tx.tenantID, Status: strPtr(string(status))})
	out := make([]catalog.Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, productFromRow(r))
	}
	return out, err
}

func metricFromRow(r dbgen.BillableMetric) (catalog.Metric, error) {
	maxQty, err := ratFromNumeric(r.MaxQuantityPerEvent)
	if err != nil {
		return catalog.Metric{}, err
	}
	return catalog.Metric{ID: r.ID, TenantID: r.TenantID, Code: r.Code, Name: r.Name, Unit: r.Unit, Aggregation: catalog.Aggregation(r.Aggregation),
		MaxQuantityPerEvent: maxQty, LateGraceHours: int(r.LateGraceHours), Status: catalog.Status(r.Status), CreatedAt: r.CreatedAt}, nil
}

func (tx catalogTx) InsertMetric(ctx context.Context, m catalog.Metric) error {
	maxQty, err := numericFromRat(m.MaxQuantityPerEvent)
	if err != nil {
		return err
	}
	err = tx.q.InsertMetric(ctx, dbgen.InsertMetricParams{ID: m.ID, TenantID: tx.tenantID, Code: m.Code, Name: m.Name, Unit: m.Unit,
		Aggregation: string(m.Aggregation), MaxQuantityPerEvent: maxQty, LateGraceHours: int32(m.LateGraceHours), Status: string(m.Status),
		CreatedAt: m.CreatedAt})
	if postgres.IsUniqueViolation(err, "billable_metrics_code_key") {
		return apperr.Conflict("metric_code_taken", "Já existe uma métrica com este código.")
	}
	return err
}

func (tx catalogTx) Metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error) {
	return tx.metric(ctx, id)
}

func (b base) metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error) {
	r, err := b.q.GetMetric(ctx, dbgen.GetMetricParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return catalog.Metric{}, notFound(err, "metric_not_found", "Métrica não encontrada.")
	}
	return metricFromRow(r)
}

func (tx catalogTx) Metrics(ctx context.Context) ([]catalog.Metric, error) {
	rows, err := tx.q.ListMetrics(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Metric, 0, len(rows))
	for _, r := range rows {
		m, err := metricFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func planFromRow(r dbgen.Plan) (catalog.Plan, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return catalog.Plan{}, err
	}
	return catalog.Plan{ID: r.ID, TenantID: r.TenantID, ProductID: r.ProductID, Code: r.Code, Name: r.Name, Description: r.Description,
		Interval: calendar.Interval(r.BillingInterval), Currency: cur, TrialDays: int(r.TrialDays), Status: catalog.Status(r.Status),
		CurrentPriceVersionID: r.CurrentPriceVersionID, CreatedAt: r.CreatedAt, Version: int(r.Version)}, nil
}

func (tx catalogTx) InsertPlan(ctx context.Context, p catalog.Plan) error {
	err := tx.q.InsertPlan(ctx, dbgen.InsertPlanParams{ID: p.ID, TenantID: tx.tenantID, ProductID: p.ProductID, Code: p.Code, Name: p.Name,
		Description: p.Description, BillingInterval: string(p.Interval), Currency: p.Currency.Code(), TrialDays: int32(p.TrialDays),
		Status: string(p.Status), CurrentPriceVersionID: p.CurrentPriceVersionID, CreatedAt: p.CreatedAt})
	if postgres.IsUniqueViolation(err, "plans_code_key") {
		return apperr.Conflict("plan_code_taken", "Já existe um plano com este código.")
	}
	return err
}

func (tx catalogTx) Plan(ctx context.Context, id uuid.UUID, forUpdate bool) (catalog.Plan, error) {
	var r dbgen.Plan
	var err error
	if forUpdate {
		r, err = tx.q.GetPlanForUpdate(ctx, dbgen.GetPlanForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetPlan(ctx, dbgen.GetPlanParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return catalog.Plan{}, notFound(err, "plan_not_found", "Plano não encontrado.")
	}
	return planFromRow(r)
}

func (b base) plan(ctx context.Context, id uuid.UUID) (catalog.Plan, error) {
	r, err := b.q.GetPlan(ctx, dbgen.GetPlanParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return catalog.Plan{}, notFound(err, "plan_not_found", "Plano não encontrado.")
	}
	return planFromRow(r)
}

func (tx catalogTx) UpdatePlan(ctx context.Context, p catalog.Plan) error {
	n, err := tx.q.UpdatePlan(ctx, dbgen.UpdatePlanParams{TenantID: tx.tenantID, ID: p.ID, Name: p.Name, Description: p.Description,
		TrialDays: int32(p.TrialDays), Status: string(p.Status), CurrentPriceVersionID: p.CurrentPriceVersionID, Version: int32(p.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "O plano foi alterado por outra pessoa.")
}

func (tx catalogTx) Plans(ctx context.Context, status catalog.Status, productID *uuid.UUID) ([]catalog.Plan, error) {
	rows, err := tx.q.ListPlans(ctx, dbgen.ListPlansParams{TenantID: tx.tenantID, Status: strPtr(string(status)), ProductID: productID})
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Plan, 0, len(rows))
	for _, r := range rows {
		p, err := planFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func priceFromRow(r dbgen.PlanPriceVersion) (catalog.PriceVersion, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return catalog.PriceVersion{}, err
	}
	pv := catalog.PriceVersion{ID: r.ID, TenantID: r.TenantID, PlanID: r.PlanID, Version: int(r.Version), Model: catalog.PricingModel(r.PricingModel),
		MetricID: r.MetricID, Currency: cur, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
	if r.FlatAmount != nil {
		m := money.New(*r.FlatAmount, cur)
		pv.FlatAmount = &m
	}
	if pv.UnitAmount, err = ratFromNumeric(r.UnitAmount); err != nil {
		return catalog.PriceVersion{}, err
	}
	if pv.IncludedQuantity, err = ratFromNumeric(r.IncludedQuantity); err != nil {
		return catalog.PriceVersion{}, err
	}
	return pv, nil
}

func (tx catalogTx) InsertPriceVersion(ctx context.Context, pv catalog.PriceVersion) error {
	unit, err := numericPtr(pv.UnitAmount)
	if err != nil {
		return err
	}
	incl, err := numericPtr(pv.IncludedQuantity)
	if err != nil {
		return err
	}
	var flat *int64
	if pv.FlatAmount != nil {
		v := pv.FlatAmount.Amount()
		flat = &v
	}
	return tx.q.InsertPriceVersion(ctx, dbgen.InsertPriceVersionParams{ID: pv.ID, TenantID: tx.tenantID, PlanID: pv.PlanID, Version: int32(pv.Version),
		PricingModel: string(pv.Model), FlatAmount: flat, UnitAmount: unit, MetricID: pv.MetricID, IncludedQuantity: incl,
		Currency: pv.Currency.Code(), CreatedBy: pv.CreatedBy, CreatedAt: pv.CreatedAt})
}

func (tx catalogTx) PriceVersions(ctx context.Context, planID uuid.UUID) ([]catalog.PriceVersion, error) {
	rows, err := tx.q.ListPriceVersions(ctx, dbgen.ListPriceVersionsParams{TenantID: tx.tenantID, PlanID: planID})
	if err != nil {
		return nil, err
	}
	out := make([]catalog.PriceVersion, 0, len(rows))
	for _, r := range rows {
		pv, err := priceFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, pv)
	}
	return out, nil
}

func (tx catalogTx) PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	return tx.priceVersion(ctx, id)
}

func (b base) priceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	r, err := b.q.GetPriceVersion(ctx, dbgen.GetPriceVersionParams{TenantID: b.tenantID, ID: id})
	if err != nil {
		return catalog.PriceVersion{}, notFound(err, "price_not_found", "Versão de preço não encontrada.")
	}
	return priceFromRow(r)
}

func (tx catalogTx) MaxPriceVersion(ctx context.Context, planID uuid.UUID) (int, error) {
	v, err := tx.q.MaxPriceVersion(ctx, dbgen.MaxPriceVersionParams{TenantID: tx.tenantID, PlanID: planID})
	return int(v), err
}
