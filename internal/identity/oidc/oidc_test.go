package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

const (
	testClientID     = "recorvia-web"
	testClientSecret = "segredo-do-cliente"
)

// fakeProvider é um provedor OIDC mínimo em httptest: descoberta, JWKS,
// token e revogação. Os ID tokens são assinados com a chave RSA do teste.
type fakeProvider struct {
	t      *testing.T
	srv    *httptest.Server
	issuer string
	key    *rsa.PrivateKey

	mu sync.Mutex
	// tokenResponse é a resposta do endpoint de token (código ou renovação).
	tokenResponse map[string]any
	tokenStatus   int
	tokenForms    []url.Values
	revokeStatus  int
	revocations   []url.Values
	revokeAuth    []string
	// noEndpoints omite end_session_endpoint e revocation_endpoint.
	noEndpoints bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProvider{t: t, key: key, tokenStatus: http.StatusOK, revokeStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("/jwks", f.jwks)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/revoke", f.revoke)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	return f
}

func (f *fakeProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	meta := map[string]any{
		"issuer": f.issuer, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
		"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
	}
	if !f.noEndpoints {
		meta["end_session_endpoint"] = f.srv.URL + "/logout"
		meta["revocation_endpoint"] = f.srv.URL + "/revoke"
	}
	writeJSON(w, http.StatusOK, meta)
}

func (f *fakeProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
		"n": b64(f.key.N.Bytes()), "e": b64(big.NewInt(int64(f.key.E)).Bytes()),
	}}})
}

func (f *fakeProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Error(err)
	}
	f.mu.Lock()
	f.tokenForms = append(f.tokenForms, r.PostForm)
	status, body := f.tokenStatus, f.tokenResponse
	f.mu.Unlock()
	if status != http.StatusOK {
		writeJSON(w, status, map[string]string{"error": "invalid_grant"})
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (f *fakeProvider) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Error(err)
	}
	f.mu.Lock()
	f.revocations = append(f.revocations, r.PostForm)
	f.revokeAuth = append(f.revokeAuth, r.Header.Get("Authorization"))
	status := f.revokeStatus
	f.mu.Unlock()
	w.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// claims padrão de um ID token válido para o cliente do teste.
func (f *fakeProvider) claims(nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": f.issuer, "sub": "usuario-1", "aud": testClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": nonce, "email": "ana@acme.com", "email_verified": true, "name": "Ana Souza",
	}
}

// sign monta um JWT RS256 com a chave do provedor (ou HS256 com o segredo
// do cliente, para o teste de algoritmo).
func (f *fakeProvider) sign(t *testing.T, alg string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": alg, "kid": "k1", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := b64(header) + "." + b64(payload)
	var sig []byte
	switch alg {
	case "RS256":
		sum := sha256.Sum256([]byte(input))
		if sig, err = rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:]); err != nil {
			t.Fatal(err)
		}
	case "HS256":
		m := hmac.New(sha256.New, []byte(testClientSecret))
		m.Write([]byte(input))
		sig = m.Sum(nil)
	case "none":
	}
	return input + "." + b64(sig)
}

// tamper troca o conteúdo do token mantendo cabeçalho e assinatura.
func tamper(token string, claims map[string]any) string {
	parts := strings.Split(token, ".")
	payload, _ := json.Marshal(claims)
	return parts[0] + "." + b64(payload) + "." + parts[2]
}

func (f *fakeProvider) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(context.Background(), Config{Issuer: f.issuer, ClientID: testClientID, ClientSecret: testClientSecret,
		RedirectURL: "https://app.test/auth/callback", HTTPTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewDiscoversProvider(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	if p.endSession != f.srv.URL+"/logout" || p.revocation != f.srv.URL+"/revoke" || p.oauth.Endpoint.TokenURL != f.srv.URL+"/token" {
		t.Fatalf("endpoints não descobertos: %+v", p)
	}

	// Emissor anunciado diferente do configurado: a descoberta falha.
	if _, err := New(context.Background(), Config{Issuer: f.srv.URL + "/outro", ClientID: testClientID}); err == nil {
		t.Fatal("descoberta com emissor divergente deveria falhar")
	}
	// Endereço interno de descoberta: o emissor público continua validado.
	f.issuer = "https://idp.publico.test/realms/recorvia"
	p, err := New(context.Background(), Config{Issuer: f.issuer, DiscoveryURL: f.srv.URL, ClientID: testClientID})
	if err != nil {
		t.Fatalf("descoberta pelo endereço interno: %v", err)
	}
	if _, err := p.VerifyIDToken(context.Background(), f.sign(t, "RS256", f.claims("n")), "n"); err != nil {
		t.Fatalf("token com o emissor público deveria ser aceito: %v", err)
	}
	other := f.claims("n")
	other["iss"] = f.srv.URL
	if _, err := p.VerifyIDToken(context.Background(), f.sign(t, "RS256", other), "n"); err == nil {
		t.Fatal("token com o emissor interno não é o emissor configurado")
	}
}

func TestAuthCodeURLUsesPKCEAndNonce(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	verifier := p.NewPKCEVerifier()
	if len(verifier) < 43 {
		t.Fatalf("verificador PKCE curto: %q", verifier)
	}
	raw := p.AuthCodeURL("estado-1", "nonce-1", verifier)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte(verifier))
	if !strings.HasPrefix(raw, f.srv.URL+"/auth?") || q.Get("state") != "estado-1" || q.Get("nonce") != "nonce-1" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != b64(sum[:]) || q.Get("client_id") != testClientID ||
		q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") || q.Get("redirect_uri") != "https://app.test/auth/callback" {
		t.Fatalf("URL de autorização inesperada: %s", raw)
	}
	if strings.Contains(raw, verifier) {
		t.Fatal("o verificador PKCE não pode ir para o navegador")
	}
}

