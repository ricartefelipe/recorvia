package metering

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

const (
	msgSuspended       = "Assinatura suspensa: consumo não é faturável durante a suspensão."
	msgPropertiesNUL   = "properties não pode conter o caractere NUL (\\u0000)."
	msgBilledNoMetric  = "Período já faturado sem consumo desta métrica."
	msgInvalidTextTail = " contém caractere inválido (NUL ou UTF-8 inválido)."
)

// --- Ingest: external_id repetido no mesmo lote ---

func TestIngestWithinBatchRepeatedExternalID(t *testing.T) {
	t.Run("primeiro rejeitado não impede o seguinte válido", func(t *testing.T) {
		e := newEnv()
		bad := e.input()
		bad.Quantity = "abc"
		res := e.ingest(t, bad, e.input())
		assertOutcome(t, res[0], OutcomeRejected)
		if res[0].Error != msgDecimal {
			t.Fatalf("mensagem = %q, esperava %q", res[0].Error, msgDecimal)
		}
		assertOutcome(t, res[1], OutcomeAccepted)
		if len(e.st().events) != 1 {
			t.Fatalf("eventos gravados = %d, esperava 1", len(e.st().events))
		}
	})
	t.Run("repetição idêntica e repetição divergente", func(t *testing.T) {
		e := newEnv()
		other := e.input()
		other.Quantity = "5"
		res := e.ingest(t, e.input(), e.input(), other)
		assertOutcome(t, res[0], OutcomeAccepted)
		assertOutcome(t, res[1], OutcomeDuplicate)
		if *res[1].EventID != *res[0].EventID {
			t.Fatalf("duplicata aponta %s, esperava %s", *res[1].EventID, *res[0].EventID)
		}
		assertOutcome(t, res[2], OutcomeRejected)
		if res[2].Error != msgConflict {
			t.Fatalf("mensagem = %q, esperava %q", res[2].Error, msgConflict)
		}
		if ev := e.st().events["evt-1"]; ev.Quantity.Cmp(big.NewRat(25, 2)) != 0 {
			t.Fatalf("evento gravado alterado pela repetição divergente: %s", ev.Quantity.RatString())
		}
	})
	t.Run("instantes que diferem abaixo do microssegundo são o mesmo conteúdo", func(t *testing.T) {
		e := newEnv()
		a, b := e.input(), e.input()
		a.OccurredAt = occurredAt.Add(123456789 * time.Nanosecond)
		b.OccurredAt = occurredAt.Add(123456001 * time.Nanosecond)
		res := e.ingest(t, a, b)
		assertOutcome(t, res[0], OutcomeAccepted)
		assertOutcome(t, res[1], OutcomeDuplicate)
	})
}

// --- Ingest: deduplicação antes das validações de estado ---

func TestIngestRetransmissionIgnoresLaterStateChanges(t *testing.T) {
	cases := []struct {
		name   string
		change func(*env)
	}{
		{"métrica arquivada", func(e *env) { e.editMetric(func(m *catalog.Metric) { m.Status = catalog.Archived }) }},
		{"máximo por evento reduzido", func(e *env) { e.editMetric(func(m *catalog.Metric) { m.MaxQuantityPerEvent = big.NewRat(1, 1) }) }},
		{"preço sem a métrica", func(e *env) {
			e.editPrice(func(p *catalog.PriceVersion) {
				p.Model, p.MetricID, p.FlatAmount = catalog.Flat, nil, new(money.New(1000, money.BRL))
			})
		}},
		{"suspensão registrada antes do instante", func(e *env) {
			e.st().suspensions[e.sub.ID] = []SuspensionChange{{At: occurredAt.Add(-time.Hour), Suspended: true}}
		}},
		{"encerramento anterior ao instante", func(e *env) {
			ended := occurredAt.Add(-time.Hour)
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			in := e.input()
			original := e.ingest(t, in)[0]
			assertOutcome(t, original, OutcomeAccepted)
			tc.change(e)

			r := e.ingest(t, in)[0]
			assertOutcome(t, r, OutcomeDuplicate)
			if *r.EventID != *original.EventID {
				t.Fatalf("duplicata aponta %s, esperava %s", *r.EventID, *original.EventID)
			}
			changed := in
			changed.Quantity = "13"
			r = e.ingest(t, changed)[0]
			assertOutcome(t, r, OutcomeRejected)
			if r.Error != msgConflict {
				t.Fatalf("conteúdo diferente: mensagem = %q, esperava %q", r.Error, msgConflict)
			}
			if e.store.calls["InsertEvent"] != 1 {
				t.Fatalf("reenvios não devem gravar: %d inserções", e.store.calls["InsertEvent"])
			}
		})
	}
}

