package subscriptions

import (
	"context"
	"errors"
	"slices"
	"testing"

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
