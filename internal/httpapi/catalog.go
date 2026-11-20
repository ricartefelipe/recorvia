package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

type productDTO struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	Version     int       `json:"version"`
}

func prodDTO(p catalog.Product) productDTO {
	return productDTO{ID: p.ID, Name: p.Name, Description: p.Description, Status: string(p.Status), CreatedAt: p.CreatedAt, Version: p.Version}
}

func (a *API) listProducts(w http.ResponseWriter, r *http.Request) {
	items, err := a.Catalog.Products(r.Context(), mustPrincipal(r).TenantID, catalog.Status(r.URL.Query().Get("status")))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]productDTO, 0, len(items))
	for _, p := range items {
		out = append(out, prodDTO(p))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	prod, err := a.Catalog.CreateProduct(r.Context(), p.TenantID, p.Actor(), catalog.ProductInput{Name: in.Name, Description: in.Description})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, prod.Version, prodDTO(prod))
}

func (a *API) updateProduct(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Status      catalog.Status `json:"status"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	prod, err := a.Catalog.UpdateProduct(r.Context(), p.TenantID, p.Actor(), id, catalog.ProductInput{Name: in.Name, Description: in.Description}, in.Status, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusOK, prod.Version, prodDTO(prod))
}

type metricDTO struct {
	ID                  uuid.UUID `json:"id"`
	Code                string    `json:"code"`
	Name                string    `json:"name"`
	Unit                string    `json:"unit"`
	Aggregation         string    `json:"aggregation"`
	MaxQuantityPerEvent string    `json:"max_quantity_per_event"`
	LateGraceHours      int       `json:"late_grace_hours"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"created_at"`
}

func metDTO(m catalog.Metric) metricDTO {
	return metricDTO{ID: m.ID, Code: m.Code, Name: m.Name, Unit: m.Unit, Aggregation: string(m.Aggregation),
		MaxQuantityPerEvent: money.RatString(m.MaxQuantityPerEvent), LateGraceHours: m.LateGraceHours, Status: string(m.Status), CreatedAt: m.CreatedAt}
}

func (a *API) listMetrics(w http.ResponseWriter, r *http.Request) {
	items, err := a.Catalog.Metrics(r.Context(), mustPrincipal(r).TenantID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]metricDTO, 0, len(items))
	for _, m := range items {
		out = append(out, metDTO(m))
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

func (a *API) createMetric(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code                string              `json:"code"`
		Name                string              `json:"name"`
		Unit                string              `json:"unit"`
		Aggregation         catalog.Aggregation `json:"aggregation"`
		MaxQuantityPerEvent string              `json:"max_quantity_per_event"`
		LateGraceHours      *int                `json:"late_grace_hours"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	m, err := a.Catalog.CreateMetric(r.Context(), p.TenantID, p.Actor(), catalog.MetricInput{Code: in.Code, Name: in.Name, Unit: in.Unit,
		Aggregation: in.Aggregation, MaxQuantityPerEvent: in.MaxQuantityPerEvent, LateGraceHours: in.LateGraceHours})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, metDTO(m))
}

type priceDTO struct {
	ID                uuid.UUID  `json:"id"`
	Version           int        `json:"version"`
	PricingModel      string     `json:"pricing_model"`
	FlatAmount        *int64     `json:"flat_amount"`
	UnitAmountDecimal *string    `json:"unit_amount_decimal"`
	MetricID          *uuid.UUID `json:"metric_id"`
	IncludedQuantity  *string    `json:"included_quantity"`
	Currency          string     `json:"currency"`
	CreatedAt         time.Time  `json:"created_at"`
}

func priceVersionDTO(pv catalog.PriceVersion) priceDTO {
	out := priceDTO{ID: pv.ID, Version: pv.Version, PricingModel: string(pv.Model), MetricID: pv.MetricID, Currency: pv.Currency.Code(), CreatedAt: pv.CreatedAt}
	if pv.FlatAmount != nil {
		v := pv.FlatAmount.Amount()
		out.FlatAmount = &v
	}
	if pv.UnitAmount != nil {
		v := money.RatString(pv.UnitAmount)
		out.UnitAmountDecimal = &v
	}
	if pv.IncludedQuantity != nil {
		v := money.RatString(pv.IncludedQuantity)
		out.IncludedQuantity = &v
	}
	return out
}

