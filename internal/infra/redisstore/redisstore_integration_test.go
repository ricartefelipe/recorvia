//go:build integration

package redisstore_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	"github.com/ricartefelipe/recorvia/internal/portal"
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

func TestPortalSessionsAndFailureCounter(t *testing.T) {
	ctx := context.Background()
	rdb := startRedis(t)
	st := redisstore.NewPortalSessions(redisstore.New(rdb, keyring(t), "portal"))
	sess := portal.Session{ID: "sessao-do-portal", TenantID: uuid.New(), CustomerID: uuid.New(), LinkID: uuid.New(), CSRFToken: "csrf",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.Save(ctx, sess, 0); !errors.Is(err, portal.ErrSessionNotFound) {
		t.Fatalf("TTL não positiva deve ser recusada: %v", err)
	}
	if err := st.Save(ctx, sess, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, sess.ID)
	if err != nil || got.CustomerID != sess.CustomerID || got.LinkID != sess.LinkID {
		t.Fatalf("sessão lida: %+v %v", got, err)
	}
	// Chave pelo hash do identificador, conteúdo cifrado e namespace próprio.
	keys, err := rdb.Keys(ctx, "recorvia:portal:portal_session:*").Result()
	if err != nil || len(keys) != 1 || strings.Contains(keys[0], sess.ID) {
		t.Fatalf("chaves: %v %v", keys, err)
	}
	raw, err := rdb.Get(ctx, keys[0]).Bytes()
	if err != nil || bytes.Contains(raw, []byte(sess.CustomerID.String())) {
		t.Fatalf("conteúdo da sessão não deveria estar em claro: %s", raw)
	}
	if ttl := rdb.TTL(ctx, keys[0]).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("TTL da sessão: %v", ttl)
	}
	// A sessão do painel com o mesmo identificador não é lida como sessão do portal.
	web := redisstore.New(rdb, keyring(t), "web")
	if _, err := web.Get(ctx, sess.ID); !errors.Is(err, identity.ErrSessionNotFound) {
		t.Fatalf("namespaces devem ser separados: %v", err)
	}
	if err := st.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, sess.ID); !errors.Is(err, portal.ErrSessionNotFound) {
		t.Fatalf("sessão removida: %v", err)
	}

	// Contador de falhas em janela fixa iniciada na primeira falha.
	if n, err := web.Failures(ctx, "portal_access:ip:203.0.113.7"); err != nil || n != 0 {
		t.Fatalf("contador inicial: %d %v", n, err)
	}
	for range 3 {
		if err := web.RecordFailure(ctx, "portal_access:ip:203.0.113.7", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := web.Failures(ctx, "portal_access:ip:203.0.113.7"); err != nil || n != 3 {
		t.Fatalf("contador após três falhas: %d %v", n, err)
	}
	fkeys, err := rdb.Keys(ctx, "recorvia:failures:*").Result()
	if err != nil || len(fkeys) != 1 || strings.Contains(fkeys[0], "203.0.113.7") {
		t.Fatalf("a chave do contador guarda apenas o hash da origem: %v %v", fkeys, err)
	}
	if ttl := rdb.TTL(ctx, fkeys[0]).Val(); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("janela do contador: %v", ttl)
	}
	if n, _ := web.Failures(ctx, "api_credential:ip:203.0.113.7"); n != 0 {
		t.Fatalf("contadores de chaves diferentes são independentes: %d", n)
	}
}