func TestExchange(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	f.tokenResponse = map[string]any{"access_token": "at", "refresh_token": "rt", "token_type": "Bearer", "expires_in": 300, "id_token": "idt"}
	tok, err := p.Exchange(context.Background(), "codigo-1", "verificador-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.IDToken != "idt" || tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.Expiry.IsZero() {
		t.Fatalf("tokens inesperados: %+v", tok)
	}
	form := f.tokenForms[0]
	if form.Get("code") != "codigo-1" || form.Get("code_verifier") != "verificador-1" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("troca sem o verificador PKCE: %v", form)
	}

	f.tokenResponse = map[string]any{"access_token": "at", "token_type": "Bearer"}
	if _, err := p.Exchange(context.Background(), "codigo-2", "v"); err == nil || !strings.Contains(err.Error(), "sem id_token") {
		t.Fatalf("resposta sem id_token deveria falhar: %v", err)
	}
	f.tokenStatus = http.StatusBadRequest
	if _, err := p.Exchange(context.Background(), "codigo-3", "v"); err == nil {
		t.Fatal("código recusado pelo provedor deveria falhar")
	}
}

func TestVerifyIDToken(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	ctx := context.Background()

	c, err := p.VerifyIDToken(ctx, f.sign(t, "RS256", f.claims("nonce-1")), "nonce-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != f.issuer || c.Subject != "usuario-1" || c.Email != "ana@acme.com" || !c.EmailVerified || c.Name != "Ana Souza" {
		t.Fatalf("claims inesperadas: %+v", c)
	}
	noName := f.claims("n")
	delete(noName, "name")
	noName["preferred_username"] = "ana.souza"
	if c, err := p.VerifyIDToken(ctx, f.sign(t, "RS256", noName), "n"); err != nil || c.Name != "ana.souza" {
		t.Fatalf("sem name, usa preferred_username: %+v %v", c, err)
	}

	mutate := func(fn func(map[string]any)) map[string]any {
		c := f.claims("nonce-1")
		fn(c)
		return c
	}
	cases := []struct {
		name  string
		token string
		nonce string
	}{
		{"nonce divergente", f.sign(t, "RS256", f.claims("outro-nonce")), "nonce-1"},
		{"nonce ausente no token", f.sign(t, "RS256", mutate(func(c map[string]any) { delete(c, "nonce") })), ""},
		{"algoritmo HS256", f.sign(t, "HS256", f.claims("nonce-1")), "nonce-1"},
		{"algoritmo none", f.sign(t, "none", f.claims("nonce-1")), "nonce-1"},
		{"conteúdo adulterado", tamper(f.sign(t, "RS256", f.claims("nonce-1")), mutate(func(c map[string]any) { c["email"] = "outra@acme.com" })), "nonce-1"},
		{"emissor divergente", f.sign(t, "RS256", mutate(func(c map[string]any) { c["iss"] = "https://outro.test" })), "nonce-1"},
		{"audiência de outro cliente", f.sign(t, "RS256", mutate(func(c map[string]any) { c["aud"] = "outro-cliente" })), "nonce-1"},
		{"expirado", f.sign(t, "RS256", mutate(func(c map[string]any) {
			c["iat"], c["exp"] = time.Now().Add(-time.Hour).Unix(), time.Now().Add(-time.Minute).Unix()
		})), "nonce-1"},
		{"sem e-mail", f.sign(t, "RS256", mutate(func(c map[string]any) { delete(c, "email") })), "nonce-1"},
		{"malformado", "nao.e.jwt", "nonce-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if c, err := p.VerifyIDToken(ctx, tc.token, tc.nonce); err == nil {
				t.Fatalf("token deveria ser recusado: %+v", c)
			}
		})
	}
}

func TestRefresh(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	f.tokenResponse = map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "token_type": "Bearer", "expires_in": 300}
	tok, err := p.Refresh(context.Background(), "rt-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-2" || tok.RefreshToken != "rt-2" || tok.IDToken != "" {
		t.Fatalf("renovação inesperada: %+v", tok)
	}
	if form := f.tokenForms[0]; form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-1" {
		t.Fatalf("renovação sem o refresh token: %v", form)
	}
	f.tokenStatus = http.StatusBadRequest
	if _, err := p.Refresh(context.Background(), "rt-revogado"); err == nil {
		t.Fatal("renovação recusada deveria falhar")
	}
}

