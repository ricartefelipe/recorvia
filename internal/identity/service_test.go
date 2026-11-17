package identity

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

var testNow = time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)

type identEnv struct {
	idp      *fakeIDP
	sessions *fakeSessions
	users    *fakeUsers
	clock    *clock.Manual
	policy   Policy
	svc      *Service
}

func newIdentEnv() *identEnv {
	e := &identEnv{
		idp: &fakeIDP{
			authURL:  "https://idp.example/auth",
			verifier: "verificador-pkce",
			tokens:   Tokens{IDToken: "id-token", AccessToken: "access", RefreshToken: "refresh", Expiry: testNow.Add(5 * time.Minute)},
			claims:   Claims{Issuer: "https://idp.example", Subject: "sub-1", Email: "ana@acme.com", EmailVerified: true, Name: "Ana"},
		},
		sessions: newFakeSessions(),
		users:    newFakeUsers(),
		clock:    clock.NewManual(testNow),
		policy:   DefaultPolicy(),
	}
	e.rebuild()
	return e
}

func (e *identEnv) rebuild() { e.svc = NewService(e.idp, e.sessions, e.users, e.clock, e.policy) }

func assertKind(t testing.TB, err error, kind apperr.Kind, code string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok {
		t.Fatalf("esperado *apperr.Error %s/%s, obtido %v", kind, code, err)
	}
	if ae.Kind != kind || ae.Code != code {
		t.Fatalf("erro = %s/%s, esperado %s/%s", ae.Kind, ae.Code, kind, code)
	}
}

// begin inicia um login e devolve o state salvo.
func (e *identEnv) begin(t *testing.T, returnTo string) string {
	t.Helper()
	if _, err := e.svc.BeginLogin(context.Background(), returnTo); err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	return e.idp.gotState
}

// --- BeginLogin ---

func TestBeginLogin(t *testing.T) {
	e := newIdentEnv()
	target, err := e.svc.BeginLogin(context.Background(), "/faturas?status=open")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(target, "https://idp.example/auth?state=") {
		t.Fatalf("URL de autorização inesperada: %s", target)
	}
	if len(e.sessions.flows) != 1 {
		t.Fatalf("esperado um fluxo salvo, obtidos %d", len(e.sessions.flows))
	}
	f, ok := e.sessions.flows[e.idp.gotState]
	if !ok {
		t.Fatal("o fluxo deve ser salvo pelo mesmo state enviado ao provedor")
	}
	if f.Nonce != e.idp.gotNonce || f.PKCEVerifier != "verificador-pkce" || e.idp.gotVerifier != "verificador-pkce" {
		t.Fatalf("nonce ou verificador PKCE divergentes: %+v", f)
	}
	if len(f.State) != 43 || len(f.Nonce) != 43 || f.State == f.Nonce {
		t.Fatalf("state e nonce devem ser aleatórios e distintos: %q %q", f.State, f.Nonce)
	}
	if f.ReturnTo != "/faturas?status=open" || !f.CreatedAt.Equal(testNow) {
		t.Fatalf("fluxo inesperado: %+v", f)
	}
	if e.sessions.flowTTL != e.policy.LoginFlowTTL {
		t.Errorf("TTL do fluxo = %v, esperado %v", e.sessions.flowTTL, e.policy.LoginFlowTTL)
	}

	// Cada login usa um state novo.
	first := f.State
	if second := e.begin(t, "/"); second == first {
		t.Fatal("state reutilizado entre logins")
	}
}

func TestBeginLoginSanitizesReturnPath(t *testing.T) {
	e := newIdentEnv()
	state := e.begin(t, "https://evil.example/")
	if got := e.sessions.flows[state].ReturnTo; got != "/" {
		t.Fatalf("destino externo deveria virar a raiz, obtido %q", got)
	}
}

func TestBeginLoginProviderUnavailable(t *testing.T) {
	e := newIdentEnv()
	e.idp.authURL = ""
	target, err := e.svc.BeginLogin(context.Background(), "/")
	assertKind(t, err, apperr.KindUnavailable, "identity_provider_unavailable")
	if target != "" || len(e.sessions.flows) != 0 {
		t.Fatal("nenhum fluxo deveria ser salvo com o provedor indisponível")
	}
}

