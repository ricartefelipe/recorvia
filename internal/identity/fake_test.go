package identity

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var errBoom = errors.New("falha simulada")

// fakeIDP simula o provedor OIDC e registra os argumentos recebidos.
type fakeIDP struct {
	authURL     string // vazio simula provedor indisponível
	verifier    string
	tokens      Tokens
	exchangeErr error
	claims      Claims
	verifyErr   error
	refreshed   Tokens
	refreshErr  error

	gotState, gotNonce, gotVerifier string
	gotCode, gotExchangeVerifier    string
	gotIDToken, gotExpectedNonce    string
	gotRefreshToken                 string
	refreshCalls                    int
	revokeErr                       error
	mu                              sync.Mutex
	revoked                         []string
	// revokeBlock faz a revogação aguardar o prazo do contexto, contando as
	// chamadas simultâneas.
	revokeBlock           bool
	inFlight, maxInFlight int
}

func (p *fakeIDP) AuthCodeURL(state, nonce, verifier string) string {
	p.gotState, p.gotNonce, p.gotVerifier = state, nonce, verifier
	if p.authURL == "" {
		return ""
	}
	return p.authURL + "?state=" + url.QueryEscape(state)
}

func (p *fakeIDP) Exchange(_ context.Context, code, verifier string) (Tokens, error) {
	p.gotCode, p.gotExchangeVerifier = code, verifier
	return p.tokens, p.exchangeErr
}

func (p *fakeIDP) VerifyIDToken(_ context.Context, raw, nonce string) (Claims, error) {
	p.gotIDToken, p.gotExpectedNonce = raw, nonce
	return p.claims, p.verifyErr
}

func (p *fakeIDP) Refresh(_ context.Context, refreshToken string) (Tokens, error) {
	p.refreshCalls++
	p.gotRefreshToken = refreshToken
	return p.refreshed, p.refreshErr
}

func (p *fakeIDP) EndSessionURL(redirect string) string {
	return "https://idp.example/logout?client_id=recorvia-web&post_logout_redirect_uri=" + url.QueryEscape(redirect)
}

func (p *fakeIDP) RevokeTokens(ctx context.Context, t Tokens) error {
	if p.revokeBlock {
		p.mu.Lock()
		p.inFlight++
		p.maxInFlight = max(p.maxInFlight, p.inFlight)
		p.mu.Unlock()
		<-ctx.Done()
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.RefreshToken != "" {
		p.revoked = append(p.revoked, t.RefreshToken)
		slices.Sort(p.revoked)
	}
	return p.revokeErr
}

func (p *fakeIDP) NewPKCEVerifier() string { return p.verifier }

// fakeSessions guarda sessões e fluxos de login em memória.
type fakeSessions struct {
	flows    map[string]LoginFlow
	flowTTL  time.Duration
	sessions map[string]Session
	ttls     map[string]time.Duration
	saves    int
	deleted  []string
	revoked  []uuid.UUID
	fail     map[string]error
	// gens e tenantGens são as gerações correntes por usuário e por usuário
	// no tenant.
	gens       map[uuid.UUID]int64
	tenantGens map[string]int64
	// afterGet é chamado depois de cada leitura (para intercalar revogações).
	afterGet func()
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{flows: map[string]LoginFlow{}, sessions: map[string]Session{},
		ttls: map[string]time.Duration{}, fail: map[string]error{}, gens: map[uuid.UUID]int64{}, tenantGens: map[string]int64{}}
}

func (s *fakeSessions) SaveLoginFlow(_ context.Context, f LoginFlow, ttl time.Duration) error {
	if err := s.fail["SaveLoginFlow"]; err != nil {
		return err
	}
	s.flows[f.State] = f
	s.flowTTL = ttl
	return nil
}

func (s *fakeSessions) TakeLoginFlow(_ context.Context, state string) (LoginFlow, error) {
	if err := s.fail["TakeLoginFlow"]; err != nil {
		return LoginFlow{}, err
	}
	f, ok := s.flows[state]
	if !ok {
		return LoginFlow{}, ErrLoginFlowNotFound
	}
	delete(s.flows, state)
	return f, nil
}

func tenantGenKey(userID, tenantID uuid.UUID) string {
	return userID.String() + "/" + tenantID.String()
}

// current confere as gerações da sessão, como o armazenamento real.
func (s *fakeSessions) current(sess Session) bool {
	if s.gens[sess.UserID] != sess.Generation {
		return false
	}
	return sess.ActiveTenantID == nil || s.tenantGens[tenantGenKey(sess.UserID, *sess.ActiveTenantID)] == sess.TenantGeneration
}

func (s *fakeSessions) Create(_ context.Context, sess Session, ttl time.Duration) error {
	if err := s.fail["Create"]; err != nil {
		return err
	}
	if !s.current(sess) {
		return ErrSessionNotFound
	}
	s.saves++
	s.sessions[sess.ID] = sess
	s.ttls[sess.ID] = ttl
	return nil
}

func (s *fakeSessions) Save(_ context.Context, sess Session, ttl time.Duration) error {
	if err := s.fail["Save"]; err != nil {
		return err
	}
	if _, ok := s.sessions[sess.ID]; !ok || !s.current(sess) {
		return ErrSessionNotFound
	}
	s.saves++
	s.sessions[sess.ID] = sess
	s.ttls[sess.ID] = ttl
	return nil
}

func (s *fakeSessions) Generations(_ context.Context, userID uuid.UUID, tenantID *uuid.UUID) (int64, int64, error) {
	if err := s.fail["Generations"]; err != nil {
		return 0, 0, err
	}
	var tg int64
	if tenantID != nil {
		tg = s.tenantGens[tenantGenKey(userID, *tenantID)]
	}
	return s.gens[userID], tg, nil
}

func (s *fakeSessions) InvalidateTenant(_ context.Context, userID, tenantID uuid.UUID) error {
	if err := s.fail["InvalidateTenant"]; err != nil {
		return err
	}
	s.tenantGens[tenantGenKey(userID, tenantID)]++
	return nil
}

func (s *fakeSessions) Get(_ context.Context, id string) (Session, error) {
	if err := s.fail["Get"]; err != nil {
		return Session{}, err
	}
	sess, ok := s.sessions[id]
	if s.afterGet != nil {
		defer s.afterGet()
	}
	if !ok || !s.current(sess) {
		return Session{}, ErrSessionNotFound
	}
	return sess, nil
}

func (s *fakeSessions) Delete(_ context.Context, id string) error {
	if err := s.fail["Delete"]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, id)
	delete(s.sessions, id)
	return nil
}

