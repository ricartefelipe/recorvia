package metering

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// Calendário de referência: assinatura mensal em America/Sao_Paulo (UTC-3)
// ancorada em 10/01/2026; os períodos começam às 03:00 UTC do dia 10.
var (
	startedAt  = time.Date(2026, time.January, 10, 13, 0, 0, 0, time.UTC)
	p1Start    = time.Date(2026, time.February, 10, 3, 0, 0, 0, time.UTC)
	p2Start    = time.Date(2026, time.March, 10, 3, 0, 0, 0, time.UTC)
	p2End      = time.Date(2026, time.April, 10, 3, 0, 0, 0, time.UTC)
	testNow    = time.Date(2026, time.March, 15, 12, 0, 0, 0, time.UTC)
	occurredAt = time.Date(2026, time.March, 14, 10, 0, 0, 0, time.UTC)
	lateAt     = time.Date(2026, time.February, 20, 15, 0, 0, 0, time.UTC)
)

const (
	msgConflict      = "external_id já usado por um evento com conteúdo diferente."
	msgNotInPrice    = "A métrica não faz parte do preço vigente da assinatura."
	msgDecimal       = "Quantidade deve ser um decimal com ponto (por exemplo, 12.5)."
	msgNegative      = "Quantidade não pode ser negativa."
	msgAboveLimit    = "Quantidade acima do limite."
	msgScale         = "Quantidade com mais de 6 casas decimais."
	msgOutOfGrace    = "Período já faturado e fora da tolerância de atraso da métrica."
	msgPropertiesMax = "properties deve ter no máximo 4 KB."
)

type env struct {
	store    *meterStore
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	sub      subscriptions.Subscription
	price    catalog.PriceVersion
	metric   catalog.Metric
}

func newEnv() *env {
	tenantID := uuid.New()
	metric := catalog.Metric{ID: uuid.New(), TenantID: tenantID, Code: "api.requests", Name: "Requisições", Unit: "request",
		Aggregation: catalog.AggSum, MaxQuantityPerEvent: big.NewRat(1000, 1), LateGraceHours: 72, Status: catalog.Active}
	metricID := metric.ID
	price := catalog.PriceVersion{ID: uuid.New(), TenantID: tenantID, PlanID: uuid.New(), Version: 1, Model: catalog.Usage,
		UnitAmount: big.NewRat(1, 2), MetricID: &metricID, Currency: money.BRL}
	sub := subscriptions.Subscription{ID: uuid.New(), TenantID: tenantID, CustomerID: uuid.New(), PlanID: price.PlanID,
		PriceVersionID: price.ID, Status: subscriptions.Active, Quantity: 1, Interval: calendar.Monthly, Timezone: "America/Sao_Paulo",
		Anchor: calendar.Date{Year: 2026, Month: time.January, Day: 10}, StartedAt: startedAt, Version: 1}
	store := newMeterStore()
	st := store.state(tenantID)
	st.subs[sub.ID] = sub
	st.prices[price.ID] = price
	st.metrics[metric.ID] = metric
	c := clock.NewManual(testNow)
	return &env{store: store, clock: c, svc: NewService(store, c), tenantID: tenantID, sub: sub, price: price, metric: metric}
}

func (e *env) st() *meterState { return e.store.state(e.tenantID) }

func (e *env) editSub(fn func(*subscriptions.Subscription)) {
	fn(&e.sub)
	e.st().subs[e.sub.ID] = e.sub
}

func (e *env) editMetric(fn func(*catalog.Metric)) {
	fn(&e.metric)
	e.st().metrics[e.metric.ID] = e.metric
}

func (e *env) editPrice(fn func(*catalog.PriceVersion)) {
	fn(&e.price)
	e.st().prices[e.price.ID] = e.price
}

func (e *env) closePeriod(start time.Time) { e.st().closed[keyOf(e.sub.ID, e.metric.ID, start)] = true }

func (e *env) input() EventInput {
	return EventInput{ExternalID: "evt-1", SubscriptionID: e.sub.ID, MetricCode: e.metric.Code, Unit: e.metric.Unit,
		Quantity: "12.5", OccurredAt: occurredAt}
}

func (e *env) ingest(t *testing.T, batch ...EventInput) []Result {
	t.Helper()
	res, err := e.svc.Ingest(context.Background(), e.tenantID, batch)
	if err != nil {
		t.Fatalf("Ingest: erro inesperado: %v", err)
	}
	if len(res) != len(batch) {
		t.Fatalf("Ingest: %d resultados para %d eventos", len(res), len(batch))
	}
	return res
}

func assertAppErr(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("esperava *apperr.Error %v/%s, obteve %v", kind, code, err)
	}
	if ae.Kind != kind || ae.Code != code {
		t.Fatalf("esperava erro %v/%s, obteve %v/%s", kind, code, ae.Kind, ae.Code)
	}
}

func assertOutcome(t *testing.T, r Result, want Outcome) {
	t.Helper()
	if r.Outcome != want {
		t.Fatalf("resultado %q (erro %q), esperava %q", r.Outcome, r.Error, want)
	}
	if want == OutcomeRejected {
		if r.EventID != nil || r.Error == "" {
			t.Fatalf("rejeição deve ter mensagem e nenhum evento: %+v", r)
		}
		return
	}
	if r.EventID == nil || r.Error != "" {
		t.Fatalf("resultado %q deve apontar o evento e não ter erro: %+v", want, r)
	}
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return r
}

// --- Ingest: lote ---

func TestIngestBatchSize(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{{"lote vazio", 0}, {"lote acima do limite", MaxBatch + 1}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			batch := make([]EventInput, tc.size)
			for i := range batch {
				batch[i] = e.input()
			}
			res, err := e.svc.Ingest(context.Background(), e.tenantID, batch)
			assertAppErr(t, err, apperr.KindInvalid, "invalid_usage_batch")
			if res != nil || e.store.calls["InTenant"] != 0 {
				t.Fatalf("lote inválido não deve abrir transação: resultados %v, chamadas %v", res, e.store.calls)
			}
		})
	}
	t.Run("lote nulo", func(t *testing.T) {
		e := newEnv()
		_, err := e.svc.Ingest(context.Background(), e.tenantID, nil)
		assertAppErr(t, err, apperr.KindInvalid, "invalid_usage_batch")
	})
}