// O endereço de logout vai para o navegador: client_id e
// post_logout_redirect_uri, nunca o ID token.
func TestEndSessionURLCarriesNoTokens(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	raw := p.EndSessionURL("https://app.test/")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if !strings.HasPrefix(raw, f.srv.URL+"/logout?") || q.Get("client_id") != testClientID || q.Get("post_logout_redirect_uri") != "https://app.test/" ||
		q.Has("id_token_hint") || len(q) != 2 {
		t.Fatalf("URL de encerramento inesperada: %s", raw)
	}

	f.noEndpoints = true
	if got := f.provider(t).EndSessionURL("https://app.test/"); got != "https://app.test/" {
		t.Fatalf("sem end_session_endpoint, volta direto para a aplicação: %s", got)
	}
	p.endSession = "://invalido"
	if got := p.EndSessionURL("https://app.test/"); got != "https://app.test/" {
		t.Fatalf("endpoint inválido volta para a aplicação: %s", got)
	}
}

func TestRevokeTokens(t *testing.T) {
	f := newFakeProvider(t)
	p := f.provider(t)
	ctx := context.Background()
	if err := p.RevokeTokens(ctx, identity.Tokens{IDToken: "idt", AccessToken: "at", RefreshToken: "rt-1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.revocations) != 1 || f.revocations[0].Get("token") != "rt-1" || f.revocations[0].Get("token_type_hint") != "refresh_token" {
		t.Fatalf("revogação inesperada: %v", f.revocations)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testClientID+":"+url.QueryEscape(testClientSecret)))
	if f.revokeAuth[0] != want {
		t.Fatalf("o cliente confidencial deve se autenticar na revogação: %q", f.revokeAuth[0])
	}

	if err := p.RevokeTokens(ctx, identity.Tokens{AccessToken: "at"}); err != nil || len(f.revocations) != 1 {
		t.Fatalf("sem refresh token, nada a revogar: %v", err)
	}
	f.revokeStatus = http.StatusUnauthorized
	if err := p.RevokeTokens(ctx, identity.Tokens{RefreshToken: "rt-2"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("recusa do provedor deveria ser informada: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.RevokeTokens(cctx, identity.Tokens{RefreshToken: "rt-3"}); err == nil {
		t.Fatal("contexto cancelado deveria falhar")
	}

	f.noEndpoints = true
	if err := f.provider(t).RevokeTokens(ctx, identity.Tokens{RefreshToken: "rt-4"}); err != nil {
		t.Fatalf("sem revocation_endpoint, não faz nada: %v", err)
	}
}

func TestLazyProvider(t *testing.T) {
	f := newFakeProvider(t)
	ctx := context.Background()
	down := NewLazy(Config{Issuer: "http://127.0.0.1:1/realms/x", ClientID: testClientID, HTTPTimeout: time.Second})
	if err := down.Ready(ctx); !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("provedor indisponível: %v", err)
	}
	if down.AuthCodeURL("s", "n", "v") != "" || down.EndSessionURL("/volta") != "/volta" {
		t.Fatal("indisponível: sem URL de autorização e logout local")
	}
	if _, err := down.Exchange(ctx, "c", "v"); !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("Exchange: %v", err)
	}
	if _, err := down.VerifyIDToken(ctx, "t", "n"); !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if _, err := down.Refresh(ctx, "rt"); !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("Refresh: %v", err)
	}
	if err := down.RevokeTokens(ctx, identity.Tokens{RefreshToken: "rt"}); !apperr.IsKind(err, apperr.KindUnavailable) {
		t.Fatalf("RevokeTokens: %v", err)
	}

	up := NewLazy(Config{Issuer: f.issuer, ClientID: testClientID, ClientSecret: testClientSecret, HTTPTimeout: 5 * time.Second})
	if err := up.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if v := up.NewPKCEVerifier(); len(v) < 43 {
		t.Fatalf("verificador: %q", v)
	}
	if u := up.AuthCodeURL("s", "n", "v"); !strings.HasPrefix(u, f.srv.URL+"/auth?") {
		t.Fatalf("AuthCodeURL: %s", u)
	}
	if u := up.EndSessionURL("/volta"); !strings.Contains(u, "client_id="+testClientID) || strings.Contains(u, "id_token_hint") {
		t.Fatalf("EndSessionURL: %s", u)
	}
	f.tokenResponse = map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": f.sign(t, "RS256", f.claims("n"))}
	tok, err := up.Exchange(ctx, "c", "v")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.VerifyIDToken(ctx, tok.IDToken, "n"); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Refresh(ctx, "rt"); err != nil {
		t.Fatal(err)
	}
	if err := up.RevokeTokens(ctx, identity.Tokens{RefreshToken: "rt"}); err != nil || len(f.revocations) != 1 {
		t.Fatalf("RevokeTokens: %v", err)
	}
}