// --- Ingest: textos inválidos ---

func TestIngestRejectsInvalidTextPerEvent(t *testing.T) {
	cases := []struct {
		name string
		edit func(*EventInput)
		want string
	}{
		{"external_id com NUL", func(in *EventInput) { in.ExternalID = "evt\x00-1" }, "external_id" + msgInvalidTextTail},
		{"external_id com UTF-8 inválido", func(in *EventInput) { in.ExternalID = "evt-\xff" }, "external_id" + msgInvalidTextTail},
		{"metric_code com NUL", func(in *EventInput) { in.MetricCode = "api\x00requests" }, "metric_code" + msgInvalidTextTail},
		{"unit com NUL", func(in *EventInput) { in.Unit = "request\x00" }, "unit" + msgInvalidTextTail},
		{"unique_key com NUL", func(in *EventInput) { in.UniqueKey = "user\x007" }, "unique_key" + msgInvalidTextTail},
		{"valor de properties com NUL", func(in *EventInput) { in.Properties = map[string]any{"nota": "a\x00b"} }, msgPropertiesNUL},
		{"chave de properties com NUL", func(in *EventInput) { in.Properties = map[string]any{"a\x00": "b"} }, msgPropertiesNUL},
		{"NUL em lista de properties", func(in *EventInput) { in.Properties = map[string]any{"tags": []any{"ok", "x\x00"}} }, msgPropertiesNUL},
		{"NUL em objeto aninhado", func(in *EventInput) {
			in.Properties = map[string]any{"meta": map[string]any{"k": []string{"\x00"}}}
		}, msgPropertiesNUL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			bad, good := e.input(), e.input()
			bad.ExternalID = "evt-invalido"
			tc.edit(&bad)
			good.ExternalID = "evt-valido"
			res := e.ingest(t, bad, good)
			assertOutcome(t, res[0], OutcomeRejected)
			if res[0].Error != tc.want {
				t.Fatalf("mensagem = %q, esperava %q", res[0].Error, tc.want)
			}
			assertOutcome(t, res[1], OutcomeAccepted)
			if len(e.st().events) != 1 {
				t.Fatalf("eventos gravados = %d, esperava somente o válido", len(e.st().events))
			}
			// O texto inválido não chega a nenhuma consulta.
			if e.store.calls["MetricByCode"] != 1 || e.store.calls["EventByExternalID"] != 1 {
				t.Fatalf("consultas com o evento inválido: %v", e.store.calls)
			}
		})
	}
	t.Run("barra invertida literal seguida de u0000 é texto válido", func(t *testing.T) {
		e := newEnv()
		in := e.input()
		in.Properties = map[string]any{"nota": `\u0000`, "lista": []any{`a\u0000`}}
		assertOutcome(t, e.ingest(t, in)[0], OutcomeAccepted)
	})
}

func TestHasNUL(t *testing.T) {
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"sem escape", `{"a":"b"}`, false},
		{"NUL em valor", `{"a":"x\u0000"}`, true},
		{"NUL em chave", `{"\u0000":1}`, true},
		{"barra invertida literal", `{"a":"\\u0000"}`, false},
		{"número e booleano", `{"a":1,"b":true,"c":null,"d":"\\u0000"}`, false},
		{"serialização inválida com escape", `{"a":"\u0000`, true},
	}
	for _, tc := range cases {
		if got := hasNUL([]byte(tc.json)); got != tc.want {
			t.Errorf("%s: hasNUL(%s) = %v, esperava %v", tc.name, tc.json, got, tc.want)
		}
	}
}