func TestIngestMaxBatchUsesCachedLookups(t *testing.T) {
	e := newEnv()
	batch := make([]EventInput, MaxBatch)
	for i := range batch {
		in := e.input()
		in.ExternalID = fmt.Sprintf("evt-%03d", i)
		in.Quantity = "1"
		batch[i] = in
	}
	for i, r := range e.ingest(t, batch...) {
		if r.Outcome != OutcomeAccepted {
			t.Fatalf("evento %d: resultado %q (%s)", i, r.Outcome, r.Error)
		}
	}
	if n := len(e.st().events); n != MaxBatch {
		t.Fatalf("eventos gravados = %d, esperava %d", n, MaxBatch)
	}
	for _, op := range []string{"InTenant", "SubscriptionForShare", "SuspensionHistory", "MetricByCode", "PendingChange", "PriceVersion"} {
		if e.store.calls[op] != 1 {
			t.Fatalf("%s chamado %d vezes, esperava 1 (consulta em cache no lote)", op, e.store.calls[op])
		}
	}
	if e.store.calls["Subscription"] != 0 {
		t.Fatalf("a ingestão deve ler a assinatura com bloqueio compartilhado: %d leituras sem bloqueio", e.store.calls["Subscription"])
	}
}

// --- Ingest: aceito ---

func TestIngestAcceptedEvent(t *testing.T) {
	e := newEnv()
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	in := e.input()
	in.ExternalID = "  evt-1  "
	in.Unit = ""
	in.Quantity = " 12.50 "
	in.UniqueKey = "  user-7  "
	in.OccurredAt = occurredAt.In(loc)
	in.Properties = map[string]any{"region": "sa-east-1"}

	res := e.ingest(t, in)
	assertOutcome(t, res[0], OutcomeAccepted)
	if res[0].ExternalID != "evt-1" {
		t.Fatalf("external_id do resultado = %q, esperava sem espaços", res[0].ExternalID)
	}
	ev, ok := e.st().events["evt-1"]
	if !ok {
		t.Fatal("evento não gravado com o external_id normalizado")
	}
	switch {
	case ev.ID != *res[0].EventID:
		t.Fatalf("ID gravado %s difere do resultado %s", ev.ID, *res[0].EventID)
	case ev.TenantID != e.tenantID || ev.CustomerID != e.sub.CustomerID || ev.SubscriptionID != e.sub.ID || ev.MetricID != e.metric.ID:
		t.Fatalf("vínculos do evento incorretos: %+v", ev)
	case ev.Quantity.Cmp(big.NewRat(25, 2)) != 0:
		t.Fatalf("quantidade = %s, esperava 12.5", ev.Quantity.RatString())
	case ev.UniqueKey != "user-7":
		t.Fatalf("unique_key = %q, esperava sem espaços", ev.UniqueKey)
	case !ev.OccurredAt.Equal(occurredAt) || ev.OccurredAt.Location() != time.UTC:
		t.Fatalf("occurred_at = %v, esperava %v em UTC", ev.OccurredAt, occurredAt)
	case !ev.ReceivedAt.Equal(testNow):
		t.Fatalf("received_at = %v, esperava o relógio %v", ev.ReceivedAt, testNow)
	case !ev.PeriodStart.Equal(p2Start):
		t.Fatalf("period_start = %v, esperava %v", ev.PeriodStart, p2Start)
	case ev.Status != Accepted:
		t.Fatalf("situação = %q, esperava accepted", ev.Status)
	case ev.Properties["region"] != "sa-east-1":
		t.Fatalf("properties não preservadas: %v", ev.Properties)
	}
}

func TestIngestAcceptsBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*env)
		edit  func(*EventInput)
	}{
		{"preço fixo com excedente", func(e *env) {
			e.editPrice(func(p *catalog.PriceVersion) {
				p.Model, p.FlatAmount, p.IncludedQuantity = catalog.FlatPlusOverage, new(money.New(1000, money.BRL)), big.NewRat(10, 1)
			})
		}, nil},
		{"quantidade zero", nil, func(in *EventInput) { in.Quantity = "0" }},
		{"seis casas decimais", nil, func(in *EventInput) { in.Quantity = "0.000001" }},
		{"zeros à direita além de seis casas", nil, func(in *EventInput) { in.Quantity = "1.0000000" }},
		{"igual ao máximo por evento", nil, func(in *EventInput) { in.Quantity = "1000" }},
		{"maior valor representável sem máximo por evento", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.MaxQuantityPerEvent = nil })
		}, func(in *EventInput) { in.Quantity = "99999999999999.999999" }},
		{"unique_key com 128 caracteres", nil, func(in *EventInput) { in.UniqueKey = strings.Repeat("k", 128) }},
		{"external_id com 128 caracteres", nil, func(in *EventInput) { in.ExternalID = strings.Repeat("e", 128) }},
		{"contagem distinta com unique_key", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.Aggregation = catalog.AggUniqueCount })
		}, func(in *EventInput) { in.UniqueKey = "user-1" }},
		{"properties vazias", nil, func(in *EventInput) { in.Properties = map[string]any{} }},
		{"occurred_at no limite da tolerância de relógio", nil, func(in *EventInput) { in.OccurredAt = testNow.Add(MaxFutureSkew) }},
		{"nanossegundos além do limite são truncados", nil, func(in *EventInput) {
			in.OccurredAt = testNow.Add(MaxFutureSkew + 999*time.Nanosecond)
		}},
		{"occurred_at igual ao início da assinatura", nil, func(in *EventInput) { in.OccurredAt = startedAt }},
		{"occurred_at igual ao fim do trial", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.TrialEndsAt = &occurredAt })
		}, nil},
		{"occurred_at antes do encerramento", func(e *env) {
			ended := occurredAt.Add(time.Nanosecond)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}, nil},
		{"occurred_at antes da suspensão", func(e *env) {
			suspended := occurredAt.Add(time.Second)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.SuspendedAt = subscriptions.Suspended, &suspended })
		}, nil},
		{"data de suspensão sem situação suspensa", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.SuspendedAt = &startedAt })
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.setup != nil {
				tc.setup(e)
			}
			in := e.input()
			if tc.edit != nil {
				tc.edit(&in)
			}
			assertOutcome(t, e.ingest(t, in)[0], OutcomeAccepted)
			if len(e.st().events) != 1 {
				t.Fatalf("eventos gravados = %d, esperava 1", len(e.st().events))
			}
		})
	}
}

