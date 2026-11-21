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

// As sessões do portal são indexadas pelo link de origem: revogar o link
// remove todas as sessões abertas com ele, e somente elas. O índice guarda
// hashes (nunca identificadores de sessão), expira junto com a sessão de
// maior validade e a remoção é idempotente.
func TestPortalSessionsAreRemovedByLink(t *testing.T) {
	ctx := context.Background()
	rdb := startRedis(t)
	st := redisstore.NewPortalSessions(redisstore.New(rdb, keyring(t), "portal"))
	link, otherLink := uuid.New(), uuid.New()
	session := func(id string, linkID uuid.UUID) portal.Session {
		return portal.Session{ID: id, TenantID: uuid.New(), CustomerID: uuid.New(), LinkID: linkID, CSRFToken: "csrf"}
	}
	for id, s := range map[string]struct {
		link uuid.UUID
		ttl  time.Duration
	}{"sessao-um": {link, 10 * time.Minute}, "sessao-dois": {link, 30 * time.Minute}, "sessao-alheia": {otherLink, 30 * time.Minute}} {
		if err := st.Save(ctx, session(id, s.link), s.ttl); err != nil {
			t.Fatal(err)
		}
	}
	index := "recorvia:portal:link_sessions:" + link.String()
	members, err := rdb.SMembers(ctx, index).Result()
	if err != nil || len(members) != 2 {
		t.Fatalf("índice do link: %v %v", members, err)
	}
	for _, m := range members {
		if strings.Contains(m, "sessao-") || len(m) != 64 {
			t.Fatalf("o índice guarda apenas o hash do identificador da sessão: %q", m)
		}
	}
	// A expiração do índice acompanha a sessão de maior validade e não é
	// encurtada por uma regravação com validade menor (sessão perto do fim).
	if ttl := rdb.TTL(ctx, index).Val(); ttl <= 10*time.Minute || ttl > 30*time.Minute {
		t.Fatalf("expiração do índice: %v", ttl)
	}
	if err := st.Save(ctx, session("sessao-um", link), time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, index).Val(); ttl <= 10*time.Minute {
		t.Fatalf("a expiração do índice não pode ser encurtada: %v", ttl)
	}
	if n := rdb.SCard(ctx, index).Val(); n != 2 {
		t.Fatalf("regravar a sessão não duplica a entrada do índice: %d", n)
	}

	if err := st.DeleteByLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sessao-um", "sessao-dois"} {
		if _, err := st.Get(ctx, id); !errors.Is(err, portal.ErrSessionNotFound) {
			t.Fatalf("sessão %s do link revogado não removida: %v", id, err)
		}
	}
	if n := rdb.Exists(ctx, index).Val(); n != 0 {
		t.Fatal("o índice do link revogado deveria ter sido removido")
	}
	if got, err := st.Get(ctx, "sessao-alheia"); err != nil || got.LinkID != otherLink {
		t.Fatalf("a sessão de outro link não pode ser afetada: %+v %v", got, err)
	}
	// Idempotente: repetir, ou revogar link sem sessões, não falha nem remove nada.
	for _, id := range []uuid.UUID{link, uuid.New()} {
		if err := st.DeleteByLink(ctx, id); err != nil {
			t.Fatalf("remoção repetida: %v", err)
		}
	}
	if keys, err := rdb.Keys(ctx, "recorvia:portal:portal_session:*").Result(); err != nil || len(keys) != 1 {
		t.Fatalf("sessões restantes: %v %v", keys, err)
	}
	// Sessão encerrada pelo próprio cliente (Delete): a entrada residual do
	// índice aponta para uma chave inexistente e a remoção pelo link a limpa.
	if err := st.Delete(ctx, "sessao-alheia"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteByLink(ctx, otherLink); err != nil {
		t.Fatal(err)
	}
	if keys, err := rdb.Keys(ctx, "recorvia:portal:*").Result(); err != nil || len(keys) != 0 {
		t.Fatalf("nada do portal deveria restar: %v %v", keys, err)
	}
}

// Com o Redis indisponível, a remoção pelo link devolve o erro (quem revoga
// decide o que fazer; o link já está revogado no banco).
func TestPortalSessionsDeleteByLinkReportsRedisFailure(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	st := redisstore.NewPortalSessions(redisstore.New(rdb, keyring(t), "portal"))
	if err := st.DeleteByLink(context.Background(), uuid.New()); err == nil {
		t.Fatal("esperava erro com o Redis indisponível")
	}
	if err := st.Save(context.Background(), portal.Session{ID: "s", LinkID: uuid.New()}, time.Minute); err == nil {
		t.Fatal("esperava erro ao gravar com o Redis indisponível")
	}
}