type planDTO struct {
	ID              uuid.UUID  `json:"id"`
	ProductID       uuid.UUID  `json:"product_id"`
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	BillingInterval string     `json:"billing_interval"`
	Currency        string     `json:"currency"`
	TrialDays       int        `json:"trial_days"`
	Status          string     `json:"status"`
	CurrentPrice    *priceDTO  `json:"current_price"`
	CreatedAt       time.Time  `json:"created_at"`
	Version         int        `json:"version"`
	Prices          []priceDTO `json:"prices,omitempty"`
}

func plnDTO(p catalog.Plan, prices []catalog.PriceVersion) planDTO {
	out := planDTO{ID: p.ID, ProductID: p.ProductID, Code: p.Code, Name: p.Name, Description: p.Description, BillingInterval: string(p.Interval),
		Currency: p.Currency.Code(), TrialDays: p.TrialDays, Status: string(p.Status), CreatedAt: p.CreatedAt, Version: p.Version}
	for _, pv := range prices {
		d := priceVersionDTO(pv)
		if p.CurrentPriceVersionID != nil && pv.ID == *p.CurrentPriceVersionID {
			cur := d
			out.CurrentPrice = &cur
		}
		out.Prices = append(out.Prices, d)
	}
	return out
}

func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	productID, err := queryUUID(r, "product_id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	tenant := mustPrincipal(r).TenantID
	items, err := a.Catalog.Plans(r.Context(), tenant, catalog.Status(r.URL.Query().Get("status")), productID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]planDTO, 0, len(items))
	for _, p := range items {
		d, err := a.Catalog.Plan(r.Context(), tenant, p.ID)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		dto := plnDTO(d.Plan, d.Prices)
		dto.Prices = nil
		out = append(out, dto)
	}
	a.writeJSON(w, r, http.StatusOK, list(out))
}

type priceInput struct {
	PricingModel      catalog.PricingModel `json:"pricing_model"`
	FlatAmount        *int64               `json:"flat_amount"`
	UnitAmountDecimal string               `json:"unit_amount_decimal"`
	MetricID          *uuid.UUID           `json:"metric_id"`
	IncludedQuantity  string               `json:"included_quantity"`
}

func (in priceInput) domain() catalog.PriceInput {
	return catalog.PriceInput{Model: in.PricingModel, FlatAmount: in.FlatAmount, UnitAmount: in.UnitAmountDecimal, MetricID: in.MetricID,
		IncludedQuantity: in.IncludedQuantity}
}

func (a *API) createPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProductID       uuid.UUID         `json:"product_id"`
		Code            string            `json:"code"`
		Name            string            `json:"name"`
		Description     string            `json:"description"`
		BillingInterval calendar.Interval `json:"billing_interval"`
		TrialDays       int               `json:"trial_days"`
		Price           priceInput        `json:"price"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	uid := p.UserID()
	d, err := a.Catalog.CreatePlan(r.Context(), p.TenantID, p.Actor(), uid, catalog.PlanInput{ProductID: in.ProductID, Code: in.Code, Name: in.Name,
		Description: in.Description, Interval: in.BillingInterval, TrialDays: in.TrialDays}, in.Price.domain())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, d.Version, plnDTO(d.Plan, d.Prices))
}

func (a *API) getPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	d, err := a.Catalog.Plan(r.Context(), mustPrincipal(r).TenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	dto := plnDTO(d.Plan, d.Prices)
	if dto.Prices == nil {
		dto.Prices = []priceDTO{}
	}
	a.writeVersioned(w, r, http.StatusOK, d.Version, dto)
}

func (a *API) updatePlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		TrialDays   int            `json:"trial_days"`
		Status      catalog.Status `json:"status"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	if _, err := a.Catalog.UpdatePlan(r.Context(), p.TenantID, p.Actor(), id, catalog.UpdatePlanInput{Name: in.Name, Description: in.Description,
		TrialDays: in.TrialDays, Status: in.Status}, version); err != nil {
		a.writeError(w, r, err)
		return
	}
	a.getPlan(w, r)
}

func (a *API) addPrice(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in priceInput
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	uid := p.UserID()
	pv, err := a.Catalog.AddPriceVersion(r.Context(), p.TenantID, p.Actor(), uid, id, in.domain())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, priceVersionDTO(pv))
}
