//go:build integration

package pgstore_test

import (
	"context"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// A listagem de assinaturas devolve os nomes do cliente e do plano de cada
// item na própria página (lidos na consulta da lista), iguais aos da leitura
// por assinatura: a API não precisa de uma transação por item (N+1 medido
// no teste de carga, ADR-088), inclusive na página seguinte do cursor.
func TestSubscriptionListCarriesNames(t *testing.T) {
	ctx := context.Background()
	a := newAdvEnv(t, 4)
	tn := a.NewTenant(t, "lista-nomes")
	plans := []int64{9900, 29900}
	var created []subscriptions.Subscription
	for i := range 5 {
		plan := a.newFlatPlan(t, tn, plans[i%len(plans)])
		created = append(created, a.newSubscriptionQty(t, tn, plan.ID, 1))
	}
	seen := 0
	cursor := ""
	for range 3 {
		page, err := a.Subscriptions.List(ctx, tn.ID, subscriptions.ListFilter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Names) != len(page.Items) {
			t.Fatalf("nomes de %d itens numa página de %d", len(page.Names), len(page.Items))
		}
		for _, s := range page.Items {
			customer, plan, err := a.Store.Subscriptions().Names(ctx, tn.ID, s.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := page.Names[s.ID]
			if got.Customer == "" || got.Plan == "" || got.Customer != customer || got.Plan != plan {
				t.Errorf("assinatura %s: nomes na lista %+v, na leitura individual %q e %q", s.ID, got, customer, plan)
			}
			seen++
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if seen != len(created) {
		t.Errorf("itens percorridos = %d, esperado %d", seen, len(created))
	}
}