// --- Ingest: rejeições ---

func TestIngestRejections(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*env)
		edit  func(*EventInput)
		want  string
	}{
		{"external_id vazio", nil, func(in *EventInput) { in.ExternalID = "" }, "external_id deve ter de 1 a 128 caracteres."},
		{"external_id só com espaços", nil, func(in *EventInput) { in.ExternalID = "   " }, "external_id deve ter de 1 a 128 caracteres."},
		{"external_id longo", nil, func(in *EventInput) { in.ExternalID = strings.Repeat("e", 129) }, "external_id deve ter de 1 a 128 caracteres."},
		{"assinatura inexistente", nil, func(in *EventInput) { in.SubscriptionID = uuid.New() }, "Assinatura não encontrada."},
		{"métrica inexistente", nil, func(in *EventInput) { in.MetricCode = "storage.gb" }, "Métrica não encontrada."},
		{"preço sem consumo", func(e *env) {
			e.editPrice(func(p *catalog.PriceVersion) { p.Model, p.FlatAmount = catalog.Flat, new(money.New(1000, money.BRL)) })
		}, nil, msgNotInPrice},
		{"preço de consumo sem métrica", func(e *env) {
			e.editPrice(func(p *catalog.PriceVersion) { p.MetricID = nil })
		}, nil, msgNotInPrice},
		{"preço com outra métrica", func(e *env) {
			e.editPrice(func(p *catalog.PriceVersion) { p.MetricID = new(uuid.New()) })
		}, nil, msgNotInPrice},
		{"métrica arquivada", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.Status = catalog.Archived })
		}, nil, "A métrica está arquivada."},
		{"unidade diferente", nil, func(in *EventInput) { in.Unit = "minute" }, "Unidade diferente da unidade da métrica (request)."},
		{"quantidade vazia", nil, func(in *EventInput) { in.Quantity = "" }, msgDecimal},
		{"quantidade só com espaços", nil, func(in *EventInput) { in.Quantity = "  " }, msgDecimal},
		{"quantidade com vírgula", nil, func(in *EventInput) { in.Quantity = "12,5" }, msgDecimal},
		{"quantidade com expoente", nil, func(in *EventInput) { in.Quantity = "1e3" }, msgDecimal},
		{"quantidade não numérica", nil, func(in *EventInput) { in.Quantity = "doze" }, msgDecimal},
		{"quantidade com mais de dez casas", nil, func(in *EventInput) { in.Quantity = "0.00000000001" }, msgDecimal},
		{"quantidade negativa", nil, func(in *EventInput) { in.Quantity = "-0.5" }, msgNegative},
		{"quantidade no limite da coluna", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.MaxQuantityPerEvent = nil })
		}, func(in *EventInput) { in.Quantity = "100000000000000" }, msgAboveLimit},
		{"quantidade com sete casas", nil, func(in *EventInput) { in.Quantity = "1.0000001" }, msgScale},
		{"quantidade acima do máximo por evento", nil, func(in *EventInput) { in.Quantity = "1000.000001" },
			"Quantidade acima do máximo por evento da métrica (1000)."},
		{"contagem distinta sem unique_key", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.Aggregation = catalog.AggUniqueCount })
		}, func(in *EventInput) { in.UniqueKey = "   " }, "unique_key é obrigatório para métricas de contagem distinta."},
		{"unique_key longa", nil, func(in *EventInput) { in.UniqueKey = strings.Repeat("k", 129) }, "unique_key deve ter no máximo 128 caracteres."},
		{"properties acima de 4 KB", nil, func(in *EventInput) {
			in.Properties = map[string]any{"payload": strings.Repeat("x", MaxPropertiesSize)}
		}, msgPropertiesMax},
		{"properties não serializáveis", nil, func(in *EventInput) { in.Properties = map[string]any{"valor": math.Inf(1)} }, msgPropertiesMax},
		{"occurred_at ausente", nil, func(in *EventInput) { in.OccurredAt = time.Time{} }, "Informe occurred_at."},
		{"occurred_at no futuro", nil, func(in *EventInput) { in.OccurredAt = testNow.Add(MaxFutureSkew + time.Microsecond) },
			"occurred_at está no futuro."},
		{"occurred_at antes do início", nil, func(in *EventInput) { in.OccurredAt = startedAt.Add(-time.Nanosecond) },
			"occurred_at é anterior ao início da assinatura."},
		{"occurred_at durante o trial", func(e *env) {
			trialEnd := occurredAt.Add(time.Nanosecond)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.TrialEndsAt = subscriptions.Trialing, &trialEnd })
		}, nil, "Consumo durante o período de avaliação não é faturável."},
		{"occurred_at no encerramento", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &occurredAt })
		}, nil, "occurred_at é posterior ao encerramento da assinatura."},
		{"occurred_at durante a suspensão", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.SuspendedAt = subscriptions.Suspended, &occurredAt })
		}, nil, "Assinatura suspensa: consumo não é faturável durante a suspensão."},
		{"fuso inválido", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Timezone = "Invalido/Fuso" })
		}, nil, "Calendário da assinatura inválido."},
		{"periodicidade inválida", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Interval = calendar.Interval("week") })
		}, nil, "Calendário da assinatura inválido."},
		{"occurred_at antes da âncora do ciclo", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Anchor = calendar.Date{Year: 2026, Month: time.March, Day: 20} })
		}, nil, "occurred_at é anterior ao primeiro período de cobrança."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.setup != nil {
				tc.setup(e)
			}
			in := e.input()
			if tc.edit != nil {
				tc.edit(&in)
			}
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, OutcomeRejected)
			if r.Error != tc.want {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, tc.want)
			}
			if len(e.st().events) != 0 || e.store.calls["InsertEvent"] != 0 {
				t.Fatalf("evento rejeitado não deve ser gravado: %d eventos, %d inserções", len(e.st().events), e.store.calls["InsertEvent"])
			}
		})
	}
}