func TestBeginLoginStoreFailure(t *testing.T) {
	e := newIdentEnv()
	e.sessions.fail["SaveLoginFlow"] = errBoom
	target, err := e.svc.BeginLogin(context.Background(), "/")
	if !errors.Is(err, errBoom) || target != "" {
		t.Fatalf("esperado erro encadeado sem URL, obtido %q, %v", target, err)
	}
}

// --- CompleteLogin ---

func TestCompleteLogin(t *testing.T) {
	e := newIdentEnv()
	state := e.begin(t, "/clientes")
	flow := e.sessions.flows[state]

	sess, user, returnTo, err := e.svc.CompleteLogin(context.Background(), state, "codigo-1")
	if err != nil {
		t.Fatal(err)
	}
	if e.idp.gotCode != "codigo-1" || e.idp.gotExchangeVerifier != flow.PKCEVerifier {
		t.Errorf("troca do código deve usar o verificador PKCE do fluxo: %q %q", e.idp.gotCode, e.idp.gotExchangeVerifier)
	}
	if e.idp.gotIDToken != "id-token" || e.idp.gotExpectedNonce != flow.Nonce {
		t.Errorf("ID token deve ser validado com o nonce do fluxo: %q %q", e.idp.gotIDToken, e.idp.gotExpectedNonce)
	}
	if returnTo != "/clientes" {
		t.Errorf("destino = %q, esperado /clientes", returnTo)
	}
	if user.ID == uuid.Nil || user.Email != "ana@acme.com" || user.Status != "active" {
		t.Fatalf("usuário inesperado: %+v", user)
	}
	if len(sess.ID) != 43 || len(sess.CSRFToken) != 43 || sess.ID == sess.CSRFToken {
		t.Fatalf("sessão deve ter identificador e CSRF aleatórios e distintos: %q %q", sess.ID, sess.CSRFToken)
	}
	if sess.UserID != user.ID || sess.ActiveTenantID != nil || !sess.CreatedAt.Equal(testNow) || !sess.LastSeenAt.Equal(testNow) ||
		!sess.ExpiresAt.Equal(testNow.Add(12*time.Hour)) || sess.Tokens != e.idp.tokens {
		t.Fatalf("sessão inesperada: %+v", sess)
	}
	if stored, ok := e.sessions.sessions[sess.ID]; !ok || stored.CSRFToken != sess.CSRFToken {
		t.Fatal("sessão não persistida")
	}
	if ttl := e.sessions.ttls[sess.ID]; ttl != e.policy.Idle {
		t.Errorf("TTL = %v, esperado a inatividade %v", ttl, e.policy.Idle)
	}
	if len(e.sessions.flows) != 0 {
		t.Error("o fluxo de login deve ser consumido")
	}

	// O state é de uso único.
	_, _, _, err = e.svc.CompleteLogin(context.Background(), state, "codigo-1")
	assertKind(t, err, apperr.KindUnauthenticated, "login_invalid")

	// Um novo login cria outra sessão, sem reaproveitar identificadores.
	sess2, _, _, err := e.svc.CompleteLogin(context.Background(), e.begin(t, "/"), "codigo-2")
	if err != nil {
		t.Fatal(err)
	}
	if sess2.ID == sess.ID || sess2.CSRFToken == sess.CSRFToken || sess2.UserID != sess.UserID {
		t.Fatalf("segunda sessão deve ter identificadores novos para o mesmo usuário: %+v", sess2)
	}
}

func TestCompleteLoginTTLLimitedByAbsoluteExpiry(t *testing.T) {
	e := newIdentEnv()
	e.policy.Absolute = 30 * time.Minute
	e.rebuild()
	sess, _, _, err := e.svc.CompleteLogin(context.Background(), e.begin(t, "/"), "c")
	if err != nil {
		t.Fatal(err)
	}
	if ttl := e.sessions.ttls[sess.ID]; ttl != 30*time.Minute {
		t.Fatalf("TTL = %v, esperado 30m", ttl)
	}
}

