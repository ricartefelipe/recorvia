// Package catalog modela produtos, métricas de consumo, planos e versões de
// preço. Uma versão de preço é imutável: mudar o preço cria uma nova versão,
// e assinaturas e faturas existentes continuam apontando para a versão (e o
// snapshot) com que foram criadas.
package catalog

import (
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Status de itens do catálogo.
type Status string

const (
	Active   Status = "active"
	Archived Status = "archived"
)

// Product é um produto ou serviço ofertado.
type Product struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Name        string
	Description string
	Status      Status
	CreatedAt   time.Time
	Version     int
}

// Aggregation define como eventos de consumo são agregados no período. A
// semântica depende da métrica: soma não é aplicada indiscriminadamente.
type Aggregation string

const (
	// AggSum soma quantidades (ex.: requisições, minutos).
	AggSum Aggregation = "sum"
	// AggMax usa o pico do período (ex.: usuários simultâneos).
	AggMax Aggregation = "max"
	// AggUniqueCount conta chaves distintas (ex.: usuários ativos).
	AggUniqueCount Aggregation = "unique_count"
	// AggLast usa o último valor reportado no período (ex.: armazenamento).
	AggLast Aggregation = "last"
)

// Metric é uma métrica de consumo faturável.
type Metric struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Code                string
	Name                string
	Unit                string
	Aggregation         Aggregation
	MaxQuantityPerEvent *big.Rat
	LateGraceHours      int
	Status              Status
	CreatedAt           time.Time
}

// Plan é um plano comercial do tenant.
type Plan struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	ProductID             uuid.UUID
	Code                  string
	Name                  string
	Description           string
	Interval              calendar.Interval
	Currency              money.Currency
	TrialDays             int
	Status                Status
	CurrentPriceVersionID *uuid.UUID
	CreatedAt             time.Time
	Version               int
}

// PricingModel é o modelo de cobrança da versão de preço.
type PricingModel string

const (
	// Flat cobra valor fixo por período.
	Flat PricingModel = "flat"
	// PerUnit cobra por quantidade contratada (usuários, unidades).
	PerUnit PricingModel = "per_unit"
	// Usage cobra pelo consumo medido no período (pós-pago).
	Usage PricingModel = "usage"
	// FlatPlusOverage cobra mensalidade fixa com franquia e excedente.
	FlatPlusOverage PricingModel = "flat_plus_overage"
)

// PriceVersion é uma versão imutável de preço. UnitAmount está em unidades
// mínimas (centavos) e pode ter até 10 casas decimais.
type PriceVersion struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	PlanID           uuid.UUID
	Version          int
	Model            PricingModel
	FlatAmount       *money.Money
	UnitAmount       *big.Rat
	MetricID         *uuid.UUID
	IncludedQuantity *big.Rat
	Currency         money.Currency
	CreatedBy        *uuid.UUID
	CreatedAt        time.Time
}

// HasUsage informa se a versão depende de consumo medido.
func (p PriceVersion) HasUsage() bool { return p.Model == Usage || p.Model == FlatPlusOverage }

// HasFixed informa se a versão tem componente recorrente fixo.
func (p PriceVersion) HasFixed() bool { return p.Model != Usage }

// FixedCharge calcula o componente recorrente do período (cobrança
// antecipada): valor fixo, ou preço unitário vezes a quantidade contratada,
// arredondado uma única vez.
func (p PriceVersion) FixedCharge(quantity int) (amount money.Money, qty, unit *big.Rat, err error) {
	switch p.Model {
	case Flat, FlatPlusOverage:
		return *p.FlatAmount, big.NewRat(1, 1), p.FlatAmount.Rat(), nil
	case PerUnit:
		q := big.NewRat(int64(quantity), 1)
		total := new(big.Rat).Mul(p.UnitAmount, q)
		amount, err = money.FromRat(total, p.Currency, money.DefaultRounding)
		return amount, q, p.UnitAmount, err
	}
	return money.Zero(p.Currency), new(big.Rat), new(big.Rat), nil
}

// UsageCharge calcula o componente de consumo (pós-pago) a partir da
// quantidade agregada. Em FlatPlusOverage cobra apenas o excedente.
func (p PriceVersion) UsageCharge(aggregated *big.Rat) (amount money.Money, billable *big.Rat, err error) {
	if !p.HasUsage() || aggregated.Sign() <= 0 {
		return money.Zero(p.Currency), new(big.Rat), nil
	}
	billable = new(big.Rat).Set(aggregated)
	if p.Model == FlatPlusOverage {
		billable.Sub(billable, p.IncludedQuantity)
		if billable.Sign() <= 0 {
			return money.Zero(p.Currency), new(big.Rat), nil
		}
	}
	amount, err = money.FromRat(new(big.Rat).Mul(p.UnitAmount, billable), p.Currency, money.DefaultRounding)
	return amount, billable, err
}