func TestIngestCachesMissingLookups(t *testing.T) {
	cases := []struct {
		name string
		edit func(*EventInput)
		op   string
		want string
	}{
		{"assinatura inexistente", func(in *EventInput) { in.SubscriptionID = uuid.Nil }, "SubscriptionForShare", "Assinatura não encontrada."},
		{"métrica inexistente", func(in *EventInput) { in.MetricCode = "inexistente" }, "MetricByCode", "Métrica não encontrada."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			a, b := e.input(), e.input()
			b.ExternalID = "evt-2"
			tc.edit(&a)
			tc.edit(&b)
			for _, r := range e.ingest(t, a, b) {
				if r.Outcome != OutcomeRejected || r.Error != tc.want {
					t.Fatalf("resultado %+v, esperava rejeição %q", r, tc.want)
				}
			}
			if e.store.calls[tc.op] != 1 {
				t.Fatalf("%s chamado %d vezes, esperava 1 (ausência em cache)", tc.op, e.store.calls[tc.op])
			}
		})
	}
}

// --- Ingest: deduplicação ---

func TestIngestDeduplicatesWithinBatch(t *testing.T) {
	t.Run("mesmo external_id repetido", func(t *testing.T) {
		e := newEnv()
		res := e.ingest(t, e.input(), e.input())
		assertOutcome(t, res[0], OutcomeAccepted)
		assertOutcome(t, res[1], OutcomeDuplicate)
		if *res[1].EventID != *res[0].EventID || res[1].ExternalID != "evt-1" {
			t.Fatalf("duplicata deve apontar o evento original: %+v e %+v", res[0], res[1])
		}
		if len(e.st().events) != 1 || e.store.calls["InsertEvent"] != 1 {
			t.Fatalf("esperava uma única gravação: %d eventos, %d inserções", len(e.st().events), e.store.calls["InsertEvent"])
		}
	})
	t.Run("repetição de evento rejeitado mantém a rejeição", func(t *testing.T) {
		e := newEnv()
		bad := e.input()
		bad.Quantity = "abc"
		res := e.ingest(t, bad, bad)
		for i, r := range res {
			if r.Outcome != OutcomeRejected || r.Error != msgDecimal || r.ExternalID != "evt-1" {
				t.Fatalf("evento %d: %+v, esperava rejeição %q", i, r, msgDecimal)
			}
		}
		if len(e.st().events) != 0 {
			t.Fatalf("nenhum evento deveria ser gravado, há %d", len(e.st().events))
		}
	})
	t.Run("external_id com espaços resolve pelo evento já gravado no lote", func(t *testing.T) {
		e := newEnv()
		padded := e.input()
		padded.ExternalID = " evt-1 "
		res := e.ingest(t, e.input(), padded)
		assertOutcome(t, res[0], OutcomeAccepted)
		assertOutcome(t, res[1], OutcomeDuplicate)
		if *res[1].EventID != *res[0].EventID {
			t.Fatalf("duplicata deve apontar o evento original: %s e %s", *res[0].EventID, *res[1].EventID)
		}
	})
}

// Os eventos do lote são gravados em ordem de external_id, para que dois
// lotes concorrentes com external_ids em comum esperem um pelo outro na mesma
// ordem (sem deadlock); cada resultado continua na posição do evento no lote
// e, entre repetições do mesmo external_id, vale a ordem do lote.
func TestIngestInsertsInExternalIDOrder(t *testing.T) {
	e := newEnv()
	ids := []string{"evt-c", " evt-a", "evt-b", "evt-a"}
	batch := make([]EventInput, len(ids))
	for i, id := range ids {
		batch[i] = e.input()
		batch[i].ExternalID = id
	}
	batch[3].Quantity = "99"
	res := e.ingest(t, batch...)
	if want := []string{"evt-a", "evt-b", "evt-c"}; !slices.Equal(e.store.inserts, want) {
		t.Fatalf("ordem das gravações = %v, esperada %v", e.store.inserts, want)
	}
	for i, want := range []string{"evt-c", "evt-a", "evt-b", "evt-a"} {
		if res[i].ExternalID != want {
			t.Errorf("resultado %d é de %q, esperado %q", i, res[i].ExternalID, want)
		}
	}
	assertOutcome(t, res[1], OutcomeAccepted)
	assertOutcome(t, res[3], OutcomeRejected)
	if res[3].Error != msgConflict {
		t.Errorf("repetição com outro conteúdo: %q, esperado %q", res[3].Error, msgConflict)
	}
}

func TestIngestWithinBatchDivergentContentIsRejected(t *testing.T) {
	e := newEnv()
	other := e.input()
	other.Quantity = "99"
	res := e.ingest(t, e.input(), other)
	assertOutcome(t, res[0], OutcomeAccepted)
	assertOutcome(t, res[1], OutcomeRejected)
	if res[1].Error != msgConflict {
		t.Fatalf("mensagem = %q, esperava %q", res[1].Error, msgConflict)
	}
}

