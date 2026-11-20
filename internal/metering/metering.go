// Package metering recebe e consulta eventos de consumo medido.
//
// Política de eventos:
//   - external_id identifica o evento: reenvio com o mesmo conteúdo é
//     "duplicate"; com conteúdo diferente é rejeitado. A regra vale também
//     para external_id repetido no mesmo lote, comparado com o evento já
//     gravado. A deduplicação vem antes das validações que dependem do
//     estado atual (preço vigente, métrica, suspensão, encerramento): o
//     reenvio de um evento gravado é respondido pelo conteúdo, mesmo que a
//     assinatura, o preço ou a métrica tenham mudado depois.
//   - occurred_at é truncado a microssegundos (precisão do timestamptz)
//     antes de validar, gravar e comparar.
//   - O evento pertence ao período de cobrança que contém occurred_at, no
//     fuso do contrato; eventos anteriores ao primeiro período (avaliação ou
//     antes do início), posteriores ao encerramento, ocorridos durante uma
//     suspensão (pelo histórico da assinatura, mesmo depois da reativação)
//     ou mais de 5 minutos no futuro são rejeitados.
//   - Se o período ainda não foi fechado pelo faturamento, a métrica precisa
//     fazer parte da versão de preço que vai precificá-lo e o evento é
//     "accepted" e entra no fechamento. Essa versão é a da mudança agendada
//     pendente que já vale no início do período (o motor a aplica no passo
//     desse período, antes de fechá-lo) ou, sem ela, a vigente. Período cujo
//     fechamento já passou sem agregado da métrica não é mais faturável e o
//     evento é rejeitado. O período em que uma suspensão começou continua
//     aberto depois de uma reativação que pulou o passo seguinte a ele (o
//     consumo anterior à suspensão é fechado pelo primeiro passo após a
//     reativação); períodos inteiramente suspensos nunca são fechados.
//   - Se o período já foi fechado (há agregado da métrica), a métrica é
//     válida para ele, ainda que o preço vigente tenha mudado. O evento é
//     "late" quando recebido até late_grace_hours (configuração da métrica)
//     após o fim do período, e corrige o faturamento por ajuste posterior,
//     sem alterar a fatura emitida; depois da tolerância, é rejeitado.
//   - A ingestão lê as assinaturas do lote com bloqueio compartilhado antes
//     de qualquer outra leitura ou gravação, e o faturamento as reivindica
//     com bloqueio exclusivo: a decisão entre "accepted" e "late" nunca é
//     tomada durante um fechamento ainda não confirmado.
//   - Textos com caractere NUL ou UTF-8 inválido e properties com NUL são
//     rejeitados por evento, sem desfazer o lote.
package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

// SuspensionChange é uma suspensão ou reativação registrada no histórico da
// assinatura.
type SuspensionChange struct {
	At time.Time
	// Suspended indica suspensão; falso indica reativação.
	Suspended bool
}

