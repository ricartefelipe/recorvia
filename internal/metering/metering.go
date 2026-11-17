// Package metering recebe e consulta eventos de consumo medido.
//
// Política de eventos:
//   - external_id identifica o evento: reenvio com o mesmo conteúdo é
//     "duplicate"; com conteúdo diferente é rejeitado.
//   - O evento pertence ao período de cobrança que contém occurred_at, no
//     fuso do contrato; eventos anteriores ao primeiro período (avaliação ou
//     antes do início), posteriores ao encerramento, durante suspensão ou
//     mais de 5 minutos no futuro são rejeitados.
//   - Se o período ainda não foi fechado pelo faturamento, o evento é
//     "accepted" e entra no fechamento.
//   - Se o período já foi fechado, o evento é "late" quando recebido até
//     late_grace_hours (configuração da métrica) após o fim do período, e
//     corrige o faturamento por ajuste posterior, sem alterar a fatura
//     emitida; depois da tolerância, é rejeitado.
package metering

import (
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Limites de ingestão.
const (
	MaxBatch          = 500
	MaxPropertiesSize = 4096
	MaxFutureSkew     = 5 * time.Minute
	// MaxScale é o número de casas decimais aceitas (numeric(20,6)).
	MaxScale = 6
)

// maxQuantity é o maior valor representável (10^14).
var maxQuantity = new(big.Rat).SetInt64(100_000_000_000_000)

// Status é a situação de um evento aceito.
type Status string

const (
	Accepted Status = "accepted"
	Late     Status = "late"
)

// Outcome é o resultado da ingestão de um evento.
type Outcome string

const (
	OutcomeAccepted  Outcome = "accepted"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeLate      Outcome = "late"
	OutcomeRejected  Outcome = "rejected"
)

// EventInput é um evento recebido.
type EventInput struct {
	ExternalID     string
	SubscriptionID uuid.UUID
	MetricCode     string
	Unit           string
	Quantity       string
	UniqueKey      string
	OccurredAt     time.Time
	Properties     map[string]any
}

// Event é um evento persistido.
type Event struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ExternalID     string
	CustomerID     uuid.UUID
	SubscriptionID uuid.UUID
	MetricID       uuid.UUID
	Quantity       *big.Rat
	UniqueKey      string
	OccurredAt     time.Time
	ReceivedAt     time.Time
	PeriodStart    time.Time
	Status         Status
	AggregateID    *uuid.UUID
	InvoiceItemID  *uuid.UUID
	Properties     map[string]any
}

// sameContent compara o conteúdo relevante para a deduplicação.
func (e Event) sameContent(o Event) bool {
	return e.SubscriptionID == o.SubscriptionID && e.MetricID == o.MetricID && e.Quantity.Cmp(o.Quantity) == 0 &&
		e.UniqueKey == o.UniqueKey && e.OccurredAt.Equal(o.OccurredAt) && reflect.DeepEqual(normalize(e.Properties), normalize(o.Properties))
}

func normalize(p map[string]any) any {
	if len(p) == 0 {
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return p
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return p
	}
	return out
}

// Result é o resultado por evento.
type Result struct {
	ExternalID string
	Outcome    Outcome
	EventID    *uuid.UUID
	Error      string
}

// Tx são as operações de consumo na transação do tenant.
type Tx interface {
	Subscription(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error)
	PriceVersion(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error)
	Metric(ctx context.Context, id uuid.UUID) (catalog.Metric, error)
	MetricByCode(ctx context.Context, code string) (catalog.Metric, error)
	EventByExternalID(ctx context.Context, externalID string) (Event, error)
	InsertEvent(ctx context.Context, e Event) (bool, error)
	PeriodClosed(ctx context.Context, subscriptionID, metricID uuid.UUID, periodStart time.Time) (bool, error)
	Aggregate(ctx context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error)
	ListEvents(ctx context.Context, f ListFilter) (Page, error)
}

// Store abre transações do tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
}