func TestIngestDeduplicatesAgainstStore(t *testing.T) {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	cases := []struct {
		name          string
		first, resend func(*EventInput)
	}{
		{"mesmo conteúdo", nil, nil},
		{"quantidade com zeros à direita", nil, func(in *EventInput) { in.Quantity = "12.500000" }},
		{"occurred_at em outro fuso", nil, func(in *EventInput) { in.OccurredAt = occurredAt.In(loc) }},
		{"unique_key com espaços", func(in *EventInput) { in.UniqueKey = "user-7" }, func(in *EventInput) { in.UniqueKey = " user-7 " }},
		{"unidade omitida no reenvio", nil, func(in *EventInput) { in.Unit = "" }},
		{"properties ausentes e vazias", nil, func(in *EventInput) { in.Properties = map[string]any{} }},
		{"properties numéricas equivalentes",
			func(in *EventInput) { in.Properties = map[string]any{"n": 3, "tags": []string{"a"}} },
			func(in *EventInput) { in.Properties = map[string]any{"n": 3.0, "tags": []any{"a"}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			first := e.input()
			if tc.first != nil {
				tc.first(&first)
			}
			original := e.ingest(t, first)[0]
			assertOutcome(t, original, OutcomeAccepted)

			resend := first
			if tc.resend != nil {
				tc.resend(&resend)
			}
			e.clock.Advance(time.Hour)
			r := e.ingest(t, resend)[0]
			assertOutcome(t, r, OutcomeDuplicate)
			if *r.EventID != *original.EventID {
				t.Fatalf("duplicata aponta %s, esperava %s", *r.EventID, *original.EventID)
			}
			if len(e.st().events) != 1 || e.store.calls["InsertEvent"] != 1 {
				t.Fatalf("reenvio não deve gravar: %d eventos, %d inserções", len(e.st().events), e.store.calls["InsertEvent"])
			}
		})
	}
}

func TestIngestRejectsDivergentContent(t *testing.T) {
	cases := []struct {
		name   string
		stored func(*Event)
	}{
		{"quantidade", func(ev *Event) { ev.Quantity = big.NewRat(13, 1) }},
		{"occurred_at", func(ev *Event) { ev.OccurredAt = ev.OccurredAt.Add(time.Second) }},
		{"unique_key", func(ev *Event) { ev.UniqueKey = "outra-chave" }},
		{"properties", func(ev *Event) { ev.Properties = map[string]any{"region": "us-east-1"} }},
		{"assinatura", func(ev *Event) { ev.SubscriptionID = uuid.New() }},
		{"métrica", func(ev *Event) { ev.MetricID = uuid.New() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			in := e.input()
			assertOutcome(t, e.ingest(t, in)[0], OutcomeAccepted)
			ev := e.st().events[in.ExternalID]
			tc.stored(&ev)
			e.st().events[in.ExternalID] = ev

			r := e.ingest(t, in)[0]
			assertOutcome(t, r, OutcomeRejected)
			if r.Error != msgConflict {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, msgConflict)
			}
			if e.store.calls["InsertEvent"] != 1 {
				t.Fatalf("conteúdo divergente não deve tentar gravar: %d inserções", e.store.calls["InsertEvent"])
			}
		})
	}
}

func TestIngestRetransmissionWithSubMicrosecondOccurredAt(t *testing.T) {
	e := newEnv()
	in := e.input()
	in.OccurredAt = occurredAt.Add(123456789 * time.Nanosecond)
	original := e.ingest(t, in)[0]
	assertOutcome(t, original, OutcomeAccepted)
	r := e.ingest(t, in)[0]
	assertOutcome(t, r, OutcomeDuplicate)
	if *r.EventID != *original.EventID {
		t.Fatalf("duplicata aponta %s, esperava %s", *r.EventID, *original.EventID)
	}
}

func TestIngestRetransmissionAfterPriceChangeIsDuplicate(t *testing.T) {
	e := newEnv()
	e.clock.Set(lateAt.Add(time.Hour))
	in := e.input()
	in.OccurredAt = lateAt
	original := e.ingest(t, in)[0]
	assertOutcome(t, original, OutcomeAccepted)

	// Na virada do ciclo o motor fecha o período e aplica a mudança de plano.
	flat := catalog.PriceVersion{ID: uuid.New(), TenantID: e.tenantID, PlanID: uuid.New(), Version: 1, Model: catalog.Flat,
		FlatAmount: new(money.New(5000, money.BRL)), Currency: money.BRL}
	e.st().prices[flat.ID] = flat
	e.editSub(func(s *subscriptions.Subscription) { s.PriceVersionID, s.PlanID = flat.ID, flat.PlanID })
	e.closePeriod(p1Start)
	e.clock.Set(p2Start.Add(time.Hour))

	r := e.ingest(t, in)[0]
	assertOutcome(t, r, OutcomeDuplicate)
	if *r.EventID != *original.EventID {
		t.Fatalf("duplicata aponta %s, esperava %s", *r.EventID, *original.EventID)
	}
}

// --- Ingest: períodos fechados e atraso ---

func TestIngestClosedPeriod(t *testing.T) {
	ended := time.Date(2026, time.February, 25, 12, 0, 0, 0, time.UTC)
	grace := 72 * time.Hour
	cases := []struct {
		name   string
		setup  func(*env)
		closed bool
		now    time.Time
		want   Outcome
	}{
		{"período aberto aceita mesmo após a tolerância", nil, false, p2Start.Add(30 * 24 * time.Hour), OutcomeAccepted},
		{"logo após o fechamento", nil, true, p2Start.Add(time.Hour), OutcomeLate},
		{"no limite da tolerância", nil, true, p2Start.Add(grace), OutcomeLate},
		{"após a tolerância", nil, true, p2Start.Add(grace + time.Nanosecond), OutcomeRejected},
		{"tolerância zero no fim do período", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.LateGraceHours = 0 })
		}, true, p2Start, OutcomeLate},
		{"tolerância zero após o fim do período", func(e *env) {
			e.editMetric(func(m *catalog.Metric) { m.LateGraceHours = 0 })
		}, true, p2Start.Add(time.Nanosecond), OutcomeRejected},
		{"encerramento antecipa o fim do prazo", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}, true, ended.Add(grace + time.Nanosecond), OutcomeRejected},
		{"encerramento dentro da tolerância", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}, true, ended.Add(grace), OutcomeLate},
		{"encerramento após o fim do período não altera o prazo", func(e *env) {
			later := p2Start.Add(24 * time.Hour)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &later })
		}, true, p2Start.Add(grace), OutcomeLate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.setup != nil {
				tc.setup(e)
			}
			if tc.closed {
				e.closePeriod(p1Start)
			}
			e.clock.Set(tc.now)
			in := e.input()
			in.OccurredAt = lateAt
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			ev, stored := e.st().events[in.ExternalID]
			switch tc.want {
			case OutcomeRejected:
				if r.Error != msgOutOfGrace || stored {
					t.Fatalf("esperava rejeição por tolerância sem gravação: %+v, gravado %v", r, stored)
				}
			case OutcomeLate:
				if !stored || ev.Status != Late || !ev.PeriodStart.Equal(p1Start) {
					t.Fatalf("evento atrasado gravado incorretamente: %+v", ev)
				}
			default:
				if !stored || ev.Status != Accepted || !ev.PeriodStart.Equal(p1Start) {
					t.Fatalf("evento aceito gravado incorretamente: %+v", ev)
				}
			}
		})
	}
}