func TestCompleteLoginRejected(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *identEnv, state string) (string, string)
		kind  apperr.Kind
		code  string
		cause error
	}{
		{"state vazio", func(_ *identEnv, _ string) (string, string) { return "", "codigo" }, apperr.KindUnauthenticated, "login_invalid", nil},
		{"código vazio", func(_ *identEnv, s string) (string, string) { return s, "" }, apperr.KindUnauthenticated, "login_invalid", nil},
		{"state desconhecido", func(_ *identEnv, _ string) (string, string) { return RandomToken(32), "codigo" }, apperr.KindUnauthenticated, "login_invalid", nil},
		{"troca do código falha", func(e *identEnv, s string) (string, string) {
			e.idp.exchangeErr = errBoom
			return s, "codigo"
		}, apperr.KindUnauthenticated, "login_invalid", errBoom},
		{"ID token inválido", func(e *identEnv, s string) (string, string) {
			e.idp.verifyErr = errBoom
			return s, "codigo"
		}, apperr.KindUnauthenticated, "login_invalid", errBoom},
		{"e-mail não verificado", func(e *identEnv, s string) (string, string) {
			e.idp.claims.EmailVerified = false
			return s, "codigo"
		}, apperr.KindForbidden, "email_not_verified", nil},
		{"usuário desativado", func(e *identEnv, s string) (string, string) {
			e.users.statusOnCreate = "disabled"
			return s, "codigo"
		}, apperr.KindForbidden, "user_disabled", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIdentEnv()
			state, code := tc.setup(e, e.begin(t, "/"))
			sess, user, returnTo, err := e.svc.CompleteLogin(context.Background(), state, code)
			assertKind(t, err, tc.kind, tc.code)
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("a causa original deve ser encadeada: %v", err)
			}
			if sess.ID != "" || user.ID != uuid.Nil || returnTo != "" {
				t.Fatal("nenhuma sessão ou usuário deveria ser devolvido")
			}
			if len(e.sessions.sessions) != 0 {
				t.Fatal("nenhuma sessão deveria ser criada")
			}
		})
	}
}

func TestCompleteLoginUnverifiedEmailAllowedByPolicy(t *testing.T) {
	e := newIdentEnv()
	e.policy.RequireVerifiedEmail = false
	e.rebuild()
	e.idp.claims.EmailVerified = false
	if _, _, _, err := e.svc.CompleteLogin(context.Background(), e.begin(t, "/"), "codigo"); err != nil {
		t.Fatalf("política sem exigência de verificação deveria aceitar: %v", err)
	}
}

func TestCompleteLoginInfrastructureErrors(t *testing.T) {
	t.Run("falha ao ler o fluxo", func(t *testing.T) {
		e := newIdentEnv()
		state := e.begin(t, "/")
		e.sessions.fail["TakeLoginFlow"] = errBoom
		_, _, _, err := e.svc.CompleteLogin(context.Background(), state, "codigo")
		if !errors.Is(err, errBoom) || apperr.IsKind(err, apperr.KindUnauthenticated) {
			t.Fatalf("falha de armazenamento não pode virar login inválido: %v", err)
		}
	})
	t.Run("falha ao gravar o usuário", func(t *testing.T) {
		e := newIdentEnv()
		e.users.upsertErr = errBoom
		if _, _, _, err := e.svc.CompleteLogin(context.Background(), e.begin(t, "/"), "codigo"); !errors.Is(err, errBoom) {
			t.Fatalf("esperado erro propagado, obtido %v", err)
		}
	})
	t.Run("falha ao gravar a sessão", func(t *testing.T) {
		e := newIdentEnv()
		state := e.begin(t, "/")
		e.sessions.fail["Save"] = errBoom
		if _, _, _, err := e.svc.CompleteLogin(context.Background(), state, "codigo"); !errors.Is(err, errBoom) {
			t.Fatalf("esperado erro propagado, obtido %v", err)
		}
	})
}

// --- Authenticate ---