// Service implementa a ingestão e as consultas de consumo.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// Ingest grava um lote de eventos numa única transação. Erros de validação
// de um evento não impedem os demais (resultado "rejected"); falhas de
// infraestrutura desfazem o lote inteiro, que pode ser reenviado com
// segurança (deduplicação por external_id).
func (s *Service) Ingest(ctx context.Context, tenantID uuid.UUID, batch []EventInput) ([]Result, error) {
	if len(batch) == 0 || len(batch) > MaxBatch {
		return nil, apperr.Invalid("invalid_usage_batch", "Envie de 1 a 500 eventos por lote.")
	}
	results := make([]Result, len(batch))
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		now := s.clock.Now()
		subs := map[uuid.UUID]*subscriptions.Subscription{}
		metrics := map[string]*catalog.Metric{}
		prices := map[uuid.UUID]catalog.PriceVersion{}
		seen := map[string]int{}
		for i, in := range batch {
			if j, dup := seen[in.ExternalID]; dup && in.ExternalID != "" {
				results[i] = Result{ExternalID: in.ExternalID, Outcome: OutcomeDuplicate, EventID: results[j].EventID}
				if results[j].Outcome == OutcomeRejected {
					results[i] = results[j]
				}
				continue
			}
			seen[in.ExternalID] = i
			r, err := s.ingestOne(ctx, tx, tenantID, in, now, subs, metrics, prices)
			if err != nil {
				return err
			}
			results[i] = r
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func reject(in EventInput, msg string) Result {
	return Result{ExternalID: in.ExternalID, Outcome: OutcomeRejected, Error: msg}
}

func (s *Service) ingestOne(ctx context.Context, tx Tx, tenantID uuid.UUID, in EventInput, now time.Time,
	subs map[uuid.UUID]*subscriptions.Subscription, metrics map[string]*catalog.Metric, prices map[uuid.UUID]catalog.PriceVersion) (Result, error) {
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	if l := len(in.ExternalID); l < 1 || l > 128 {
		return reject(in, "external_id deve ter de 1 a 128 caracteres."), nil
	}
	sub, ok := subs[in.SubscriptionID]
	if !ok {
		found, err := tx.Subscription(ctx, in.SubscriptionID)
		if apperr.IsKind(err, apperr.KindNotFound) {
			subs[in.SubscriptionID] = nil
			return reject(in, "Assinatura não encontrada."), nil
		}
		if err != nil {
			return Result{}, err
		}
		sub, subs[in.SubscriptionID] = &found, &found
	}
	if sub == nil {
		return reject(in, "Assinatura não encontrada."), nil
	}
	metric, ok := metrics[in.MetricCode]
	if !ok {
		found, err := tx.MetricByCode(ctx, in.MetricCode)
		if apperr.IsKind(err, apperr.KindNotFound) {
			metrics[in.MetricCode] = nil
			return reject(in, "Métrica não encontrada."), nil
		}
		if err != nil {
			return Result{}, err
		}
		metric, metrics[in.MetricCode] = &found, &found
	}
	if metric == nil {
		return reject(in, "Métrica não encontrada."), nil
	}
	price, ok := prices[sub.PriceVersionID]
	if !ok {
		var err error
		if price, err = tx.PriceVersion(ctx, sub.PriceVersionID); err != nil {
			return Result{}, err
		}
		prices[sub.PriceVersionID] = price
	}
	ev, msg := buildEvent(tenantID, *sub, price, *metric, in, now)
	if msg != "" {
		return reject(in, msg), nil
	}
	existing, err := tx.EventByExternalID(ctx, ev.ExternalID)
	switch {
	case err == nil:
		return duplicateOrConflict(in, existing, ev), nil
	case !apperr.IsKind(err, apperr.KindNotFound):
		return Result{}, err
	}
	closed, err := tx.PeriodClosed(ctx, sub.ID, metric.ID, ev.PeriodStart)
	if err != nil {
		return Result{}, err
	}
	outcome := OutcomeAccepted
	if closed {
		sched, err := sub.Schedule()
		if err != nil {
			return Result{}, err
		}
		end := sched.Period(sched.IndexOf(ev.OccurredAt)).End
		if sub.EndedAt != nil && sub.EndedAt.Before(end) {
			end = *sub.EndedAt
		}
		if now.After(end.Add(time.Duration(metric.LateGraceHours) * time.Hour)) {
			return reject(in, "Período já faturado e fora da tolerância de atraso da métrica."), nil
		}
		ev.Status, outcome = Late, OutcomeLate
	}
	inserted, err := tx.InsertEvent(ctx, ev)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		// Inserção concorrente com o mesmo external_id.
		existing, err := tx.EventByExternalID(ctx, ev.ExternalID)
		if err != nil {
			return Result{}, err
		}
		return duplicateOrConflict(in, existing, ev), nil
	}
	id := ev.ID
	return Result{ExternalID: in.ExternalID, Outcome: outcome, EventID: &id}, nil
}

func duplicateOrConflict(in EventInput, existing, ev Event) Result {
	if !existing.sameContent(ev) {
		return reject(in, "external_id já usado por um evento com conteúdo diferente.")
	}
	id := existing.ID
	return Result{ExternalID: in.ExternalID, Outcome: OutcomeDuplicate, EventID: &id}
}

// buildEvent valida o evento contra a assinatura, o preço e a métrica.
// Retorna a mensagem de rejeição, se houver.
func buildEvent(tenantID uuid.UUID, sub subscriptions.Subscription, price catalog.PriceVersion, metric catalog.Metric,
	in EventInput, now time.Time) (Event, string) {
	if !price.HasUsage() || price.MetricID == nil || *price.MetricID != metric.ID {
		return Event{}, "A métrica não faz parte do preço vigente da assinatura."
	}
	if metric.Status != catalog.Active {
		return Event{}, "A métrica está arquivada."
	}
	if in.Unit != "" && in.Unit != metric.Unit {
		return Event{}, "Unidade diferente da unidade da métrica (" + metric.Unit + ")."
	}
	qty, err := money.ParseDecimal(strings.TrimSpace(in.Quantity))
	switch {
	case err != nil:
		return Event{}, "Quantidade deve ser um decimal com ponto (por exemplo, 12.5)."
	case qty.Sign() < 0:
		return Event{}, "Quantidade não pode ser negativa."
	case qty.Cmp(maxQuantity) >= 0:
		return Event{}, "Quantidade acima do limite."
	case !withinScale(qty, MaxScale):
		return Event{}, "Quantidade com mais de 6 casas decimais."
	case metric.MaxQuantityPerEvent != nil && qty.Cmp(metric.MaxQuantityPerEvent) > 0:
		return Event{}, "Quantidade acima do máximo por evento da métrica (" + money.RatString(metric.MaxQuantityPerEvent) + ")."
	}
	key := strings.TrimSpace(in.UniqueKey)
	if metric.Aggregation == catalog.AggUniqueCount && key == "" {
		return Event{}, "unique_key é obrigatório para métricas de contagem distinta."
	}
	if len(key) > 128 {
		return Event{}, "unique_key deve ter no máximo 128 caracteres."
	}
	if in.Properties != nil {
		b, err := json.Marshal(in.Properties)
		if err != nil || len(b) > MaxPropertiesSize {
			return Event{}, "properties deve ter no máximo 4 KB."
		}
	}
	at := in.OccurredAt.UTC()
	if at.IsZero() {
		return Event{}, "Informe occurred_at."
	}
	if at.After(now.Add(MaxFutureSkew)) {
		return Event{}, "occurred_at está no futuro."
	}
	if at.Before(sub.StartedAt) {
		return Event{}, "occurred_at é anterior ao início da assinatura."
	}
	if sub.TrialEndsAt != nil && at.Before(*sub.TrialEndsAt) {
		return Event{}, "Consumo durante o período de avaliação não é faturável."
	}
	if sub.EndedAt != nil && !at.Before(*sub.EndedAt) {
		return Event{}, "occurred_at é posterior ao encerramento da assinatura."
	}
	if sub.Status == subscriptions.Suspended && sub.SuspendedAt != nil && !at.Before(*sub.SuspendedAt) {
		return Event{}, "Assinatura suspensa: consumo não é faturável durante a suspensão."
	}
	sched, err := sub.Schedule()
	if err != nil {
		return Event{}, "Calendário da assinatura inválido."
	}
	idx := sched.IndexOf(at)
	if idx < 0 {
		return Event{}, "occurred_at é anterior ao primeiro período de cobrança."
	}
	return Event{ID: ids.New(), TenantID: tenantID, ExternalID: strings.TrimSpace(in.ExternalID), CustomerID: sub.CustomerID,
		SubscriptionID: sub.ID, MetricID: metric.ID, Quantity: qty, UniqueKey: key, OccurredAt: at, ReceivedAt: now,
		PeriodStart: sched.Period(idx).Start, Status: Accepted, Properties: in.Properties}, ""
}

func withinScale(r *big.Rat, scale int) bool {
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	return new(big.Rat).Mul(r, new(big.Rat).SetInt(d)).IsInt()
}

// ListFilter define os filtros da listagem.
type ListFilter struct {
	SubscriptionID *uuid.UUID
	MetricID       *uuid.UUID
	Limit          int
	Cursor         string
}

// Page é uma página de eventos.
type Page struct {
	Items      []Event
	NextCursor string
}

// List lista eventos do mais recente para o mais antigo.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (Page, error) {
	var p Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.ListEvents(ctx, f)
		return err
	})
	return p, err
}