func TestIngestLateRetransmissionAfterGraceIsDuplicate(t *testing.T) {
	e := newEnv()
	e.closePeriod(p1Start)
	e.clock.Set(p2Start.Add(time.Hour))
	in := e.input()
	in.OccurredAt = lateAt
	original := e.ingest(t, in)[0]
	assertOutcome(t, original, OutcomeLate)

	e.clock.Set(p2Start.Add(30 * 24 * time.Hour))
	r := e.ingest(t, in)[0]
	assertOutcome(t, r, OutcomeDuplicate)
	if *r.EventID != *original.EventID {
		t.Fatalf("duplicata aponta %s, esperava %s", *r.EventID, *original.EventID)
	}
}

func TestIngestLateEventAfterPlanChangeAtCycleBoundary(t *testing.T) {
	e := newEnv()
	flat := catalog.PriceVersion{ID: uuid.New(), TenantID: e.tenantID, PlanID: uuid.New(), Version: 1, Model: catalog.Flat,
		FlatAmount: new(money.New(5000, money.BRL)), Currency: money.BRL}
	e.st().prices[flat.ID] = flat
	e.editSub(func(s *subscriptions.Subscription) { s.PriceVersionID, s.PlanID = flat.ID, flat.PlanID })
	e.closePeriod(p1Start)
	e.clock.Set(p2Start.Add(time.Hour))

	in := e.input()
	in.OccurredAt = lateAt
	assertOutcome(t, e.ingest(t, in)[0], OutcomeLate)
}

// --- Ingest: inserção concorrente ---

func TestIngestConcurrentInsert(t *testing.T) {
	cases := []struct {
		name     string
		quantity *big.Rat
		want     Outcome
	}{
		{"mesmo conteúdo vira duplicata", big.NewRat(25, 2), OutcomeDuplicate},
		{"conteúdo diferente é rejeitado", big.NewRat(7, 1), OutcomeRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			in := e.input()
			other := Event{ID: uuid.New(), TenantID: e.tenantID, ExternalID: in.ExternalID, CustomerID: e.sub.CustomerID,
				SubscriptionID: e.sub.ID, MetricID: e.metric.ID, Quantity: tc.quantity, OccurredAt: occurredAt,
				ReceivedAt: testNow, PeriodStart: p2Start, Status: Accepted}
			e.store.concurrent[in.ExternalID] = other

			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			if tc.want == OutcomeDuplicate && *r.EventID != other.ID {
				t.Fatalf("duplicata aponta %s, esperava o evento concorrente %s", *r.EventID, other.ID)
			}
			if tc.want == OutcomeRejected && r.Error != msgConflict {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, msgConflict)
			}
			if e.store.calls["EventByExternalID"] != 2 {
				t.Fatalf("esperava nova consulta após a inserção recusada, houve %d", e.store.calls["EventByExternalID"])
			}
			if got := e.st().events[in.ExternalID].ID; got != other.ID {
				t.Fatalf("evento concorrente substituído: %s", got)
			}
		})
	}
}

// --- Ingest: falhas de infraestrutura ---

func TestIngestInfrastructureErrorRollsBackBatch(t *testing.T) {
	cases := []struct {
		name string
		op   string
		call int
	}{
		{"abertura da transação", "InTenant", 0},
		{"consulta da assinatura", "SubscriptionForShare", 0},
		{"consulta do cliente", "CustomerAnonymized", 0},
		{"consulta do histórico de suspensões", "SuspensionHistory", 0},
		{"consulta da métrica", "MetricByCode", 0},
		{"consulta da mudança agendada", "PendingChange", 0},
		{"consulta do preço", "PriceVersion", 0},
		{"consulta por external_id", "EventByExternalID", 2},
		{"consulta de fechamento", "PeriodClosed", 2},
		{"inserção", "InsertEvent", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			e.store.fail[tc.op] = failure{err: errBoom, call: tc.call}
			first, second := e.input(), e.input()
			first.ExternalID = "evt-0"
			res, err := e.svc.Ingest(context.Background(), e.tenantID, []EventInput{first, second})
			if !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperava a falha simulada", err)
			}
			if res != nil {
				t.Fatalf("falha de infraestrutura não deve devolver resultados: %+v", res)
			}
			if n := len(e.st().events); n != 0 {
				t.Fatalf("lote deveria ser desfeito, há %d eventos gravados", n)
			}
		})
	}
	t.Run("consulta após inserção recusada", func(t *testing.T) {
		e := newEnv()
		in := e.input()
		e.store.concurrent[in.ExternalID] = Event{ID: uuid.New(), ExternalID: in.ExternalID}
		e.store.fail["EventByExternalID"] = failure{err: errBoom, call: 2}
		if _, err := e.svc.Ingest(context.Background(), e.tenantID, []EventInput{in}); !errors.Is(err, errBoom) {
			t.Fatalf("erro = %v, esperava a falha simulada", err)
		}
	})
	t.Run("versão de preço inexistente", func(t *testing.T) {
		e := newEnv()
		delete(e.st().prices, e.price.ID)
		_, err := e.svc.Ingest(context.Background(), e.tenantID, []EventInput{e.input()})
		assertAppErr(t, err, apperr.KindNotFound, "price_version_not_found")
	})
}

func TestIngestIsolatesTenants(t *testing.T) {
	e := newEnv()
	other := uuid.New()
	res, err := e.svc.Ingest(context.Background(), other, []EventInput{e.input()})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res[0].Outcome != OutcomeRejected || res[0].Error != "Assinatura não encontrada." {
		t.Fatalf("assinatura de outro tenant deveria ser desconhecida: %+v", res[0])
	}
	if len(e.st().events) != 0 || len(e.store.state(other).events) != 0 {
		t.Fatal("nenhum evento deveria ser gravado")
	}
}

