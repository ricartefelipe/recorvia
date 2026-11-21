//go:build integration

package pgstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

type customersTx = customers.Tx

func emitEvents(t *testing.T, env *fixture.Env, tenantID uuid.UUID, n int) {
	t.Helper()
	ctx := context.Background()
	err := env.Store.Customers().InTenant(ctx, tenantID, func(tx customers.Tx) error {
		for range n {
			if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventCustomerUpdated, "customer", uuid.New(), map[string]any{"n": 1}, now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Dois publicadores disputando a outbox recebem conjuntos disjuntos; um
// lease expirado devolve os eventos à disputa; só o dono do lease confirma.
func TestOutboxClaimIsExclusiveAndRecoverable(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "outbox")
	emitEvents(t, env, tn.ID, 40)
	var mu sync.Mutex
	seen := map[uuid.UUID]string{}
	var wg sync.WaitGroup
	for _, w := range []string{"w1", "w2"} {
		wg.Go(func() {
			for {
				recs, err := env.Store.ClaimOutbox(ctx, w, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(recs) == 0 {
					return
				}
				mu.Lock()
				for _, r := range recs {
					if prev, dup := seen[r.ID]; dup && r.TenantID == tn.ID {
						t.Errorf("evento %s reivindicado por %s e %s", r.ID, prev, w)
					}
					seen[r.ID] = w
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	mine := 0
	var someID uuid.UUID
	var owner string
	for id, w := range seen {
		mine++
		someID, owner = id, w
	}
	if mine < 40 {
		t.Fatalf("reivindicados %d de 40", mine)
	}
	other := "w1"
	if owner == "w1" {
		other = "w2"
	}
	if n, err := env.Store.MarkOutboxPublished(ctx, other, []uuid.UUID{someID}); err != nil || n != 0 {
		t.Fatalf("outro publicador confirmou evento alheio: n=%d err=%v", n, err)
	}
	// Lease expira (publicador caiu após publicar e antes de confirmar).
	err := env.DB.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app_outbox_release($1, $2, 0, 'simulação de queda')`, owner, someID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := env.Store.ClaimOutbox(ctx, "w3", 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range again {
		if r.ID == someID {
			found = true
		}
	}
	if !found {
		t.Fatal("evento liberado não voltou à disputa")
	}
	if n, err := env.Store.MarkOutboxPublished(ctx, "w3", []uuid.UUID{someID}); err != nil || n != 1 {
		t.Fatalf("novo dono deveria confirmar: n=%d err=%v", n, err)
	}
}

type flakyPublisher struct {
	fail      atomic.Bool
	published sync.Map
}

func (p *flakyPublisher) PublishEvent(_ context.Context, r outbox.Record) error {
	if p.fail.Load() {
		return errors.New("broker indisponível")
	}
	p.published.Store(r.ID, true)
	return nil
}

type relayAdapter struct{ s *pgstore.Store }

func (a relayAdapter) Claim(ctx context.Context, w string, n int, lease time.Duration) ([]outbox.Record, error) {
	recs, err := a.s.ClaimOutbox(ctx, w, n, lease)
	out := make([]outbox.Record, 0, len(recs))
	for _, x := range recs {
		out = append(out, outbox.Record{ID: x.ID, TenantID: x.TenantID, Type: x.EventType, Payload: x.Payload, Headers: x.Headers, Attempts: x.Attempts})
	}
	return out, err
}
func (a relayAdapter) MarkPublished(ctx context.Context, w string, ids []uuid.UUID) (int, error) {
	return a.s.MarkOutboxPublished(ctx, w, ids)
}
func (a relayAdapter) Release(ctx context.Context, w string, id uuid.UUID, d time.Duration, c string) error {
	return a.s.ReleaseOutbox(ctx, w, id, d, c)
}

type nopObs struct{}

func (nopObs) Published(int)  {}
func (nopObs) PublishFailed() {}

// Broker indisponível: nada é perdido; os eventos voltam a ser publicados
// quando o broker retorna.
func TestRelayKeepsEventsWhileBrokerIsDown(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "relay")
	emitEvents(t, env, tn.ID, 3)
	pub := &flakyPublisher{}
	pub.fail.Store(true)
	relay := outbox.NewRelay(relayAdapter{env.Store}, pub, "relay-test", 100, time.Minute, time.Second, discardLogger(), nopObs{})
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pending := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND published_at IS NULL`, tn.ID)
	if pending == 0 {
		t.Fatal("eventos marcados como publicados com o broker indisponível")
	}
	// Volta do broker: torna os eventos elegíveis de imediato e republica.
	err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	owner := envOwner(t)
	if _, err := owner.Exec(ctx, `UPDATE outbox_events SET available_at = now() WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	pub.fail.Store(false)
	for range 5 {
		if _, err := relay.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND published_at IS NULL`, tn.ID); n != 0 {
		t.Fatalf("eventos pendentes após retorno do broker: %d", n)
	}
}

// Requisições concorrentes com a mesma Idempotency-Key: apenas uma vence.
func TestIdempotencyKeyConcurrentBegin(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 16, now)
	tn := env.NewTenant(t, "idem")
	k := pgstore.IdempotencyKey{TenantID: tn.ID, ClientID: "user:x", Operation: "POST /api/v1/billing-runs", Key: "chave-concorrente-01"}
	hash := sha256.Sum256([]byte("corpo"))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, won, err := env.Store.IdempotencyBegin(ctx, k, hash[:], time.Minute, time.Hour)
			if err != nil {
				t.Error(err)
			}
			if won {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("vencedores = %d, esperado 1", wins.Load())
	}
	// Lease ainda válido: ninguém assume.
	if ok, err := env.Store.IdempotencyTakeOver(ctx, k, time.Minute); err != nil || ok {
		t.Fatalf("assumiu chave com lease válido: %v %v", ok, err)
	}
	if err := env.Store.IdempotencyComplete(ctx, k, 200, []byte(`{"ok":true}`), "application/json", ""); err != nil {
		t.Fatal(err)
	}
	rec, won, err := env.Store.IdempotencyBegin(ctx, k, hash[:], time.Minute, time.Hour)
	if err != nil || won || !rec.Completed || string(rec.ResponseBody) != `{"ok":true}` || rec.ResponseLocation != "" {
		t.Fatalf("repetição deveria devolver o resultado registrado: %+v won=%v err=%v", rec, won, err)
	}
}

// O resultado registrado guarda o header Location da resposta, que a
// repetição reproduz; a resposta sem o header fica com o campo vazio, e um
// endereço acima do limite da coluna não impede o registro do resultado.
func TestIdempotencyKeyKeepsResponseLocation(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "idem-loc")
	hash := sha256.Sum256([]byte("corpo"))
	cases := []struct {
		name, location, want string
	}{
		{"com endereço", "/api/v1/reconciliation/runs/0195a3f0-0000-7000-8000-000000000001", "/api/v1/reconciliation/runs/0195a3f0-0000-7000-8000-000000000001"},
		{"sem endereço", "", ""},
		{"no limite", "/" + strings.Repeat("a", 2047), "/" + strings.Repeat("a", 2047)},
		{"acima do limite", "/" + strings.Repeat("a", 2048), ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := pgstore.IdempotencyKey{TenantID: tn.ID, ClientID: "user:x", Operation: "POST /api/v1/reconciliation/runs",
				Key: fmt.Sprintf("chave-location-%02d", i)}
			if _, won, err := env.Store.IdempotencyBegin(ctx, k, hash[:], time.Minute, time.Hour); err != nil || !won {
				t.Fatalf("registro da chave: won=%v err=%v", won, err)
			}
			if err := env.Store.IdempotencyComplete(ctx, k, 200, []byte(`{"ok":true}`), "application/json", tc.location); err != nil {
				t.Fatal(err)
			}
			rec, won, err := env.Store.IdempotencyBegin(ctx, k, hash[:], time.Minute, time.Hour)
			if err != nil || won || !rec.Completed || rec.ResponseStatus != 200 || rec.ResponseCT != "application/json" ||
				string(rec.ResponseBody) != `{"ok":true}` || rec.ResponseLocation != tc.want {
				t.Fatalf("resultado registrado: %+v won=%v err=%v", rec, won, err)
			}
		})
	}
	// O banco recusa o endereço vazio ou acima do limite, mesmo fora do caso de uso.
	owner := envOwner(t)
	for _, loc := range []string{"", strings.Repeat("a", 2049)} {
		_, err := owner.Exec(ctx, `UPDATE idempotency_keys SET response_location = $2 WHERE tenant_id = $1`, tn.ID, loc)
		if !postgres.IsNamedCheckViolation(err, "idempotency_keys_response_location_length") {
			t.Fatalf("constraint do endereço (%d caracteres): %v", len(loc), err)
		}
	}
}