// MonthlyRecurring normaliza o componente fixo para um mês (MRR): planos
// anuais dividem por 12. Consumo e excedente não entram na recorrência fixa.
func (p PriceVersion) MonthlyRecurring(interval calendar.Interval, quantity int) (*big.Rat, error) {
	if !p.HasFixed() {
		return new(big.Rat), nil
	}
	amount, _, _, err := p.FixedCharge(quantity)
	if err != nil {
		return nil, err
	}
	r := amount.Rat()
	if interval == calendar.Yearly {
		r.Quo(r, big.NewRat(12, 1))
	}
	return r, nil
}

// PriceInput são os dados de uma nova versão de preço.
type PriceInput struct {
	Model            PricingModel
	FlatAmount       *int64
	UnitAmount       string
	MetricID         *uuid.UUID
	IncludedQuantity string
}

// NewPriceVersion valida e cria a versão de preço.
func NewPriceVersion(id, tenantID, planID uuid.UUID, version int, cur money.Currency, in PriceInput, metric *Metric, createdBy *uuid.UUID, now time.Time) (PriceVersion, error) {
	var f []apperr.FieldError
	add := func(field, msg string) { f = append(f, apperr.FieldError{Field: field, Message: msg}) }
	pv := PriceVersion{ID: id, TenantID: tenantID, PlanID: planID, Version: version, Model: in.Model, Currency: cur, CreatedBy: createdBy, CreatedAt: now}
	needFlat := in.Model == Flat || in.Model == FlatPlusOverage
	needUnit := in.Model == PerUnit || in.Model == Usage || in.Model == FlatPlusOverage
	needMetric := in.Model == Usage || in.Model == FlatPlusOverage
	switch in.Model {
	case Flat, PerUnit, Usage, FlatPlusOverage:
	default:
		add("pricing_model", "Modelo de preço inválido.")
	}
	if needFlat {
		if in.FlatAmount == nil || *in.FlatAmount < 0 || *in.FlatAmount > money.MaxAmount {
			add("flat_amount", "Informe o valor fixo em centavos (zero ou positivo).")
		} else {
			m := money.New(*in.FlatAmount, cur)
			pv.FlatAmount = &m
		}
	} else if in.FlatAmount != nil {
		add("flat_amount", "Este modelo não usa valor fixo.")
	}
	if needUnit {
		r, err := money.ParseDecimal(in.UnitAmount)
		if err != nil || r.Sign() < 0 || r.Cmp(new(big.Rat).SetInt64(money.MaxAmount)) > 0 {
			add("unit_amount_decimal", "Informe o preço unitário em centavos, com até 10 casas decimais.")
		} else {
			pv.UnitAmount = r
		}
	} else if in.UnitAmount != "" {
		add("unit_amount_decimal", "Este modelo não usa preço unitário.")
	}
	if needMetric {
		switch {
		case in.MetricID == nil || metric == nil:
			add("metric_id", "Informe a métrica de consumo.")
		case metric.Status != Active:
			add("metric_id", "A métrica está arquivada.")
		default:
			pv.MetricID = in.MetricID
		}
	} else if in.MetricID != nil {
		add("metric_id", "Este modelo não usa métrica.")
	}
	if in.Model == FlatPlusOverage {
		r, err := money.ParseDecimal(in.IncludedQuantity)
		if err != nil || r.Sign() < 0 {
			add("included_quantity", "Informe a franquia incluída (zero ou positiva).")
		} else {
			pv.IncludedQuantity = r
		}
	} else if in.IncludedQuantity != "" {
		add("included_quantity", "Este modelo não usa franquia.")
	}
	if len(f) > 0 {
		return PriceVersion{}, apperr.Invalid("invalid_price", "Preço inválido.", f...)
	}
	return pv, nil
}

var codeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{1,63}$`)

func textField(f *[]apperr.FieldError, field, v string, minLen, maxLen int, msg string) string {
	v = strings.TrimSpace(v)
	if n := len([]rune(v)); n < minLen || n > maxLen {
		*f = append(*f, apperr.FieldError{Field: field, Message: msg})
	}
	return v
}

// ProductInput são os dados de um produto.
type ProductInput struct {
	Name        string
	Description string
}

// NewProduct valida e cria um produto.
func NewProduct(id, tenantID uuid.UUID, in ProductInput, now time.Time) (Product, error) {
	var f []apperr.FieldError
	name := textField(&f, "name", in.Name, 2, 200, "Informe um nome entre 2 e 200 caracteres.")
	desc := textField(&f, "description", in.Description, 0, 2000, "Descrição deve ter no máximo 2.000 caracteres.")
	if len(f) > 0 {
		return Product{}, apperr.Invalid("invalid_product", "Produto inválido.", f...)
	}
	return Product{ID: id, TenantID: tenantID, Name: name, Description: desc, Status: Active, CreatedAt: now, Version: 1}, nil
}

// MetricInput são os dados de uma métrica.
type MetricInput struct {
	Code                string
	Name                string
	Unit                string
	Aggregation         Aggregation
	MaxQuantityPerEvent string
	LateGraceHours      *int
}

var unitRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// NewMetric valida e cria uma métrica.
func NewMetric(id, tenantID uuid.UUID, in MetricInput, now time.Time) (Metric, error) {
	var f []apperr.FieldError
	code := strings.TrimSpace(in.Code)
	if !codeRe.MatchString(code) {
		f = append(f, apperr.FieldError{Field: "code", Message: "Use de 2 a 64 letras minúsculas, números, '.', '_' ou '-'."})
	}
	name := textField(&f, "name", in.Name, 2, 200, "Informe um nome entre 2 e 200 caracteres.")
	unit := strings.TrimSpace(in.Unit)
	if !unitRe.MatchString(unit) {
		f = append(f, apperr.FieldError{Field: "unit", Message: "Use até 32 letras minúsculas, números ou '_'."})
	}
	switch in.Aggregation {
	case AggSum, AggMax, AggUniqueCount, AggLast:
	default:
		f = append(f, apperr.FieldError{Field: "aggregation", Message: "Use sum, max, unique_count ou last."})
	}
	maxQty, err := money.ParseDecimal(in.MaxQuantityPerEvent)
	if err != nil || maxQty.Sign() <= 0 {
		f = append(f, apperr.FieldError{Field: "max_quantity_per_event", Message: "Informe o limite positivo por evento."})
	}
	grace := 72
	if in.LateGraceHours != nil {
		grace = *in.LateGraceHours
		if grace < 0 || grace > 720 {
			f = append(f, apperr.FieldError{Field: "late_grace_hours", Message: "A tolerância deve ficar entre 0 e 720 horas."})
		}
	}
	if len(f) > 0 {
		return Metric{}, apperr.Invalid("invalid_metric", "Métrica inválida.", f...)
	}
	return Metric{ID: id, TenantID: tenantID, Code: code, Name: name, Unit: unit, Aggregation: in.Aggregation,
		MaxQuantityPerEvent: maxQty, LateGraceHours: grace, Status: Active, CreatedAt: now}, nil
}

// PlanInput são os dados de um plano.
type PlanInput struct {
	ProductID   uuid.UUID
	Code        string
	Name        string
	Description string
	Interval    calendar.Interval
	TrialDays   int
}

// NewPlan valida e cria um plano.
func NewPlan(id, tenantID uuid.UUID, in PlanInput, now time.Time) (Plan, error) {
	var f []apperr.FieldError
	code := strings.TrimSpace(in.Code)
	if !codeRe.MatchString(code) {
		f = append(f, apperr.FieldError{Field: "code", Message: "Use de 2 a 64 letras minúsculas, números, '.', '_' ou '-'."})
	}
	name := textField(&f, "name", in.Name, 2, 200, "Informe um nome entre 2 e 200 caracteres.")
	desc := textField(&f, "description", in.Description, 0, 2000, "Descrição deve ter no máximo 2.000 caracteres.")
	if _, err := calendar.ParseInterval(string(in.Interval)); err != nil {
		f = append(f, apperr.FieldError{Field: "billing_interval", Message: "Use month ou year."})
	}
	if in.TrialDays < 0 || in.TrialDays > 365 {
		f = append(f, apperr.FieldError{Field: "trial_days", Message: "O trial deve ficar entre 0 e 365 dias."})
	}
	if in.ProductID == uuid.Nil {
		f = append(f, apperr.FieldError{Field: "product_id", Message: "Informe o produto."})
	}
	if len(f) > 0 {
		return Plan{}, apperr.Invalid("invalid_plan", "Plano inválido.", f...)
	}
	return Plan{ID: id, TenantID: tenantID, ProductID: in.ProductID, Code: code, Name: name, Description: desc,
		Interval: in.Interval, Currency: money.BRL, TrialDays: in.TrialDays, Status: Active, CreatedAt: now, Version: 1}, nil
}