// seedSession grava um usuário ativo e uma sessão criada em testNow.
func (e *identEnv) seedSession(t *testing.T) Session {
	t.Helper()
	uid := uuid.New()
	e.users.byID[uid] = User{ID: uid, Email: "ana@acme.com", Status: "active"}
	sess := Session{
		ID: "sessao-1", UserID: uid, CSRFToken: "csrf-1", CreatedAt: testNow, LastSeenAt: testNow,
		ExpiresAt: testNow.Add(12 * time.Hour),
		Tokens:    Tokens{IDToken: "id-1", AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: testNow.Add(time.Hour)},
	}
	e.sessions.sessions[sess.ID] = sess
	return sess
}

func TestAuthenticate(t *testing.T) {
	e := newIdentEnv()
	seeded := e.seedSession(t)
	e.clock.Advance(30 * time.Second)

	sess, user, err := e.svc.Authenticate(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != seeded.UserID || sess.ID != seeded.ID {
		t.Fatalf("sessão ou usuário inesperados: %+v %+v", sess, user)
	}
	// Dentro do intervalo de toque a sessão não é regravada.
	if e.sessions.saves != 0 || !sess.LastSeenAt.Equal(testNow) {
		t.Fatalf("não deveria regravar antes do intervalo de toque: saves=%d", e.sessions.saves)
	}
}

func TestAuthenticateTouchesAfterInterval(t *testing.T) {
	e := newIdentEnv()
	seeded := e.seedSession(t)
	e.clock.Advance(e.policy.TouchInterval)

	sess, _, err := e.svc.Authenticate(context.Background(), seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.Now()
	if e.sessions.saves != 1 || !sess.LastSeenAt.Equal(now) || !e.sessions.sessions[seeded.ID].LastSeenAt.Equal(now) {
		t.Fatalf("última atividade deveria ser gravada: saves=%d %+v", e.sessions.saves, sess)
	}
	if ttl := e.sessions.ttls[seeded.ID]; ttl != e.policy.Idle {
		t.Errorf("TTL = %v, esperado %v", ttl, e.policy.Idle)
	}
	// A expiração absoluta não é estendida pela atividade.
	if !sess.ExpiresAt.Equal(seeded.ExpiresAt) {
		t.Errorf("expiração absoluta alterada: %v", sess.ExpiresAt)
	}
}

func TestAuthenticateTTLNeverExceedsAbsoluteExpiry(t *testing.T) {
	e := newIdentEnv()
	seeded := e.seedSession(t)
	s := e.sessions.sessions[seeded.ID]
	s.ExpiresAt = testNow.Add(90 * time.Second)
	e.sessions.sessions[seeded.ID] = s
	e.clock.Advance(time.Minute)

	if _, _, err := e.svc.Authenticate(context.Background(), seeded.ID); err != nil {
		t.Fatal(err)
	}
	if ttl := e.sessions.ttls[seeded.ID]; ttl != 30*time.Second {
		t.Fatalf("TTL = %v, esperado o restante até a expiração absoluta (30s)", ttl)
	}
}

func TestAuthenticateRejected(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *identEnv, s Session) string
		deleted bool
	}{
		{"identificador vazio", func(*identEnv, Session) string { return "" }, false},
		{"sessão inexistente", func(*identEnv, Session) string { return "nao-existe" }, false},
		{"expiração absoluta atingida", func(e *identEnv, s Session) string {
			s.LastSeenAt = s.ExpiresAt.Add(-time.Minute)
			e.sessions.sessions[s.ID] = s
			e.clock.Set(s.ExpiresAt)
			return s.ID
		}, true},
		{"inatividade atingida", func(e *identEnv, s Session) string {
			e.clock.Set(s.LastSeenAt.Add(e.policy.Idle))
			return s.ID
		}, true},
		{"usuário desativado", func(e *identEnv, s Session) string {
			u := e.users.byID[s.UserID]
			u.Status = "disabled"
			e.users.byID[s.UserID] = u
			return s.ID
		}, true},
		{"usuário não encontrado", func(e *identEnv, s Session) string {
			delete(e.users.byID, s.UserID)
			return s.ID
		}, true},
		{"falha ao ler o usuário", func(e *identEnv, s Session) string {
			e.users.userErr = errBoom
			return s.ID
		}, true},
		{"renovação do token falha", func(e *identEnv, s Session) string {
			e.idp.refreshErr = errBoom
			e.clock.Set(s.Tokens.Expiry)
			return s.ID
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIdentEnv()
			seeded := e.seedSession(t)
			id := tc.setup(e, seeded)
			sess, user, err := e.svc.Authenticate(context.Background(), id)
			assertKind(t, err, apperr.KindUnauthenticated, "session_invalid")
			if sess.ID != "" || user.ID != uuid.Nil {
				t.Fatal("nenhuma sessão deveria ser devolvida")
			}
			_, still := e.sessions.sessions[seeded.ID]
			if tc.deleted && (still || !slices.Contains(e.sessions.deleted, seeded.ID)) {
				t.Fatal("a sessão deveria ser removida")
			}
			if !tc.deleted && !still {
				t.Fatal("a sessão existente não deveria ser removida")
			}
		})
	}
}