// Tx são as operações de consumo na transação do tenant.
type Tx interface {
	// Subscription lê a assinatura sem bloqueio (consultas).
	Subscription(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error)
	// SubscriptionForShare lê a assinatura com bloqueio compartilhado até o
	// fim da transação. O faturamento reivindica a assinatura com bloqueio
	// exclusivo; assim a ingestão espera o fechamento em andamento e só
	// depois verifica se o período está fechado.
	SubscriptionForShare(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error)
	// SuspensionHistory lista as suspensões e reativações da assinatura em
	// ordem cronológica.
	SuspensionHistory(ctx context.Context, subscriptionID uuid.UUID) ([]SuspensionChange, error)
	// PendingChange devolve a mudança agendada pendente da assinatura (nula
	// se não houver), sem bloqueio: o agendamento e a aplicação da mudança
	// bloqueiam a assinatura com exclusividade, e a ingestão a mantém com
	// bloqueio compartilhado.
	PendingChange(ctx context.Context, subscriptionID uuid.UUID) (*subscriptions.ScheduledChange, error)
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
		c := newBatchCache(tx)
		// As assinaturas do lote são bloqueadas antes de tudo, em ordem
		// crescente de ID (a mesma ordem da correção de consumo), para que
		// transações concorrentes não as bloqueiem em ordens opostas.
		for _, id := range batchSubscriptions(batch) {
			if _, err := c.subscription(ctx, id); err != nil {
				return err
			}
		}
		for i, in := range batch {
			// external_id repetido no lote passa pelo mesmo caminho: a
			// consulta por external_id enxerga o evento gravado antes na
			// mesma transação e compara o conteúdo; se o primeiro foi
			// rejeitado, o seguinte é avaliado por si.
			r, err := ingestOne(ctx, c, tenantID, in, now)
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

// batchSubscriptions devolve as assinaturas distintas do lote em ordem
// crescente (a ordem de bytes é a mesma do tipo uuid no PostgreSQL).
func batchSubscriptions(batch []EventInput) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(batch))
	for _, in := range batch {
		out = append(out, in.SubscriptionID)
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// batchCache guarda as consultas repetidas dentro de um lote. Ausências
// também são guardadas (ponteiro nulo).
type batchCache struct {
	tx          Tx
	subs        map[uuid.UUID]*subscriptions.Subscription
	metrics     map[string]*catalog.Metric
	prices      map[uuid.UUID]catalog.PriceVersion
	suspensions map[uuid.UUID][]SuspensionChange
	changes     map[uuid.UUID]*subscriptions.ScheduledChange
}

func newBatchCache(tx Tx) *batchCache {
	return &batchCache{tx: tx, subs: map[uuid.UUID]*subscriptions.Subscription{}, metrics: map[string]*catalog.Metric{},
		prices: map[uuid.UUID]catalog.PriceVersion{}, suspensions: map[uuid.UUID][]SuspensionChange{},
		changes: map[uuid.UUID]*subscriptions.ScheduledChange{}}
}

// subscription lê a assinatura com bloqueio compartilhado na primeira vez
// em que aparece no lote; nil indica assinatura inexistente.
func (c *batchCache) subscription(ctx context.Context, id uuid.UUID) (*subscriptions.Subscription, error) {
	if sub, ok := c.subs[id]; ok {
		return sub, nil
	}
	found, err := c.tx.SubscriptionForShare(ctx, id)
	if apperr.IsKind(err, apperr.KindNotFound) {
		c.subs[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.subs[id] = &found
	return &found, nil
}

// metric devolve a métrica pelo código; nil indica métrica inexistente.
func (c *batchCache) metric(ctx context.Context, code string) (*catalog.Metric, error) {
	if m, ok := c.metrics[code]; ok {
		return m, nil
	}
	found, err := c.tx.MetricByCode(ctx, code)
	if apperr.IsKind(err, apperr.KindNotFound) {
		c.metrics[code] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.metrics[code] = &found
	return &found, nil
}

func (c *batchCache) price(ctx context.Context, id uuid.UUID) (catalog.PriceVersion, error) {
	if p, ok := c.prices[id]; ok {
		return p, nil
	}
	p, err := c.tx.PriceVersion(ctx, id)
	if err != nil {
		return catalog.PriceVersion{}, err
	}
	c.prices[id] = p
	return p, nil
}

// openPeriodPrice devolve a versão de preço do período aberto que começa em
// periodStart (ver openPeriodPriceID).
func (c *batchCache) openPeriodPrice(ctx context.Context, sub subscriptions.Subscription, periodStart time.Time) (catalog.PriceVersion, error) {
	ch, ok := c.changes[sub.ID]
	if !ok {
		var err error
		if ch, err = c.tx.PendingChange(ctx, sub.ID); err != nil {
			return catalog.PriceVersion{}, err
		}
		c.changes[sub.ID] = ch
	}
	return c.price(ctx, openPeriodPriceID(sub, ch, periodStart))
}

// openPeriodPriceID é a versão de preço que vai precificar o consumo de um
// período ainda aberto: a da mudança agendada pendente com data efetiva até o
// início do período (já em vigor, ainda não aplicada pelo motor) ou, sem ela,
// a vigente na assinatura.
func openPeriodPriceID(sub subscriptions.Subscription, ch *subscriptions.ScheduledChange, periodStart time.Time) uuid.UUID {
	if ch != nil && ch.NewPriceVersionID != nil && !ch.EffectiveAt.After(periodStart) {
		return *ch.NewPriceVersionID
	}
	return sub.PriceVersionID
}

// suspendedAt informa se a assinatura estava suspensa no instante at: pela
// situação atual e pela última suspensão ou reativação registrada no
// histórico até at, inclusive.
func (c *batchCache) suspendedAt(ctx context.Context, sub subscriptions.Subscription, at time.Time) (bool, error) {
	if sub.Status == subscriptions.Suspended && sub.SuspendedAt != nil && !at.Before(*sub.SuspendedAt) {
		return true, nil
	}
	history, ok := c.suspensions[sub.ID]
	if !ok {
		var err error
		if history, err = c.tx.SuspensionHistory(ctx, sub.ID); err != nil {
			return false, err
		}
		c.suspensions[sub.ID] = history
	}
	return suspendedIn(history, at), nil
}

// suspendedIn aplica a última mudança registrada até at, inclusive; em
// empate de instante vale a que aparece depois na lista.
func suspendedIn(history []SuspensionChange, at time.Time) bool {
	var last *SuspensionChange
	for i := range history {
		ch := &history[i]
		if ch.At.After(at) {
			continue
		}
		if last == nil || !ch.At.Before(last.At) {
			last = ch
		}
	}
	return last != nil && last.Suspended
}

func reject(in EventInput, msg string) Result {
	return Result{ExternalID: in.ExternalID, Outcome: OutcomeRejected, Error: msg}
}

func ingestOne(ctx context.Context, c *batchCache, tenantID uuid.UUID, in EventInput, now time.Time) (Result, error) {
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	if l := len(in.ExternalID); l < 1 || l > 128 {
		return reject(in, "external_id deve ter de 1 a 128 caracteres."), nil
	}
	// O PostgreSQL recusa NUL e UTF-8 inválido em text; a verificação vem
	// antes de qualquer consulta para que o erro não desfaça o lote.
	if msg := invalidText(in); msg != "" {
		return reject(in, msg), nil
	}
	sub, err := c.subscription(ctx, in.SubscriptionID)
	if err != nil {
		return Result{}, err
	}
	if sub == nil {
		return reject(in, "Assinatura não encontrada."), nil
	}
	metric, err := c.metric(ctx, in.MetricCode)
	if err != nil {
		return Result{}, err
	}
	if metric == nil {
		return reject(in, "Métrica não encontrada."), nil
	}
	ev, msg := parseEvent(tenantID, *sub, *metric, in, now)
	if msg != "" {
		return reject(in, msg), nil
	}
	// Deduplicação antes das validações que dependem do estado atual: o
	// reenvio de um evento gravado é respondido pelo conteúdo, mesmo que a
	// assinatura, o preço ou a métrica tenham mudado depois.
	existing, err := c.tx.EventByExternalID(ctx, ev.ExternalID)
	switch {
	case err == nil:
		return duplicateOrConflict(in, existing, ev), nil
	case !apperr.IsKind(err, apperr.KindNotFound):
		return Result{}, err
	}
	outcome, msg, err := admit(ctx, c, *sub, *metric, in, &ev, now)
	if err != nil {
		return Result{}, err
	}
	if msg != "" {
		return reject(in, msg), nil
	}
	inserted, err := c.tx.InsertEvent(ctx, ev)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		// Inserção concorrente com o mesmo external_id.
		existing, err := c.tx.EventByExternalID(ctx, ev.ExternalID)
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

// invalidText aponta o primeiro campo de texto com caractere NUL ou UTF-8
// inválido.
func invalidText(in EventInput) string {
	fields := [...]struct{ name, value string }{{"external_id", in.ExternalID}, {"metric_code", in.MetricCode},
		{"unit", in.Unit}, {"unique_key", in.UniqueKey}}
	for _, f := range fields {
		if strings.IndexByte(f.value, 0) >= 0 || !utf8.ValidString(f.value) {
			return f.name + " contém caractere inválido (NUL ou UTF-8 inválido)."
		}
	}
	return ""
}

// hasNUL informa se alguma chave ou texto das propriedades serializadas
// contém o caractere NUL (escape \u0000), que o jsonb não aceita.
func hasNUL(serialized []byte) bool {
	if !bytes.Contains(serialized, []byte(`\u0000`)) {
		return false
	}
	// O trecho pode ser uma barra invertida literal seguida de "u0000";
	// a decodificação distingue os dois casos.
	var v any
	if json.Unmarshal(serialized, &v) != nil {
		return true
	}
	return containsNUL(v)
}

func containsNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.IndexByte(x, 0) >= 0
	case map[string]any:
		for k, e := range x {
			if strings.IndexByte(k, 0) >= 0 || containsNUL(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if containsNUL(e) {
				return true
			}
		}
	}
	return false
}

// parseEvent interpreta o conteúdo do evento, que é o que a deduplicação
// compara. As validações aqui dependem somente do próprio evento; as que
// dependem do estado atual da assinatura, do preço ou da métrica ficam em
// admit, depois da deduplicação. Retorna a mensagem de rejeição, se houver.
func parseEvent(tenantID uuid.UUID, sub subscriptions.Subscription, metric catalog.Metric, in EventInput, now time.Time) (Event, string) {
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
	}
	key := strings.TrimSpace(in.UniqueKey)
	if len(key) > 128 {
		return Event{}, "unique_key deve ter no máximo 128 caracteres."
	}
	if in.Properties != nil {
		b, err := json.Marshal(in.Properties)
		if err != nil || len(b) > MaxPropertiesSize {
			return Event{}, "properties deve ter no máximo 4 KB."
		}
		if hasNUL(b) {
			return Event{}, "properties não pode conter o caractere NUL (\\u0000)."
		}
	}
	// timestamptz guarda microssegundos: o instante é truncado antes de
	// validar, gravar e comparar, para que o reenvio do mesmo evento seja
	// reconhecido como duplicata.
	at := in.OccurredAt.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		return Event{}, "Informe occurred_at."
	}
	return Event{ID: ids.New(), TenantID: tenantID, ExternalID: in.ExternalID, CustomerID: sub.CustomerID, SubscriptionID: sub.ID,
		MetricID: metric.ID, Quantity: qty, UniqueKey: key, OccurredAt: at, ReceivedAt: now, Status: Accepted,
		Properties: in.Properties}, ""
}

// admit valida o evento contra o estado atual da métrica, da assinatura e
// do período, e define o período e a situação do evento. Retorna a mensagem
// de rejeição, se houver.
func admit(ctx context.Context, c *batchCache, sub subscriptions.Subscription, metric catalog.Metric, in EventInput, ev *Event,
	now time.Time) (Outcome, string, error) {
	if msg := checkMetric(metric, in, *ev); msg != "" {
		return "", msg, nil
	}
	at := ev.OccurredAt
	switch {
	case at.After(now.Add(MaxFutureSkew)):
		return "", "occurred_at está no futuro.", nil
	case at.Before(sub.StartedAt):
		return "", "occurred_at é anterior ao início da assinatura.", nil
	case sub.TrialEndsAt != nil && at.Before(*sub.TrialEndsAt):
		return "", "Consumo durante o período de avaliação não é faturável.", nil
	case sub.EndedAt != nil && !at.Before(*sub.EndedAt):
		return "", "occurred_at é posterior ao encerramento da assinatura.", nil
	}
	suspended, err := c.suspendedAt(ctx, sub, at)
	if err != nil {
		return "", "", err
	}
	if suspended {
		return "", "Assinatura suspensa: consumo não é faturável durante a suspensão.", nil
	}
	period, idx, msg := eventPeriod(sub, at)
	if msg != "" {
		return "", msg, nil
	}
	ev.PeriodStart = period.Start
	closed, err := c.tx.PeriodClosed(ctx, sub.ID, metric.ID, period.Start)
	if err != nil {
		return "", "", err
	}
	if !closed {
		price, err := c.openPeriodPrice(ctx, sub, period.Start)
		if err != nil {
			return "", "", err
		}
		if !price.HasUsage() || price.MetricID == nil || *price.MetricID != metric.ID {
			return "", "A métrica não faz parte do preço vigente da assinatura.", nil
		}
		// O consumo do período idx é fechado no passo idx+1 ou, se a
		// reativação pulou esse passo, no primeiro passo após ela. Se o
		// faturamento já passou do fechamento sem agregado da métrica (o
		// preço daquele período não tinha a métrica) ou o período nunca será
		// fechado (inteiramente suspenso), o evento não seria faturado nem
		// corrigido.
		if !sub.UsageOpen(idx, period.Start) {
			return "", "Período já faturado sem consumo desta métrica.", nil
		}
		return OutcomeAccepted, "", nil
	}
	// Período fechado: a métrica foi faturada nele, com a versão de preço
	// registrada no agregado, ainda que o preço vigente tenha mudado.
	end := period.End
	if sub.EndedAt != nil && sub.EndedAt.Before(end) {
		end = *sub.EndedAt
	}
	if now.After(end.Add(time.Duration(metric.LateGraceHours) * time.Hour)) {
		return "", "Período já faturado e fora da tolerância de atraso da métrica.", nil
	}
	ev.Status = Late
	return OutcomeLate, "", nil
}

// eventPeriod devolve o período de cobrança que contém at, no fuso do
// contrato, e o seu índice. Retorna a mensagem de rejeição, se houver.
func eventPeriod(sub subscriptions.Subscription, at time.Time) (calendar.Period, int, string) {
	sched, err := sub.Schedule()
	if err != nil {
		return calendar.Period{}, 0, "Calendário da assinatura inválido."
	}
	idx := sched.IndexOf(at)
	if idx < 0 {
		return calendar.Period{}, 0, "occurred_at é anterior ao primeiro período de cobrança."
	}
	return sched.Period(idx), idx, ""
}

// checkMetric valida o evento contra a configuração atual da métrica.
func checkMetric(metric catalog.Metric, in EventInput, ev Event) string {
	switch {
	case metric.Status != catalog.Active:
		return "A métrica está arquivada."
	case in.Unit != "" && in.Unit != metric.Unit:
		return "Unidade diferente da unidade da métrica (" + metric.Unit + ")."
	case metric.MaxQuantityPerEvent != nil && ev.Quantity.Cmp(metric.MaxQuantityPerEvent) > 0:
		return "Quantidade acima do máximo por evento da métrica (" + money.RatString(metric.MaxQuantityPerEvent) + ")."
	case metric.Aggregation == catalog.AggUniqueCount && ev.UniqueKey == "":
		return "unique_key é obrigatório para métricas de contagem distinta."
	}
	return ""
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
// (estimativa: o valor final é calculado no fechamento). A versão de preço é
// a que vai precificar o período, como na ingestão: a da mudança agendada já
// em vigor no início dele, se houver.
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
		ch, err := tx.PendingChange(ctx, sub.ID)
		if err != nil {
			return err
		}
		price, err := tx.PriceVersion(ctx, openPeriodPriceID(sub, ch, u.Period.Start))
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