// MetricUsage é o consumo agregado de uma métrica no período corrente.
type MetricUsage struct {
	Metric          catalog.Metric
	Quantity        *big.Rat
	EventCount      int
	EstimatedAmount money.Money
}

// Usage é o consumo do período de cobrança em andamento.
type Usage struct {
	Period  calendar.Period
	Metrics []MetricUsage
}

// CurrentUsage agrega o consumo do período que contém o instante atual
// (estimativa: o valor final é calculado no fechamento).
func (s *Service) CurrentUsage(ctx context.Context, tenantID, subscriptionID uuid.UUID) (Usage, error) {
	var u Usage
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx, subscriptionID)
		if err != nil {
			return err
		}
		sched, err := sub.Schedule()
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if sub.EndedAt != nil && sub.EndedAt.Before(now) {
			now = sub.EndedAt.Add(-time.Nanosecond)
		}
		idx := max(sched.IndexOf(now), 0)
		u.Period = sched.Period(idx)
		u.Metrics = []MetricUsage{}
		price, err := tx.PriceVersion(ctx, sub.PriceVersionID)
		if err != nil || !price.HasUsage() || price.MetricID == nil {
			return err
		}
		metric, err := tx.Metric(ctx, *price.MetricID)
		if err != nil {
			return err
		}
		qty, count, err := tx.Aggregate(ctx, sub.ID, metric, u.Period.Start)
		if err != nil {
			return err
		}
		amount, _, err := price.UsageCharge(qty)
		if err != nil {
			return err
		}
		u.Metrics = append(u.Metrics, MetricUsage{Metric: metric, Quantity: qty, EventCount: count, EstimatedAmount: amount})
		return nil
	})
	return u, err
}
