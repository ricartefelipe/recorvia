package catalog

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

var errBoom = errors.New("falha simulada")

// catalogState é o catálogo de um tenant em memória.
type catalogState struct {
	products map[uuid.UUID]Product
	metrics  map[uuid.UUID]Metric
	plans    map[uuid.UUID]Plan
	prices   map[uuid.UUID]PriceVersion
	audits   []audit.Entry

	productFilter Status
	planFilter    Status
	planProduct   *uuid.UUID
}

func newCatalogState() *catalogState {
	return &catalogState{products: map[uuid.UUID]Product{}, metrics: map[uuid.UUID]Metric{},
		plans: map[uuid.UUID]Plan{}, prices: map[uuid.UUID]PriceVersion{}}
}

func (c *catalogState) clone() *catalogState {
	n := *c
	n.products = maps.Clone(c.products)
	n.metrics = maps.Clone(c.metrics)
	n.plans = maps.Clone(c.plans)
	n.prices = maps.Clone(c.prices)
	n.audits = slices.Clone(c.audits)
	return &n
}

// catalogStore implementa Store e Tx; a transação só é confirmada quando fn
// retorna nil.
type catalogStore struct {
	tenants map[uuid.UUID]*catalogState
	fail    map[string]error
	calls   map[string]int
	// maxVersionOverride simula versões já existentes no banco.
	maxVersionOverride *int
}

func newCatalogStore() *catalogStore {
	return &catalogStore{tenants: map[uuid.UUID]*catalogState{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (s *catalogStore) state(id uuid.UUID) *catalogState {
	st, ok := s.tenants[id]
	if !ok {
		st = newCatalogState()
		s.tenants[id] = st
	}
	return st
}

func (s *catalogStore) hit(op string) error {
	s.calls[op]++
	return s.fail[op]
}

func (s *catalogStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	work := s.state(tenantID).clone()
	if err := fn(&catalogTx{s: s, st: work}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

type catalogTx struct {
	s  *catalogStore
	st *catalogState
}

func (t *catalogTx) InsertProduct(_ context.Context, p Product) error {
	if err := t.s.hit("InsertProduct"); err != nil {
		return err
	}
	t.st.products[p.ID] = p
	return nil
}

func (t *catalogTx) Product(_ context.Context, id uuid.UUID, _ bool) (Product, error) {
	if err := t.s.hit("Product"); err != nil {
		return Product{}, err
	}
	p, ok := t.st.products[id]
	if !ok {
		return Product{}, apperr.NotFound("product_not_found", "Produto não encontrado.")
	}
	return p, nil
}

func (t *catalogTx) UpdateProduct(_ context.Context, p Product) error {
	if err := t.s.hit("UpdateProduct"); err != nil {
		return err
	}
	if t.st.products[p.ID].Version != p.Version {
		return apperr.PreconditionFailed("version_mismatch", "Produto alterado por outra pessoa.")
	}
	p.Version++
	t.st.products[p.ID] = p
	return nil
}

func (t *catalogTx) Products(_ context.Context, status Status) ([]Product, error) {
	if err := t.s.hit("Products"); err != nil {
		return nil, err
	}
	t.st.productFilter = status
	var out []Product
	for _, p := range t.st.products {
		if status == "" || p.Status == status {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t *catalogTx) InsertMetric(_ context.Context, m Metric) error {
	if err := t.s.hit("InsertMetric"); err != nil {
		return err
	}
	t.st.metrics[m.ID] = m
	return nil
}

func (t *catalogTx) Metric(_ context.Context, id uuid.UUID) (Metric, error) {
	if err := t.s.hit("Metric"); err != nil {
		return Metric{}, err
	}
	m, ok := t.st.metrics[id]
	if !ok {
		return Metric{}, apperr.NotFound("metric_not_found", "Métrica não encontrada.")
	}
	return m, nil
}

func (t *catalogTx) Metrics(context.Context) ([]Metric, error) {
	if err := t.s.hit("Metrics"); err != nil {
		return nil, err
	}
	return slices.Collect(maps.Values(t.st.metrics)), nil
}

func (t *catalogTx) InsertPlan(_ context.Context, p Plan) error {
	if err := t.s.hit("InsertPlan"); err != nil {
		return err
	}
	t.st.plans[p.ID] = p
	return nil
}

func (t *catalogTx) Plan(_ context.Context, id uuid.UUID, _ bool) (Plan, error) {
	if err := t.s.hit("Plan"); err != nil {
		return Plan{}, err
	}
	p, ok := t.st.plans[id]
	if !ok {
		return Plan{}, apperr.NotFound("plan_not_found", "Plano não encontrado.")
	}
	return p, nil
}

func (t *catalogTx) UpdatePlan(_ context.Context, p Plan) error {
	if err := t.s.hit("UpdatePlan"); err != nil {
		return err
	}
	if t.st.plans[p.ID].Version != p.Version {
		return apperr.PreconditionFailed("version_mismatch", "Plano alterado por outra pessoa.")
	}
	p.Version++
	t.st.plans[p.ID] = p
	return nil
}

func (t *catalogTx) Plans(_ context.Context, status Status, productID *uuid.UUID) ([]Plan, error) {
	if err := t.s.hit("Plans"); err != nil {
		return nil, err
	}
	t.st.planFilter, t.st.planProduct = status, productID
	var out []Plan
	for _, p := range t.st.plans {
		if (status == "" || p.Status == status) && (productID == nil || p.ProductID == *productID) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (t *catalogTx) InsertPriceVersion(_ context.Context, pv PriceVersion) error {
	if err := t.s.hit("InsertPriceVersion"); err != nil {
		return err
	}
	t.st.prices[pv.ID] = pv
	return nil
}

func (t *catalogTx) PriceVersions(_ context.Context, planID uuid.UUID) ([]PriceVersion, error) {
	if err := t.s.hit("PriceVersions"); err != nil {
		return nil, err
	}
	var out []PriceVersion
	for _, pv := range t.st.prices {
		if pv.PlanID == planID {
			out = append(out, pv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (t *catalogTx) PriceVersion(_ context.Context, id uuid.UUID) (PriceVersion, error) {
	if err := t.s.hit("PriceVersion"); err != nil {
		return PriceVersion{}, err
	}
	pv, ok := t.st.prices[id]
	if !ok {
		return PriceVersion{}, apperr.NotFound("price_version_not_found", "Versão de preço não encontrada.")
	}
	return pv, nil
}

func (t *catalogTx) MaxPriceVersion(_ context.Context, planID uuid.UUID) (int, error) {
	if err := t.s.hit("MaxPriceVersion"); err != nil {
		return 0, err
	}
	if t.s.maxVersionOverride != nil {
		return *t.s.maxVersionOverride, nil
	}
	maxV := 0
	for _, pv := range t.st.prices {
		if pv.PlanID == planID && pv.Version > maxV {
			maxV = pv.Version
		}
	}
	return maxV, nil
}

func (t *catalogTx) Audit(_ context.Context, e audit.Entry) error {
	if err := t.s.hit("Audit"); err != nil {
		return err
	}
	t.st.audits = append(t.st.audits, e)
	return nil
}
