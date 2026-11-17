package subscriptions

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/catalog"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

func TestServiceCancelCancelsPendingTerms(t *testing.T) {
	cases := []struct {
		name string
		mode CancelMode
	}{
		{"cancelamento imediato", CancelImmediately},
		{"cancelamento ao fim do período", CancelAtPeriodEnd},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t, catalog.Flat)
			sub := f.create(t)
			if _, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, c.mode, "", 0); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.store.termsCanceled, []uuid.UUID{sub.ID}) {
				t.Fatalf("mudanças e reajustes pendentes devem ser cancelados: %v", f.store.termsCanceled)
			}
		})
	}

	t.Run("regra de domínio impede o cancelamento das condições", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		sub := f.create(t)
		if _, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelAtPeriodEnd, "", 0); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelAtPeriodEnd, "", 0)
		wantKind(t, err, apperr.KindConflict, "already_scheduled")
		if len(f.store.termsCanceled) != 1 {
			t.Fatalf("cancelamento rejeitado não pode cancelar condições: %v", f.store.termsCanceled)
		}
	})

	t.Run("falha ao cancelar as condições desfaz o cancelamento", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		sub := f.create(t)
		injected := errors.New("condições indisponíveis")
		f.store.errs["CancelPendingTerms"] = injected
		_, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelImmediately, "", 0)
		if !errors.Is(err, injected) {
			t.Fatalf("esperava a falha injetada, obtive %v", err)
		}
		stored := f.store.subs[sub.ID]
		if stored.Status != Active || stored.Version != sub.Version || f.store.updates != 0 ||
			len(f.store.histories) != 0 || len(f.store.events) != 0 || len(f.store.audits) != 0 {
			t.Fatalf("falha deve desfazer a transação: %+v", stored)
		}
	})
}

// Itens pendentes em aberto (por exemplo, correção de consumo para a próxima
// fatura) precisam ser faturados no fechamento: o cancelamento imediato agenda
// o passo final mesmo em plano sem consumo.
func TestServiceCancelImmediatelySchedulesFinalStepForPendingItems(t *testing.T) {
	cases := []struct {
		name       string
		pending    bool
		wantFinal  bool
		wantChecks int
	}{
		{"com itens pendentes", true, true, 1},
		{"sem itens pendentes", false, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFixture(t, catalog.Flat)
			sub := f.create(t)
			f.store.pendingItems[sub.ID] = c.pending
			f.clock.Advance(2 * time.Hour)
			got, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelImmediately, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			if f.store.pendingChecks != c.wantChecks {
				t.Fatalf("consultas de itens pendentes: %d", f.store.pendingChecks)
			}
			if (got.NextBillingAt != nil) != c.wantFinal {
				t.Fatalf("passo final agendado: %v", got.NextBillingAt)
			}
			if c.wantFinal && !got.NextBillingAt.Equal(*got.EndedAt) {
				t.Fatalf("o passo final vale no encerramento: %v %v", got.NextBillingAt, got.EndedAt)
			}
			step, ok, err := got.NextStep(f.clock.Now())
			if err != nil || ok != c.wantFinal || (ok && (step.Kind != StepFinalUsage || step.UsagePeriod != nil)) {
				t.Fatalf("passo final sem período de consumo: %+v %v %v", step, ok, err)
			}
		})
	}

	t.Run("cancelamento ao fim do período não consulta itens pendentes", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		sub := f.create(t)
		if _, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelAtPeriodEnd, "", 0); err != nil {
			t.Fatal(err)
		}
		if f.store.pendingChecks != 0 {
			t.Fatal("o passo de encerramento já fatura os itens pendentes")
		}
	})

	t.Run("falha na consulta desfaz o cancelamento", func(t *testing.T) {
		f := newServiceFixture(t, catalog.Flat)
		sub := f.create(t)
		injected := errors.New("itens pendentes indisponíveis")
		f.store.errs["HasOpenPendingItems"] = injected
		if _, err := f.svc.Cancel(context.Background(), f.tenantID, f.actor, sub.ID, CancelImmediately, "", 0); !errors.Is(err, injected) {
			t.Fatalf("esperava a falha injetada, obtive %v", err)
		}
		if stored := f.store.subs[sub.ID]; stored.Status != Active || f.store.updates != 0 || len(f.store.termsCanceled) != 0 {
			t.Fatalf("falha deve desfazer a transação: %+v", stored)
		}
	})
}
