//go:build integration

package pgstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// Regressão: o registro do resultado e a liberação da chave não conferiam
// quem detinha o processamento. Uma execução antiga (A), cujo lease expirou e
// foi assumido por outra (B), podia gravar o próprio resultado por último (a
// chave repetiria a resposta de A por 24 h) ou apagar o processamento de B.
// Agora só o token da execução que detém a chave registra ou libera.
func TestIdempotencyCompleteAndReleaseRequireCurrentLease(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	tn := env.NewTenant(t, "idem-lease")
	base := pgstore.IdempotencyKey{TenantID: tn.ID, ClientID: "user:x", Operation: "POST /api/v1/subscriptions", Key: "chave-lease-" + ids.New().String()}
	hash := sha256.Sum256([]byte("corpo"))
	a, b := base, base
	a.Lease, b.Lease = ids.New(), ids.New()

	if _, won, err := env.Store.IdempotencyBegin(ctx, a, hash[:], time.Minute, time.Hour); err != nil || !won {
		t.Fatalf("A deveria vencer: %v %v", won, err)
	}
	env.Clock.Advance(2 * time.Minute)
	if ok, err := env.Store.IdempotencyTakeOver(ctx, b, time.Minute); err != nil || !ok {
		t.Fatalf("B deveria assumir o lease expirado: %v %v", ok, err)
	}

	// A termina depois de perder a chave: não registra nem libera.
	if err := env.Store.IdempotencyComplete(ctx, a, 409, []byte(`{"code":"a"}`), "application/problem+json", ""); !errors.Is(err, pgstore.ErrIdempotencyLeaseLost) {
		t.Fatalf("registro pela execução antiga: %v", err)
	}
	if err := env.Store.IdempotencyRelease(ctx, a); err != nil {
		t.Fatal(err)
	}
	rec, won, err := env.Store.IdempotencyBegin(ctx, a, hash[:], time.Minute, time.Hour)
	if err != nil || won || rec.Completed || rec.LockedUntil == nil {
		t.Fatalf("a chave continua em processamento por B: %+v won=%v err=%v", rec, won, err)
	}

	// B registra o resultado; depois disso nenhuma execução o sobrescreve.
	if err := env.Store.IdempotencyComplete(ctx, b, 201, []byte(`{"code":"b"}`), "application/json", ""); err != nil {
		t.Fatal(err)
	}
	for name, k := range map[string]pgstore.IdempotencyKey{"A": a, "B de novo": b} {
		if err := env.Store.IdempotencyComplete(ctx, k, 500, []byte(`{}`), "application/json", ""); !errors.Is(err, pgstore.ErrIdempotencyLeaseLost) {
			t.Fatalf("%s sobrescreveu o resultado registrado: %v", name, err)
		}
	}
	rec, won, err = env.Store.IdempotencyBegin(ctx, a, hash[:], time.Minute, time.Hour)
	if err != nil || won || !rec.Completed || rec.ResponseStatus != 201 || string(rec.ResponseBody) != `{"code":"b"}` {
		t.Fatalf("resultado registrado: %+v won=%v err=%v", rec, won, err)
	}
}