func TestAuthenticateJustBeforeIdleLimit(t *testing.T) {
	e := newIdentEnv()
	seeded := e.seedSession(t)
	e.clock.Advance(e.policy.Idle - time.Second)
	if _, _, err := e.svc.Authenticate(context.Background(), seeded.ID); err != nil {
		t.Fatalf("sessão ainda dentro do limite de inatividade: %v", err)
	}
}

func TestAuthenticateDeleteFailureStillRejects(t *testing.T) {
	e := newIdentEnv()
	seeded := e.seedSession(t)
	e.sessions.fail["Delete"] = errBoom
	e.clock.Set(seeded.ExpiresAt)
	_, _, err := e.svc.Authenticate(context.Background(), seeded.ID)
	assertKind(t, err, apperr.KindUnauthenticated, "session_invalid")
}

func TestAuthenticateRefreshesTokens(t *testing.T) {
	cases := []struct {
		name        string
		newIDToken  string
		wantIDToken string
	}{
		{"provedor sem novo ID token mantém o anterior", "", "id-1"},
		{"provedor com novo ID token substitui", "id-2", "id-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIdentEnv()
			seeded := e.seedSession(t)
			s := e.sessions.sessions[seeded.ID]
			s.LastSeenAt = seeded.Tokens.Expiry.Add(-10 * time.Second)
			e.sessions.sessions[seeded.ID] = s
			e.clock.Set(seeded.Tokens.Expiry)
			e.idp.refreshed = Tokens{IDToken: tc.newIDToken, AccessToken: "access-2", RefreshToken: "refresh-2", Expiry: seeded.Tokens.Expiry.Add(time.Hour)}

			sess, _, err := e.svc.Authenticate(context.Background(), seeded.ID)
			if err != nil {
				t.Fatal(err)
			}
			if e.idp.refreshCalls != 1 || e.idp.gotRefreshToken != "refresh-1" {
				t.Fatalf("renovação deveria usar o refresh token da sessão: calls=%d %q", e.idp.refreshCalls, e.idp.gotRefreshToken)
			}
			if sess.Tokens.AccessToken != "access-2" || sess.Tokens.IDToken != tc.wantIDToken {
				t.Fatalf("tokens inesperados: %+v", sess.Tokens)
			}
			// Tokens renovados são gravados mesmo antes do intervalo de toque.
			if e.sessions.saves != 1 || e.sessions.sessions[seeded.ID].Tokens.AccessToken != "access-2" {
				t.Fatal("tokens renovados devem ser gravados")
			}
			if !sess.LastSeenAt.Equal(e.clock.Now()) {
				t.Errorf("última atividade = %v, esperado %v", sess.LastSeenAt, e.clock.Now())
			}
		})
	}
}

