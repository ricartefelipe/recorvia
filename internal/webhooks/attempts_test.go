package webhooks

import (
	"context"
	"errors"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// As tentativas individuais de uma entrega são devolvidas em ordem, com o
// código de resposta ou o erro de cada uma.
func TestDeliveryAttemptsListsEachAttemptInOrder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ep, _ := e.endpoint(t)
	e.publish(t, outbox.EventInvoicePaid)
	e.transport.responses = []func() (int, error){
		func() (int, error) { return 0, errors.New("conexão recusada") },
		func() (int, error) { return 503, nil },
		func() (int, error) { return 204, nil },
	}
	for range 3 {
		deliver(t, e)
		if d := e.store.deliveries()[0]; d.NextAttemptAt != nil {
			e.clock.Set(*d.NextAttemptAt)
		}
	}
	d := e.store.deliveries()[0]
	if d.Status != Succeeded {
		t.Fatalf("entrega: %+v", d)
	}
	got, err := e.svc.DeliveryAttempts(ctx, e.tenant, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("tentativas: %+v", got)
	}
	for i, a := range got {
		if a.Number != i+1 || a.DeliveryID != d.ID || a.SignatureCount != 1 {
			t.Fatalf("tentativa %d: %+v", i+1, a)
		}
	}
	if got[0].StatusCode != nil || got[0].Error != "conexão recusada" || *got[1].StatusCode != 503 || *got[2].StatusCode != 204 || got[2].Error != "" {
		t.Fatalf("resultado das tentativas: %+v %+v %+v", got[0], got[1], got[2])
	}

	t.Run("entrega inexistente", func(t *testing.T) {
		if _, err := e.svc.DeliveryAttempts(ctx, e.tenant, ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Fatalf("esperava 404: %v", err)
		}
	})
	t.Run("endpoint removido", func(t *testing.T) {
		if err := e.svc.DeleteEndpoint(ctx, e.tenant, e.actor, ep.ID, e.version(ep.ID)); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.DeliveryAttempts(ctx, e.tenant, d.ID)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != "webhook_delivery_not_found" {
			t.Fatalf("entrega de endpoint removido deve ser 404: %v", err)
		}
	})
	t.Run("falha na leitura", func(t *testing.T) {
		f := newEnv(t)
		f.endpoint(t)
		f.publish(t, outbox.EventInvoicePaid)
		f.store.fail["Attempts"] = errInjected
		if _, err := f.svc.DeliveryAttempts(ctx, f.tenant, f.store.deliveries()[0].ID); !errors.Is(err, errInjected) {
			t.Fatalf("erro deve propagar: %v", err)
		}
	})
}