func (s *fakeSessions) ListForUser(_ context.Context, userID uuid.UUID) ([]Session, error) {
	if err := s.fail["ListForUser"]; err != nil {
		return nil, err
	}
	var out []Session
	for _, sess := range s.sessions {
		if sess.UserID == userID {
			out = append(out, sess)
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *fakeSessions) DeleteAllForUser(_ context.Context, userID uuid.UUID) error {
	if err := s.fail["DeleteAllForUser"]; err != nil {
		return err
	}
	s.revoked = append(s.revoked, userID)
	s.gens[userID]++
	for id, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, id)
		}
	}
	return nil
}

// fakeUsers guarda usuários por identificador e por (emissor, sujeito).
type fakeUsers struct {
	byID      map[uuid.UUID]User
	bySubject map[string]uuid.UUID
	staff     map[uuid.UUID]bool
	upsertErr error
	userErr   error
	staffErr  error
	// statusOnCreate define a situação de usuários criados no login.
	statusOnCreate string
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{byID: map[uuid.UUID]User{}, bySubject: map[string]uuid.UUID{}, staff: map[uuid.UUID]bool{}, statusOnCreate: "active"}
}

func (u *fakeUsers) UpsertFromClaims(_ context.Context, id uuid.UUID, c Claims, now time.Time) (User, error) {
	if u.upsertErr != nil {
		return User{}, u.upsertErr
	}
	key := c.Issuer + "|" + c.Subject
	if existing, ok := u.bySubject[key]; ok {
		usr := u.byID[existing]
		usr.Email, usr.DisplayName, usr.LastLoginAt = c.Email, c.Name, &now
		u.byID[existing] = usr
		return usr, nil
	}
	usr := User{ID: id, Issuer: c.Issuer, Subject: c.Subject, Email: c.Email, DisplayName: c.Name,
		Status: u.statusOnCreate, CreatedAt: now, LastLoginAt: &now}
	u.byID[id] = usr
	u.bySubject[key] = id
	return usr, nil
}

func (u *fakeUsers) User(_ context.Context, id uuid.UUID) (User, error) {
	if u.userErr != nil {
		return User{}, u.userErr
	}
	usr, ok := u.byID[id]
	if !ok {
		return User{}, errors.New("usuário não encontrado")
	}
	return usr, nil
}

func (u *fakeUsers) IsPlatformStaff(_ context.Context, id uuid.UUID) (bool, error) {
	if u.staffErr != nil {
		return false, u.staffErr
	}
	return u.staff[id], nil
}