func TestAuthenticateSkipsRefresh(t *testing.T) {
	cases := []struct {
		name   string
		tokens Tokens
	}{
		{"sem refresh token", Tokens{IDToken: "id-1", AccessToken: "a", Expiry: testNow}},
		{"sem expiração conhecida", Tokens{IDToken: "id-1", AccessToken: "a", RefreshToken: "r"}},
		{"token ainda válido", Tokens{IDToken: "id-1", AccessToken: "a", RefreshToken: "r", Expiry: testNow.Add(time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIdentEnv()
			seeded := e.seedSession(t)
			s := e.sessions.sessions[seeded.ID]
			s.Tokens = tc.tokens
			e.sessions.sessions[seeded.ID] = s
			e.clock.Advance(10 * time.Second)
			if _, _, err := e.svc.Authenticate(context.Background(), seeded.ID); err != nil {
				t.Fatal(err)
			}
			if e.idp.refreshCalls != 0 {
				t.Fatal("não deveria renovar")
			}
		})
	}
}

func TestAuthenticateStoreUnavailable(t *testing.T) {
	t.Run("falha ao ler a sessão", func(t *testing.T) {
		e := newIdentEnv()
		e.seedSession(t)
		e.sessions.fail["Get"] = errBoom
		_, _, err := e.svc.Authenticate(context.Background(), "sessao-1")
		assertKind(t, err, apperr.KindUnavailable, "session_store_unavailable")
		if !errors.Is(err, errBoom) {
			t.Error("a causa deve ser encadeada")
		}
	})
	t.Run("falha ao gravar a atividade", func(t *testing.T) {
		e := newIdentEnv()
		e.seedSession(t)
		e.sessions.fail["Save"] = errBoom
		e.clock.Advance(5 * time.Minute)
		_, _, err := e.svc.Authenticate(context.Background(), "sessao-1")
		assertKind(t, err, apperr.KindUnavailable, "session_store_unavailable")
	})
}

// --- SwitchTenant ---

func TestSwitchTenantRotatesSession(t *testing.T) {
	e := newIdentEnv()
	old := e.seedSession(t)
	tenantID := uuid.New()
	e.clock.Advance(10 * time.Minute)
	var gotTenant, gotUser uuid.UUID
	check := func(_ context.Context, tid, uid uuid.UUID) error {
		gotTenant, gotUser = tid, uid
		return nil
	}

	next, err := e.svc.SwitchTenant(context.Background(), old, tenantID, check)
	if err != nil {
		t.Fatal(err)
	}
	if gotTenant != tenantID || gotUser != old.UserID {
		t.Fatalf("acesso verificado com parâmetros errados: %s %s", gotTenant, gotUser)
	}
	if next.ID == old.ID || next.CSRFToken == old.CSRFToken || len(next.ID) != 43 || len(next.CSRFToken) != 43 {
		t.Fatalf("a sessão deve ser rotacionada com identificador e CSRF novos: %+v", next)
	}
	if next.ActiveTenantID == nil || *next.ActiveTenantID != tenantID {
		t.Fatalf("organização ativa = %v, esperado %s", next.ActiveTenantID, tenantID)
	}
	if !next.LastSeenAt.Equal(e.clock.Now()) || !next.CreatedAt.Equal(old.CreatedAt) || !next.ExpiresAt.Equal(old.ExpiresAt) ||
		next.UserID != old.UserID || next.Tokens != old.Tokens {
		t.Fatalf("dados da sessão não deveriam mudar além da rotação: %+v", next)
	}
	if _, ok := e.sessions.sessions[old.ID]; ok {
		t.Fatal("a sessão anterior deve ser removida")
	}
	if stored, ok := e.sessions.sessions[next.ID]; !ok || stored.CSRFToken != next.CSRFToken {
		t.Fatal("a nova sessão deve ser gravada")
	}
	if ttl := e.sessions.ttls[next.ID]; ttl != e.policy.Idle {
		t.Errorf("TTL = %v, esperado %v", ttl, e.policy.Idle)
	}
}

func TestSwitchTenantAccessDenied(t *testing.T) {
	e := newIdentEnv()
	old := e.seedSession(t)
	before := maps.Clone(e.sessions.sessions)
	denied := apperr.Forbidden("no_membership", "Você não tem acesso a esta organização.")

	next, err := e.svc.SwitchTenant(context.Background(), old, uuid.New(), func(context.Context, uuid.UUID, uuid.UUID) error { return denied })
	if !errors.Is(err, denied) {
		t.Fatalf("o erro da verificação deve ser devolvido, obtido %v", err)
	}
	if next.ID != "" {
		t.Fatal("nenhuma sessão deveria ser devolvida")
	}
	if !maps.EqualFunc(before, e.sessions.sessions, func(a, b Session) bool { return a.ID == b.ID && a.CSRFToken == b.CSRFToken }) ||
		e.sessions.saves != 0 || len(e.sessions.deleted) != 0 {
		t.Fatal("acesso negado não pode alterar sessões")
	}
}

func TestSwitchTenantStoreErrors(t *testing.T) {
	allow := func(context.Context, uuid.UUID, uuid.UUID) error { return nil }
	t.Run("falha ao gravar mantém a sessão anterior", func(t *testing.T) {
		e := newIdentEnv()
		old := e.seedSession(t)
		e.sessions.fail["Save"] = errBoom
		if _, err := e.svc.SwitchTenant(context.Background(), old, uuid.New(), allow); !errors.Is(err, errBoom) {
			t.Fatalf("esperado erro propagado, obtido %v", err)
		}
		if _, ok := e.sessions.sessions[old.ID]; !ok {
			t.Fatal("a sessão anterior não pode ser removida se a nova não foi gravada")
		}
	})
	t.Run("falha ao remover a sessão anterior", func(t *testing.T) {
		e := newIdentEnv()
		old := e.seedSession(t)
		e.sessions.fail["Delete"] = errBoom
		next, err := e.svc.SwitchTenant(context.Background(), old, uuid.New(), allow)
		if !errors.Is(err, errBoom) || next.ID != "" {
			t.Fatalf("esperado erro propagado sem sessão, obtido %+v, %v", next, err)
		}
	})
}

// --- Logout, revogação e consultas ---

func TestLogout(t *testing.T) {
	e := newIdentEnv()
	sess := e.seedSession(t)
	target, err := e.svc.Logout(context.Background(), sess, "https://app.example/")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.sessions.sessions[sess.ID]; ok {
		t.Fatal("a sessão local deve ser removida")
	}
	if !strings.Contains(target, "id_token_hint=id-1") || !strings.Contains(target, "post_logout_redirect_uri=https%3A%2F%2Fapp.example%2F") {
		t.Fatalf("URL de encerramento inesperada: %s", target)
	}

	e.sessions.fail["Delete"] = errBoom
	target, err = e.svc.Logout(context.Background(), sess, "/")
	if !errors.Is(err, errBoom) || target != "" {
		t.Fatalf("falha ao remover deve impedir o logout no provedor: %q, %v", target, err)
	}
}

func TestRevokeAll(t *testing.T) {
	e := newIdentEnv()
	sess := e.seedSession(t)
	other := Session{ID: "sessao-2", UserID: sess.UserID}
	stranger := Session{ID: "sessao-3", UserID: uuid.New()}
	e.sessions.sessions[other.ID], e.sessions.sessions[stranger.ID] = other, stranger

	if err := e.svc.RevokeAll(context.Background(), sess.UserID); err != nil {
		t.Fatal(err)
	}
	if len(e.sessions.sessions) != 1 || e.sessions.sessions["sessao-3"].ID == "" {
		t.Fatalf("somente as sessões do usuário devem ser removidas: %v", e.sessions.sessions)
	}
	e.sessions.fail["DeleteAllForUser"] = errBoom
	if err := e.svc.RevokeAll(context.Background(), sess.UserID); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
}

func TestUserQueries(t *testing.T) {
	e := newIdentEnv()
	sess := e.seedSession(t)
	e.users.staff[sess.UserID] = true

	u, err := e.svc.User(context.Background(), sess.UserID)
	if err != nil || u.ID != sess.UserID {
		t.Fatalf("User = %+v, %v", u, err)
	}
	staff, err := e.svc.IsPlatformStaff(context.Background(), sess.UserID)
	if err != nil || !staff {
		t.Fatalf("IsPlatformStaff = %v, %v", staff, err)
	}
	if staff, _ := e.svc.IsPlatformStaff(context.Background(), uuid.New()); staff {
		t.Fatal("usuário comum não integra a equipe da plataforma")
	}
	e.users.staffErr = errBoom
	if _, err := e.svc.IsPlatformStaff(context.Background(), sess.UserID); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
}