// --- Ingest: suspensão pelo histórico ---

func TestIngestSuspensionHistory(t *testing.T) {
	suspended := occurredAt.Add(-2 * time.Hour)
	reactivated := occurredAt.Add(2 * time.Hour)
	window := []SuspensionChange{{At: suspended, Suspended: true}, {At: reactivated}}
	cases := []struct {
		name    string
		history []SuspensionChange
		setup   func(*env)
		at      time.Time
		want    Outcome
	}{
		{"durante a suspensão, recebido após a reativação", window, nil, occurredAt, OutcomeRejected},
		{"no instante da suspensão", window, nil, suspended, OutcomeRejected},
		{"um microssegundo antes da suspensão", window, nil, suspended.Add(-time.Microsecond), OutcomeAccepted},
		{"no instante da reativação", window, nil, reactivated, OutcomeAccepted},
		{"depois da reativação", window, nil, reactivated.Add(time.Hour), OutcomeAccepted},
		{"entre dois ciclos de suspensão", append(window, SuspensionChange{At: reactivated.Add(2 * time.Hour), Suspended: true},
			SuspensionChange{At: reactivated.Add(4 * time.Hour)}), nil, reactivated.Add(time.Hour), OutcomeAccepted},
		{"no segundo ciclo de suspensão", append(window, SuspensionChange{At: reactivated.Add(2 * time.Hour), Suspended: true},
			SuspensionChange{At: reactivated.Add(4 * time.Hour)}), nil, reactivated.Add(3 * time.Hour), OutcomeRejected},
		{"cancelada durante a suspensão", []SuspensionChange{{At: suspended, Suspended: true}}, func(e *env) {
			ended := reactivated
			e.editSub(func(s *subscriptions.Subscription) { s.Status, s.EndedAt = subscriptions.Canceled, &ended })
		}, occurredAt, OutcomeRejected},
		{"período fechado inteiro durante a suspensão", []SuspensionChange{{At: p1Start, Suspended: true}, {At: p2Start.Add(time.Hour)}},
			func(e *env) {
				e.closePeriod(p1Start)
				e.clock.Set(p2Start.Add(2 * time.Hour))
			}, lateAt, OutcomeRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			e.st().suspensions[e.sub.ID] = tc.history
			if tc.setup != nil {
				tc.setup(e)
			}
			in := e.input()
			in.OccurredAt = tc.at
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			if tc.want == OutcomeRejected && r.Error != msgSuspended {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, msgSuspended)
			}
		})
	}
}

func TestSuspendedIn(t *testing.T) {
	t0 := occurredAt
	cases := []struct {
		name    string
		history []SuspensionChange
		at      time.Time
		want    bool
	}{
		{"sem histórico", nil, t0, false},
		{"suspensão posterior ao instante", []SuspensionChange{{At: t0.Add(time.Second), Suspended: true}}, t0, false},
		{"lista fora de ordem", []SuspensionChange{{At: t0.Add(-time.Hour)}, {At: t0.Add(-2 * time.Hour), Suspended: true}}, t0, false},
		{"empate de instante vale o último registro", []SuspensionChange{{At: t0}, {At: t0, Suspended: true}}, t0, true},
		{"reativação no mesmo instante registrada depois", []SuspensionChange{{At: t0, Suspended: true}, {At: t0}}, t0, false},
	}
	for _, tc := range cases {
		if got := suspendedIn(tc.history, tc.at); got != tc.want {
			t.Errorf("%s: suspendedIn = %v, esperava %v", tc.name, got, tc.want)
		}
	}
}

// --- Ingest: métrica válida conforme o período do evento ---