// --- List ---

func TestList(t *testing.T) {
	e := newEnv()
	second := e.sub
	second.ID = uuid.New()
	e.st().subs[second.ID] = second
	for i, subID := range []uuid.UUID{e.sub.ID, e.sub.ID, second.ID} {
		in := e.input()
		in.ExternalID = fmt.Sprintf("evt-%d", i)
		in.SubscriptionID = subID
		assertOutcome(t, e.ingest(t, in)[0], OutcomeAccepted)
		e.clock.Advance(time.Minute)
	}

	f := ListFilter{SubscriptionID: &e.sub.ID, MetricID: &e.metric.ID, Limit: 1, Cursor: "cursor-anterior"}
	page, err := e.svc.List(context.Background(), e.tenantID, f)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := e.store.lastFilter; got == nil || *got.SubscriptionID != e.sub.ID || *got.MetricID != e.metric.ID ||
		got.Limit != 1 || got.Cursor != "cursor-anterior" {
		t.Fatalf("filtro repassado incorretamente: %+v", got)
	}
	if len(page.Items) != 1 || page.Items[0].ExternalID != "evt-1" || page.NextCursor == "" {
		t.Fatalf("página = %+v, esperava o evento mais recente da assinatura e cursor seguinte", page)
	}

	all, err := e.svc.List(context.Background(), e.tenantID, ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all.Items) != 3 || all.Items[0].ExternalID != "evt-2" || all.NextCursor != "" {
		t.Fatalf("listagem completa = %+v", all)
	}

	empty, err := e.svc.List(context.Background(), uuid.New(), ListFilter{})
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("outro tenant não deveria ver eventos: %+v, %v", empty, err)
	}
}

func TestListErrors(t *testing.T) {
	for _, op := range []string{"InTenant", "ListEvents"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv()
			e.store.fail[op] = failure{err: errBoom}
			if _, err := e.svc.List(context.Background(), e.tenantID, ListFilter{Limit: 10}); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperava a falha simulada", err)
			}
		})
	}
}

// --- CurrentUsage ---

func TestCurrentUsageEstimates(t *testing.T) {
	overage := func(e *env) {
		e.editPrice(func(p *catalog.PriceVersion) {
			p.Model, p.FlatAmount, p.UnitAmount, p.IncludedQuantity = catalog.FlatPlusOverage, new(money.New(1000, money.BRL)),
				big.NewRat(3, 1), big.NewRat(20, 1)
		})
	}
	cases := []struct {
		name       string
		setup      func(*env)
		quantities []string
		wantQty    string
		wantAmount int64
	}{
		{"sem eventos", nil, nil, "0", 0},
		{"meio centavo arredonda para o par inferior", nil, []string{"10", "15"}, "25", 12},
		{"meio centavo arredonda para o par superior", nil, []string{"20", "7"}, "27", 14},
		{"fração abaixo de meio centavo", nil, []string{"0.000001"}, "0.000001", 0},
		{"excedente acima da franquia", overage, []string{"15", "10"}, "25", 15},
		{"franquia não atingida", overage, []string{"5"}, "5", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.setup != nil {
				tc.setup(e)
			}
			batch := []EventInput{}
			for i, q := range tc.quantities {
				in := e.input()
				in.ExternalID, in.Quantity = fmt.Sprintf("evt-%d", i), q
				batch = append(batch, in)
			}
			// Consumo do período anterior não entra na estimativa.
			prev := e.input()
			prev.ExternalID, prev.Quantity, prev.OccurredAt = "evt-anterior", "500", lateAt
			batch = append(batch, prev)
			e.ingest(t, batch...)

			u, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID)
			if err != nil {
				t.Fatalf("CurrentUsage: %v", err)
			}
			if !u.Period.Start.Equal(p2Start) || !u.Period.End.Equal(p2End) {
				t.Fatalf("período = %v, esperava [%v, %v)", u.Period, p2Start, p2End)
			}
			if len(u.Metrics) != 1 {
				t.Fatalf("esperava uma métrica, obteve %d", len(u.Metrics))
			}
			m := u.Metrics[0]
			switch {
			case m.Metric.ID != e.metric.ID:
				t.Fatalf("métrica = %s, esperava %s", m.Metric.ID, e.metric.ID)
			case m.Quantity.Cmp(ratOf(t, tc.wantQty)) != 0:
				t.Fatalf("quantidade = %s, esperava %s", m.Quantity.RatString(), tc.wantQty)
			case m.EventCount != len(tc.quantities):
				t.Fatalf("eventos = %d, esperava %d", m.EventCount, len(tc.quantities))
			case m.EstimatedAmount.Amount() != tc.wantAmount || m.EstimatedAmount.Currency() != money.BRL:
				t.Fatalf("valor estimado = %v, esperava %d centavos em BRL", m.EstimatedAmount, tc.wantAmount)
			}
		})
	}
}

func TestCurrentUsagePeriod(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*env)
		wantStart time.Time
		wantEnd   time.Time
	}{
		{"período em andamento", nil, p2Start, p2End},
		{"encerramento no início do período corrente usa o período anterior", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &p2Start })
		}, p1Start, p2Start},
		{"encerramento no meio do período anterior", func(e *env) {
			ended := lateAt.Add(24 * time.Hour)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}, p1Start, p2Start},
		{"encerramento programado no futuro não altera o período", func(e *env) {
			ended := testNow.Add(24 * time.Hour)
			e.editSub(func(s *subscriptions.Subscription) { s.EndedAt = &ended })
		}, p2Start, p2End},
		{"trial antes da âncora usa o primeiro período", func(e *env) {
			trialEnd := time.Date(2026, time.March, 20, 3, 0, 0, 0, time.UTC)
			e.editSub(func(s *subscriptions.Subscription) {
				s.Status, s.TrialEndsAt, s.StartedAt = subscriptions.Trialing, &trialEnd, time.Date(2026, time.March, 13, 13, 0, 0, 0, time.UTC)
				s.Anchor = calendar.Date{Year: 2026, Month: time.March, Day: 20}
			})
		}, time.Date(2026, time.March, 20, 3, 0, 0, 0, time.UTC), time.Date(2026, time.April, 20, 3, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.setup != nil {
				tc.setup(e)
			}
			u, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID)
			if err != nil {
				t.Fatalf("CurrentUsage: %v", err)
			}
			if !u.Period.Start.Equal(tc.wantStart) || !u.Period.End.Equal(tc.wantEnd) {
				t.Fatalf("período = [%v, %v), esperava [%v, %v)", u.Period.Start, u.Period.End, tc.wantStart, tc.wantEnd)
			}
			if len(e.store.aggregated) != 1 || !e.store.aggregated[0].Equal(tc.wantStart) {
				t.Fatalf("agregação consultada em %v, esperava %v", e.store.aggregated, tc.wantStart)
			}
		})
	}
}

