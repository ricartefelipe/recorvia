package metering

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

var errBoom = errors.New("falha simulada")

// periodKey identifica o fechamento de uma métrica num período.
type periodKey struct {
	subscriptionID uuid.UUID
	metricID       uuid.UUID
	periodStart    int64
}

func keyOf(subscriptionID, metricID uuid.UUID, periodStart time.Time) periodKey {
	return periodKey{subscriptionID: subscriptionID, metricID: metricID, periodStart: periodStart.UnixNano()}
}

// meterState é o consumo de um tenant em memória. Eventos são indexados por
// external_id, como a restrição única (tenant_id, external_id).
type meterState struct {
	subs    map[uuid.UUID]subscriptions.Subscription
	prices  map[uuid.UUID]catalog.PriceVersion
	metrics map[uuid.UUID]catalog.Metric
	events  map[string]Event
	closed  map[periodKey]bool
}

func newMeterState() *meterState {
	return &meterState{subs: map[uuid.UUID]subscriptions.Subscription{}, prices: map[uuid.UUID]catalog.PriceVersion{},
		metrics: map[uuid.UUID]catalog.Metric{}, events: map[string]Event{}, closed: map[periodKey]bool{}}
}

func (s *meterState) clone() *meterState {
	return &meterState{subs: maps.Clone(s.subs), prices: maps.Clone(s.prices), metrics: maps.Clone(s.metrics),
		events: maps.Clone(s.events), closed: maps.Clone(s.closed)}
}

// failure programa a falha de uma operação; call zero falha em todas as
// chamadas, call n falha somente na n-ésima.
type failure struct {
	err  error
	call int
}

// meterStore implementa Store e Tx; a transação só é confirmada quando fn
// retorna nil.
type meterStore struct {
	tenants map[uuid.UUID]*meterState
	fail    map[string]failure
	calls   map[string]int
	// concurrent simula eventos confirmados por outra transação entre a
	// consulta por external_id e a inserção.
	concurrent map[string]Event
	lastFilter *ListFilter
	aggregated []time.Time
}

func newMeterStore() *meterStore {
	return &meterStore{tenants: map[uuid.UUID]*meterState{}, fail: map[string]failure{}, calls: map[string]int{},
		concurrent: map[string]Event{}}
}

func (s *meterStore) state(id uuid.UUID) *meterState {
	st, ok := s.tenants[id]
	if !ok {
		st = newMeterState()
		s.tenants[id] = st
	}
	return st
}

func (s *meterStore) hit(op string) error {
	s.calls[op]++
	f, ok := s.fail[op]
	if !ok || (f.call != 0 && f.call != s.calls[op]) {
		return nil
	}
	return f.err
}