func TestIngestMetricValidatedForEventPeriod(t *testing.T) {
	otherMetric := func(e *env) {
		other := catalog.Metric{ID: uuid.New(), TenantID: e.tenantID, Code: "storage.gb", Name: "Armazenamento", Unit: "gb",
			Aggregation: catalog.AggSum, LateGraceHours: 72, Status: catalog.Active}
		e.st().metrics[other.ID] = other
		e.editPrice(func(p *catalog.PriceVersion) { p.MetricID = &other.ID })
	}
	cases := []struct {
		name      string
		setup     func(*env)
		closed    bool
		at        time.Time
		want      Outcome
		wantError string
	}{
		{"período fechado com preço vigente de outra métrica", otherMetric, true, lateAt, OutcomeLate, ""},
		{"período aberto com preço vigente de outra métrica", otherMetric, false, occurredAt, OutcomeRejected, msgNotInPrice},
		{"período cujo fechamento passou sem agregado da métrica", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.NextBillingIndex = 3 })
		}, false, lateAt, OutcomeRejected, msgBilledNoMetric},
		{"período anterior ainda não fechado pelo faturamento", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.NextBillingIndex = 2 })
		}, false, lateAt, OutcomeAccepted, ""},
		{"período corrente depois do passo de cobrança", func(e *env) {
			e.editSub(func(s *subscriptions.Subscription) { s.NextBillingIndex = 3 })
		}, false, occurredAt, OutcomeAccepted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			tc.setup(e)
			if tc.closed {
				e.closePeriod(p1Start)
				e.clock.Set(p2Start.Add(time.Hour))
			}
			in := e.input()
			in.OccurredAt = tc.at
			r := e.ingest(t, in)[0]
			assertOutcome(t, r, tc.want)
			if r.Error != tc.wantError {
				t.Fatalf("mensagem = %q, esperava %q", r.Error, tc.wantError)
			}
			if tc.closed && e.store.calls["PriceVersion"] != 0 {
				t.Fatalf("período fechado não depende do preço vigente: %d consultas de preço", e.store.calls["PriceVersion"])
			}
		})
	}
}

// --- Ingest: precisão de occurred_at ---

func TestIngestTruncatesOccurredAtToMicroseconds(t *testing.T) {
	e := newEnv()
	in := e.input()
	in.OccurredAt = p2Start.Add(-time.Nanosecond)
	r := e.ingest(t, in)[0]
	assertOutcome(t, r, OutcomeAccepted)
	ev := e.st().events[in.ExternalID]
	if want := p2Start.Add(-time.Microsecond); !ev.OccurredAt.Equal(want) || !ev.PeriodStart.Equal(p1Start) {
		t.Fatalf("occurred_at = %v e período %v, esperava %v no período %v", ev.OccurredAt, ev.PeriodStart, want, p1Start)
	}
	again := e.ingest(t, in)[0]
	assertOutcome(t, again, OutcomeDuplicate)
}

// --- Ingest: bloqueio das assinaturas ---

func TestIngestLocksSubscriptionsInAscendingOrder(t *testing.T) {
	e := newEnv()
	subs := []uuid.UUID{e.sub.ID}
	for range 3 {
		other := e.sub
		other.ID = uuid.New()
		e.st().subs[other.ID] = other
		subs = append(subs, other.ID)
	}
	missing := uuid.New()
	var batch []EventInput
	for i, id := range append(subs, missing, subs[2], subs[0]) {
		in := e.input()
		in.ExternalID, in.SubscriptionID = fmt.Sprintf("evt-%d", i), id
		batch = append(batch, in)
	}
	res := e.ingest(t, batch...)
	for i, r := range res {
		want := OutcomeAccepted
		if batch[i].SubscriptionID == missing {
			want = OutcomeRejected
		}
		assertOutcome(t, r, want)
	}
	want := append(slices.Clone(subs), missing)
	slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(e.store.shareLocks, want) {
		t.Fatalf("ordem dos bloqueios = %v, esperava %v (uma vez cada, em ordem crescente)", e.store.shareLocks, want)
	}
}

// --- CurrentUsage: leitura sem bloqueio ---

func TestCurrentUsageReadsSubscriptionWithoutLock(t *testing.T) {
	e := newEnv()
	if _, err := e.svc.CurrentUsage(context.Background(), e.tenantID, e.sub.ID); err != nil {
		t.Fatalf("CurrentUsage: %v", err)
	}
	if e.store.calls["Subscription"] != 1 || e.store.calls["SubscriptionForShare"] != 0 {
		t.Fatalf("a consulta não deve bloquear a assinatura: %v", e.store.calls)
	}
}
