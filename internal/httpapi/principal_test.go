package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		token  string
		ok     bool
	}{
		{"Bearer rcv_abc", "rcv_abc", true},
		{"bearer rcv_abc", "rcv_abc", true},
		{"BEARER   rcv_abc  ", "rcv_abc", true},
		{"Basic dXNlcjpzZW5oYQ==", "", false},
		{"Bearer", "", false},
		{"Bearer    ", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		token, ok := bearerToken(tc.header)
		if token != tc.token || ok != tc.ok {
			t.Errorf("bearerToken(%q) = %q %v, esperado %q %v", tc.header, token, ok, tc.token, tc.ok)
		}
		if want := ok && strings.HasPrefix(token, "rcv_"); recorviaBearer(tc.header) != want {
			t.Errorf("recorviaBearer(%q) = %v, esperado %v", tc.header, !want, want)
		}
	}
	for _, h := range []string{"Bearer eyJhbGciOiJSUzI1NiJ9.e30.assinatura", "Basic cmN2X2FiYzo=", "rcv_abc"} {
		if recorviaBearer(h) {
			t.Errorf("recorviaBearer(%q) deveria ser falso", h)
		}
	}
}

// authSessions guarda sessões do painel em memória; as operações de login
// não são usadas pelo middleware.
type authSessions struct {
	identity.SessionStore
	items map[string]identity.Session
}

func (s *authSessions) Get(_ context.Context, id string) (identity.Session, error) {
	sess, ok := s.items[id]
	if !ok {
		return identity.Session{}, identity.ErrSessionNotFound
	}
	return sess, nil
}

func (s *authSessions) Save(_ context.Context, sess identity.Session, _ time.Duration) error {
	s.items[sess.ID] = sess
	return nil
}

func (s *authSessions) Delete(_ context.Context, id string) error {
	delete(s.items, id)
	return nil
}

// authUsers devolve sempre o mesmo usuário ativo.
type authUsers struct {
	identity.UserStore
	user identity.User
}

func (u authUsers) User(_ context.Context, id uuid.UUID) (identity.User, error) {
	if id != u.user.ID {
		return identity.User{}, errors.New("usuário desconhecido")
	}
	return u.user, nil
}

func (authUsers) IsPlatformStaff(context.Context, uuid.UUID) (bool, error) { return false, nil }

// Com cookie de sessão, um Authorization que não traz token do Recorvia (por
// exemplo, HTTP Basic de um proxy de homologação, repetido pelo navegador) é
// ignorado e a sessão vale; um token rcv_ tem precedência sobre o cookie; sem
// cookie, qualquer Authorization é tratado como credencial.
func TestAuthenticateChoosesSessionOrCredential(t *testing.T) {
	now := time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC)
	clk := clock.NewManual(now)
	user := identity.User{ID: uuid.New(), DisplayName: "Ana", Status: "active"}
	const sessionID = "sessao-valida"
	sessions := &authSessions{items: map[string]identity.Session{sessionID: {ID: sessionID, UserID: user.ID, CSRFToken: "csrf",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}}}
	ids := identity.NewService(nil, sessions, authUsers{user: user}, clk,
		identity.Policy{Absolute: 8 * time.Hour, Idle: time.Hour, TouchInterval: time.Minute})
	a := &API{Deps: Deps{Identity: ids, Clock: clk}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var got *Principal
	h := a.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		got = &p
		w.WriteHeader(http.StatusNoContent)
	}))
	const (
		session    = "sessão"
		credential = "credencial"
	)
	cases := []struct {
		name   string
		cookie string
		auth   string
		want   string
		code   string
	}{
		{"cookie e HTTP Basic do proxy", sessionID, "Basic dXNlcjpzZW5oYQ==", session, ""},
		{"cookie e outro esquema", sessionID, "Negotiate YIIGhgYGKwYBBQUCoIIGejCCBnag", session, ""},
		{"cookie e Bearer de outro emissor", sessionID, "Bearer eyJhbGciOiJSUzI1NiJ9.e30.assinatura", session, ""},
		{"somente cookie", sessionID, "", session, ""},
		{"cookie e token do Recorvia", sessionID, "Bearer rcv_qualquer", credential, "api_credential_invalid"},
		{"HTTP Basic sem cookie", "", "Basic dXNlcjpzZW5oYQ==", credential, "api_credential_invalid"},
		{"Bearer de outro emissor sem cookie", "", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.assinatura", credential, "api_credential_invalid"},
		{"cookie inválido e HTTP Basic", "sessao-desconhecida", "Basic dXNlcjpzZW5oYQ==", session, "session_invalid"},
		{"sem cookie nem Authorization", "", "", session, "session_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = nil
			req := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "recorvia_session", Value: tc.cookie})
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if tc.code == "" {
				if rec.Code != http.StatusNoContent || got == nil || got.Credential != nil || got.Session.ID != sessionID || got.User.ID != user.ID {
					t.Fatalf("esperada a sessão do cookie: %d %s %+v", rec.Code, rec.Body.String(), got)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized || got != nil || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("esperado 401 %s: %d %s", tc.code, rec.Code, rec.Body.String())
			}
			if bearer := strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer"); bearer != (tc.want == credential) {
				t.Fatalf("WWW-Authenticate = %q no caminho da %s", rec.Header().Get("WWW-Authenticate"), tc.want)
			}
		})
	}
}