func (s *meterStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	work := s.state(tenantID).clone()
	if err := fn(&meterTx{s: s, st: work}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

type meterTx struct {
	s  *meterStore
	st *meterState
}

func (t *meterTx) Subscription(_ context.Context, id uuid.UUID) (subscriptions.Subscription, error) {
	if err := t.s.hit("Subscription"); err != nil {
		return subscriptions.Subscription{}, err
	}
	sub, ok := t.st.subs[id]
	if !ok {
		return subscriptions.Subscription{}, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
	}
	return sub, nil
}

func (t *meterTx) PriceVersion(_ context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	if err := t.s.hit("PriceVersion"); err != nil {
		return catalog.PriceVersion{}, err
	}
	p, ok := t.st.prices[id]
	if !ok {
		return catalog.PriceVersion{}, apperr.NotFound("price_version_not_found", "Versão de preço não encontrada.")
	}
	return p, nil
}

func (t *meterTx) Metric(_ context.Context, id uuid.UUID) (catalog.Metric, error) {
	if err := t.s.hit("Metric"); err != nil {
		return catalog.Metric{}, err
	}
	m, ok := t.st.metrics[id]
	if !ok {
		return catalog.Metric{}, apperr.NotFound("metric_not_found", "Métrica não encontrada.")
	}
	return m, nil
}

func (t *meterTx) MetricByCode(_ context.Context, code string) (catalog.Metric, error) {
	if err := t.s.hit("MetricByCode"); err != nil {
		return catalog.Metric{}, err
	}
	for _, m := range t.st.metrics {
		if m.Code == code {
			return m, nil
		}
	}
	return catalog.Metric{}, apperr.NotFound("metric_not_found", "Métrica não encontrada.")
}

func (t *meterTx) EventByExternalID(_ context.Context, externalID string) (Event, error) {
	if err := t.s.hit("EventByExternalID"); err != nil {
		return Event{}, err
	}
	e, ok := t.st.events[externalID]
	if !ok {
		return Event{}, apperr.NotFound("usage_event_not_found", "Evento de consumo não encontrado.")
	}
	return e, nil
}

// InsertEvent reproduz o INSERT ... ON CONFLICT DO NOTHING: devolve false
// quando o external_id já existe. Instantes são gravados com precisão de
// microssegundos, como no timestamptz do PostgreSQL.
func (t *meterTx) InsertEvent(_ context.Context, e Event) (bool, error) {
	if err := t.s.hit("InsertEvent"); err != nil {
		return false, err
	}
	if other, ok := t.s.concurrent[e.ExternalID]; ok {
		delete(t.s.concurrent, e.ExternalID)
		t.st.events[e.ExternalID] = other
		return false, nil
	}
	if _, ok := t.st.events[e.ExternalID]; ok {
		return false, nil
	}
	e.Quantity = new(big.Rat).Set(e.Quantity)
	e.OccurredAt = e.OccurredAt.Truncate(time.Microsecond)
	e.ReceivedAt = e.ReceivedAt.Truncate(time.Microsecond)
	t.st.events[e.ExternalID] = e
	return true, nil
}

func (t *meterTx) PeriodClosed(_ context.Context, subscriptionID, metricID uuid.UUID, periodStart time.Time) (bool, error) {
	if err := t.s.hit("PeriodClosed"); err != nil {
		return false, err
	}
	return t.st.closed[keyOf(subscriptionID, metricID, periodStart)], nil
}

// Aggregate soma as quantidades do período (a semântica de cada agregação é
// coberta pelo adaptador PostgreSQL).
func (t *meterTx) Aggregate(_ context.Context, subscriptionID uuid.UUID, metric catalog.Metric, periodStart time.Time) (*big.Rat, int, error) {
	if err := t.s.hit("Aggregate"); err != nil {
		return nil, 0, err
	}
	t.s.aggregated = append(t.s.aggregated, periodStart)
	total, count := new(big.Rat), 0
	for _, e := range t.st.events {
		if e.SubscriptionID == subscriptionID && e.MetricID == metric.ID && e.PeriodStart.Equal(periodStart) {
			total.Add(total, e.Quantity)
			count++
		}
	}
	return total, count, nil
}

// ListEvents filtra por assinatura e métrica e ordena do recebimento mais
// recente para o mais antigo; o cursor é o ID do último item devolvido.
func (t *meterTx) ListEvents(_ context.Context, f ListFilter) (Page, error) {
	if err := t.s.hit("ListEvents"); err != nil {
		return Page{}, err
	}
	t.s.lastFilter = &f
	var items []Event
	for _, e := range t.st.events {
		if (f.SubscriptionID == nil || e.SubscriptionID == *f.SubscriptionID) && (f.MetricID == nil || e.MetricID == *f.MetricID) {
			items = append(items, e)
		}
	}
	slices.SortFunc(items, func(a, b Event) int {
		return cmp.Or(b.ReceivedAt.Compare(a.ReceivedAt), cmp.Compare(b.ID.String(), a.ID.String()))
	})
	var p Page
	if f.Limit > 0 && len(items) > f.Limit {
		items = items[:f.Limit]
		p.NextCursor = items[len(items)-1].ID.String()
	}
	p.Items = items
	return p, nil
}
