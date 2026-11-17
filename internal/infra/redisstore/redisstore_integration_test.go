//go:build integration

package redisstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/redisstore"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

func keyring(t *testing.T) *secrets.Keyring {
	kr, err := secrets.NewKeyring(map[string][]byte{"k": bytes.Repeat([]byte{3}, 32)}, "k")
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func startRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:8.8.4-alpine")
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("encerramento do redis: %v", err)
		}
	})
	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := redisstore.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestSessionsLoginFlowAndRevocation(t *testing.T) {
	ctx := context.Background()
	rdb := startRedis(t)
	st := redisstore.New(rdb, keyring(t), "web")
	flow := identity.LoginFlow{State: "estado-unico-123", Nonce: "n", PKCEVerifier: "v", ReturnTo: "/faturas"}
	if err := st.SaveLoginFlow(ctx, flow, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := st.TakeLoginFlow(ctx, flow.State)
	if err != nil || got.ReturnTo != "/faturas" {
		t.Fatalf("fluxo: %+v %v", got, err)
	}
	if _, err := st.TakeLoginFlow(ctx, flow.State); !errors.Is(err, identity.ErrLoginFlowNotFound) {
		t.Fatalf("estado de login deve ser de uso único: %v", err)
	}
	user := uuid.New()
	for _, id := range []string{"sessao-a", "sessao-b"} {
		if err := st.Save(ctx, identity.Session{ID: id, UserID: user, CSRFToken: "c", ExpiresAt: time.Now().Add(time.Hour)}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	// A chave no Redis é o hash do identificador; o conteúdo é cifrado.
	keys, err := rdb.Keys(ctx, "recorvia:web:session:*").Result()
	if err != nil || len(keys) != 2 {
		t.Fatalf("chaves: %v %v", keys, err)
	}
	raw, err := rdb.Get(ctx, keys[0]).Bytes()
	if err != nil || bytes.Contains(raw, []byte("sessao-")) {
		t.Fatalf("conteúdo da sessão não deveria estar em claro: %s", raw)
	}
	if err := st.DeleteAllForUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sessao-a", "sessao-b"} {
		if _, err := st.Get(ctx, id); !errors.Is(err, identity.ErrSessionNotFound) {
			t.Fatalf("sessão %s não revogada: %v", id, err)
		}
	}
	// Limite de taxa em janela fixa.
	for i := range 3 {
		ok, _, err := st.Allow(ctx, "teste", 2, time.Minute)
		if err != nil || ok != (i < 2) {
			t.Fatalf("Allow #%d = %v %v", i, ok, err)
		}
	}
}

type fakeIDP struct{ identity.IdentityProvider }

type fakeUsers struct{ identity.UserStore }

// Falha do Redis torna a autenticação indisponível (503), sem efeito em
// dados financeiros, que não dependem do cache.
func TestRedisFailureMakesAuthenticationUnavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	st := redisstore.New(rdb, keyring(t), "web")
	svc := identity.NewService(fakeIDP{}, st, fakeUsers{}, clock.System{}, identity.DefaultPolicy())
	_, _, err := svc.Authenticate(context.Background(), "qualquer-sessao")
	if !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("esperava indisponível, obtive %v", err)
	}
}