func TestPrincipalForUserAndCredential(t *testing.T) {
	uid := uuid.New()
	user := Principal{User: identity.User{ID: uid, DisplayName: "Ana"}, Role: tenancy.RoleOperator}
	if !user.Can(tenancy.PermCustomersWrite) || user.Can(tenancy.PermInvoicesVoid) {
		t.Fatal("permissões do usuário pelo papel")
	}
	if user.Actor() != audit.User(uid, "Ana") || user.ClientID() != "user:"+uid.String() || user.UserID() == nil || *user.UserID() != uid {
		t.Fatalf("usuário: %+v %s", user.Actor(), user.ClientID())
	}
	cid := uuid.New()
	cred := Principal{Credential: &apicredentials.Principal{CredentialID: cid, Name: "ERP", Scopes: []tenancy.Permission{tenancy.PermInvoicesVoid}},
		Role: tenancy.RoleOwner}
	if !cred.Can(tenancy.PermInvoicesVoid) || cred.Can(tenancy.PermCustomersRead) {
		t.Fatal("permissões da credencial apenas pelos escopos, nunca pelo papel")
	}
	if cred.Actor().Type != audit.ActorAPICredential || cred.ClientID() != "api_credential:"+cid.String() || cred.UserID() != nil {
		t.Fatalf("credencial: %+v %s", cred.Actor(), cred.ClientID())
	}
	if (Principal{}).UserID() != nil {
		t.Fatal("sem usuário, a autoria fica nula")
	}
}

func testRouter(t *testing.T) http.Handler {
	t.Helper()
	_, h := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	return h
}

// Sem serviço de credenciais configurado, o Bearer é recusado com 401 e
// WWW-Authenticate; o portal sem serviço responde 503.
func TestBearerAndPortalWithoutServices(t *testing.T) {
	h := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/customers", nil)
	req.Header.Set("Authorization", "Bearer rcv_qualquer")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") ||
		!strings.Contains(rec.Body.String(), "api_credential_invalid") {
		t.Fatalf("Bearer sem serviço: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/portal/access?token=x", "/api/portal/v1/session"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s sem serviço do portal: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRequireUserSessionRejectsCredentials(t *testing.T) {
	a := &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := a.requireUserSession(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	cases := []struct {
		name string
		p    *Principal
		want int
	}{
		{"usuário", &Principal{User: identity.User{ID: uuid.New()}}, http.StatusNoContent},
		{"credencial", &Principal{Credential: &apicredentials.Principal{CredentialID: uuid.New()}}, http.StatusForbidden},
		{"sem principal", nil, http.StatusNoContent},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
		if tc.p != nil {
			req = req.WithContext(withPrincipal(req.Context(), *tc.p))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, esperado %d", tc.name, rec.Code, tc.want)
		}
	}
}