func TestCurrentUsageWithoutUsagePrice(t *testing.T) {
	cases := []struct {
		name string
		edit func(*catalog.PriceVersion)
	}{
		{"preço fixo", func(p *catalog.PriceVersion) { p.Model, p.FlatAmount = catalog.Flat, new(money.New(1000, money.BRL)) }},
		{"preço de consumo sem métrica", func(p *catalog.PriceVersion) { p.MetricID = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			e.editPrice(tc.edit)
			u, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID)
			if err != nil {
				t.Fatalf("CurrentUsage: %v", err)
			}
			if u.Metrics == nil || len(u.Metrics) != 0 {
				t.Fatalf("esperava lista de métricas vazia e não nula, obteve %#v", u.Metrics)
			}
			if !u.Period.Start.Equal(p2Start) {
				t.Fatalf("período = %v, esperava início em %v", u.Period, p2Start)
			}
			if e.store.calls["Metric"] != 0 || e.store.calls["Aggregate"] != 0 {
				t.Fatalf("sem consumo não deve consultar métrica nem agregado: %v", e.store.calls)
			}
		})
	}
}

func TestCurrentUsageErrors(t *testing.T) {
	t.Run("assinatura inexistente", func(t *testing.T) {
		e := newEnv()
		_, err := e.svc.CurrentUsage(context.Background(), e.tenantID, uuid.New())
		assertAppErr(t, err, apperr.KindNotFound, "subscription_not_found")
	})
	t.Run("assinatura de outro tenant", func(t *testing.T) {
		e := newEnv()
		_, err := e.svc.CurrentUsage(context.Background(), uuid.New(), e.sub.ID)
		assertAppErr(t, err, apperr.KindNotFound, "subscription_not_found")
	})
	t.Run("métrica inexistente", func(t *testing.T) {
		e := newEnv()
		delete(e.st().metrics, e.metric.ID)
		_, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID)
		assertAppErr(t, err, apperr.KindNotFound, "metric_not_found")
	})
	t.Run("fuso inválido", func(t *testing.T) {
		e := newEnv()
		e.editSub(func(s *subscriptions.Subscription) { s.Timezone = "Invalido/Fuso" })
		if _, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID); err == nil {
			t.Fatal("esperava erro de calendário")
		}
	})
	t.Run("valor estimado acima do limite", func(t *testing.T) {
		e := newEnv()
		e.editPrice(func(p *catalog.PriceVersion) { p.UnitAmount = big.NewRat(money.MaxAmount, 1) })
		e.st().events["evt-grande"] = Event{ID: uuid.New(), ExternalID: "evt-grande", SubscriptionID: e.sub.ID, MetricID: e.metric.ID,
			Quantity: big.NewRat(1_000_000, 1), PeriodStart: p2Start, Status: Accepted}
		if _, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("erro = %v, esperava money.ErrOverflow", err)
		}
	})
	for _, op := range []string{"InTenant", "Subscription", "PendingChange", "PriceVersion", "Metric", "Aggregate"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newEnv()
			e.store.fail[op] = failure{err: errBoom}
			if _, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID); !errors.Is(err, errBoom) {
				t.Fatalf("erro = %v, esperava a falha simulada", err)
			}
		})
	}
}

// --- Auxiliares ---

func TestWithinScale(t *testing.T) {
	cases := []struct {
		value string
		scale int
		want  bool
	}{
		{"0", 0, true},
		{"7", 0, true},
		{"1.5", 0, false},
		{"1.5", 1, true},
		{"0.000001", 6, true},
		{"0.0000001", 6, false},
		{"1.0000001", 6, false},
		{"-0.25", 2, true},
		{"-0.125", 2, false},
		{"99999999999999.999999", 6, true},
	}
	for _, tc := range cases {
		if got := withinScale(ratOf(t, tc.value), tc.scale); got != tc.want {
			t.Errorf("withinScale(%s, %d) = %v, esperava %v", tc.value, tc.scale, got, tc.want)
		}
	}
	if withinScale(big.NewRat(1, 3), MaxScale) {
		t.Error("1/3 não tem representação decimal finita")
	}
}

func TestNormalizeProperties(t *testing.T) {
	if normalize(nil) != nil || normalize(map[string]any{}) != nil {
		t.Fatal("properties ausentes e vazias devem ser equivalentes")
	}
	unsupported := map[string]any{"valor": math.NaN()}
	got, ok := normalize(unsupported).(map[string]any)
	if !ok || len(got) != 1 {
		t.Fatalf("properties não serializáveis devem ser devolvidas sem conversão, obteve %#v", got)
	}
	a := normalize(map[string]any{"n": 1, "nested": map[string]int{"x": 2}})
	b := normalize(map[string]any{"n": 1.0, "nested": map[string]any{"x": 2.0}})
	ea := Event{Quantity: big.NewRat(1, 1), Properties: map[string]any{"n": 1, "nested": map[string]int{"x": 2}}}
	eb := Event{Quantity: big.NewRat(1, 1), Properties: map[string]any{"n": 1.0, "nested": map[string]any{"x": 2.0}}}
	if a == nil || b == nil || !ea.sameContent(eb) {
		t.Fatalf("properties equivalentes após serialização devem ser iguais: %#v e %#v", a, b)
	}
}
