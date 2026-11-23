package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
)

// Policy define a duração das sessões.
type Policy struct {
	// Absolute é a duração máxima desde o login.
	Absolute time.Duration
	// Idle encerra a sessão após inatividade.
	Idle time.Duration
	// LoginFlowTTL limita o tempo entre o redirecionamento e o callback.
	LoginFlowTTL time.Duration
	// TouchInterval evita gravar a sessão a cada requisição.
	TouchInterval time.Duration
	// RequireVerifiedEmail exige e-mail verificado no provedor.
	RequireVerifiedEmail bool
	// RevokeTimeout limita o tempo total da revogação dos tokens no
	// provedor em "encerrar todas as sessões" (feita em paralelo).
	RevokeTimeout time.Duration
}

// DefaultPolicy é a política padrão.
func DefaultPolicy() Policy {
	return Policy{Absolute: 12 * time.Hour, Idle: 2 * time.Hour, LoginFlowTTL: 10 * time.Minute, TouchInterval: time.Minute, RequireVerifiedEmail: true,
		RevokeTimeout: 5 * time.Second}
}

// Service implementa login, autenticação por sessão, troca de organização e
// logout.
type Service struct {
	idp      IdentityProvider
	sessions SessionStore
	users    UserStore
	clock    clock.Clock
	policy   Policy
}

// NewService cria o serviço.
func NewService(idp IdentityProvider, sessions SessionStore, users UserStore, c clock.Clock, p Policy) *Service {
	return &Service{idp: idp, sessions: sessions, users: users, clock: c, policy: p}
}

// LoginFlowTTL é a validade do fluxo de login (e do cookie que o vincula
// ao navegador).
func (s *Service) LoginFlowTTL() time.Duration { return s.policy.LoginFlowTTL }

// LoginStart é o início de um login: o endereço de autorização no provedor e
// o state do fluxo, que o chamador usa para nomear o cookie de vínculo.
type LoginStart struct {
	URL   string
	State string
}

// BeginLogin cria o estado de login e retorna a URL de autorização. O fluxo
// fica vinculado ao navegador pelo binding (NewBrowserBinding, um por
// fluxo), guardado pelo chamador num cookie HttpOnly: sem ele, quem
// iniciasse um login e enviasse a outra pessoa o endereço de callback com o
// próprio código faria o navegador dela entrar na conta de quem atacou
// (login CSRF).
func (s *Service) BeginLogin(ctx context.Context, returnTo, binding string) (LoginStart, error) {
	if !ValidBrowserBinding(binding) {
		return LoginStart{}, apperr.Invalid("login_binding_invalid", "Não foi possível iniciar o login. Tente novamente.")
	}
	f := LoginFlow{
		State: RandomToken(32), Nonce: RandomToken(32), PKCEVerifier: s.idp.NewPKCEVerifier(),
		ReturnTo: SafeReturnPath(returnTo), CreatedAt: s.clock.Now(), BindingHash: bindingHash(binding),
	}
	target := s.idp.AuthCodeURL(f.State, f.Nonce, f.PKCEVerifier)
	if target == "" {
		return LoginStart{}, apperr.Unavailable("identity_provider_unavailable", "Provedor de identidade indisponível. Tente novamente em instantes.")
	}
	if err := s.sessions.SaveLoginFlow(ctx, f, s.policy.LoginFlowTTL); err != nil {
		return LoginStart{}, fmt.Errorf("identity: estado de login: %w", err)
	}
	return LoginStart{URL: target, State: f.State}, nil
}

// CompleteLogin valida o callback, confere que ele chega ao navegador que
// iniciou o login (binding), troca o código com o verificador PKCE, valida o
// ID token e cria uma sessão nova (rotação: nenhum identificador anterior é
// reaproveitado).
func (s *Service) CompleteLogin(ctx context.Context, state, code, binding string) (Session, User, string, error) {
	invalid := apperr.Unauthenticated("login_invalid", "Não foi possível concluir o login. Tente novamente.")
	if state == "" || code == "" {
		return Session{}, User{}, "", invalid
	}
	flow, err := s.sessions.TakeLoginFlow(ctx, state)
	if err != nil {
		if errors.Is(err, ErrLoginFlowNotFound) {
			return Session{}, User{}, "", invalid
		}
		return Session{}, User{}, "", err
	}
	// O fluxo já foi consumido: um callback em outro navegador inutiliza o
	// state e não troca o código.
	if !flow.boundTo(binding) {
		return Session{}, User{}, "", invalid
	}
	tokens, err := s.idp.Exchange(ctx, code, flow.PKCEVerifier)
	if err != nil {
		return Session{}, User{}, "", invalid.Wrap(err)
	}
	claims, err := s.idp.VerifyIDToken(ctx, tokens.IDToken, flow.Nonce)
	if err != nil {
		return Session{}, User{}, "", invalid.Wrap(err)
	}
	if s.policy.RequireVerifiedEmail && !claims.EmailVerified {
		return Session{}, User{}, "", apperr.Forbidden("email_not_verified", "Confirme seu e-mail no provedor de identidade antes de entrar.")
	}
	now := s.clock.Now()
	user, err := s.users.UpsertFromClaims(ctx, ids.New(), claims, now)
	if err != nil {
		return Session{}, User{}, "", err
	}
	if user.Status != "active" {
		return Session{}, User{}, "", apperr.Forbidden("user_disabled", "Usuário desativado.")
	}
	gen, _, err := s.sessions.Generations(ctx, user.ID, nil)
	if err != nil {
		return Session{}, User{}, "", err
	}
	sess := Session{
		ID: RandomToken(32), UserID: user.ID, CSRFToken: RandomToken(32),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.policy.Absolute), Tokens: tokens, Generation: gen,
	}
	if err := s.sessions.Create(ctx, sess, s.ttl(sess, now)); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			// As sessões do usuário foram revogadas durante o login.
			return Session{}, User{}, "", invalid
		}
		return Session{}, User{}, "", err
	}
	return sess, user, flow.ReturnTo, nil
}

func (s *Service) ttl(sess Session, now time.Time) time.Duration {
	return min(s.policy.Idle, sess.ExpiresAt.Sub(now))
}

// Authenticate valida a sessão: expiração absoluta, inatividade, situação do
// usuário e renovação dos tokens do provedor. Falha de renovação (usuário
// desativado ou sessão revogada no provedor) encerra a sessão local.
func (s *Service) Authenticate(ctx context.Context, sessionID string) (Session, User, error) {
	unauth := apperr.Unauthenticated("session_invalid", "Sua sessão expirou. Entre novamente.")
	if sessionID == "" {
		return Session{}, User{}, unauth
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, User{}, unauth
		}
		return Session{}, User{}, apperr.Unavailable("session_store_unavailable", "Serviço de sessão indisponível.").Wrap(err)
	}
	now := s.clock.Now()
	if !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) >= s.policy.Idle {
		_ = s.sessions.Delete(ctx, sessionID) // melhor esforço; a TTL do armazenamento também expira
		return Session{}, User{}, unauth
	}
	user, err := s.users.User(ctx, sess.UserID)
	if err != nil {
		_ = s.sessions.Delete(ctx, sessionID)
		return Session{}, User{}, unauth
	}
	if user.Status != "active" {
		// Usuário desativado: todas as sessões dele são encerradas, não só a
		// desta requisição (melhor esforço; as demais também seriam
		// recusadas aqui).
		_ = s.sessions.DeleteAllForUser(ctx, user.ID)
		_ = s.sessions.Delete(ctx, sessionID)
		return Session{}, User{}, unauth
	}
	changed := false
	if sess.Tokens.RefreshToken != "" && !sess.Tokens.Expiry.IsZero() && !now.Before(sess.Tokens.Expiry) {
		tokens, err := s.idp.Refresh(ctx, sess.Tokens.RefreshToken)
		if err != nil {
			_ = s.sessions.Delete(ctx, sessionID)
			return Session{}, User{}, unauth
		}
		if tokens.IDToken == "" {
			tokens.IDToken = sess.Tokens.IDToken
		}
		sess.Tokens = tokens
		changed = true
	}
	if changed || now.Sub(sess.LastSeenAt) >= s.policy.TouchInterval {
		sess.LastSeenAt = now
		// Gravação condicional: se a sessão foi revogada enquanto esta
		// requisição estava em voo, ela não é recriada.
		if err := s.sessions.Save(ctx, sess, s.ttl(sess, now)); err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				return Session{}, User{}, unauth
			}
			return Session{}, User{}, apperr.Unavailable("session_store_unavailable", "Serviço de sessão indisponível.").Wrap(err)
		}
	}
	return sess, user, nil
}

// AccessChecker valida a associação ativa do usuário ao tenant.
type AccessChecker func(ctx context.Context, tenantID, userID uuid.UUID) error

// SwitchTenant troca a organização ativa após validar a associação e
// rotaciona a sessão (novo identificador e novo token CSRF), descartando a
// anterior.
func (s *Service) SwitchTenant(ctx context.Context, sess Session, tenantID uuid.UUID, check AccessChecker) (Session, error) {
	if err := check(ctx, tenantID, sess.UserID); err != nil {
		return Session{}, err
	}
	_, tenantGen, err := s.sessions.Generations(ctx, sess.UserID, &tenantID)
	if err != nil {
		return Session{}, err
	}
	now := s.clock.Now()
	next := sess
	next.ID = RandomToken(32)
	next.CSRFToken = RandomToken(32)
	next.ActiveTenantID = &tenantID
	next.TenantGeneration = tenantGen
	next.LastSeenAt = now
	// A sessão nova herda a geração da anterior: se as sessões do usuário
	// foram revogadas durante a troca, a nova não é gravada.
	if err := s.sessions.Create(ctx, next, s.ttl(next, now)); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, apperr.Unauthenticated("session_invalid", "Sua sessão expirou. Entre novamente.")
		}
		return Session{}, err
	}
	if err := s.sessions.Delete(ctx, sess.ID); err != nil {
		// Sem revogar a anterior, a nova sessão é descartada: o usuário
		// continua apenas com a sessão original.
		return Session{}, errors.Join(err, s.sessions.Delete(ctx, next.ID))
	}
	return next, nil
}

// LogoutResult é o resultado do encerramento de sessão.
type LogoutResult struct {
	// RedirectURL é o endereço de encerramento no provedor, sem tokens.
	RedirectURL string
	// ProviderErr registra a falha da revogação dos tokens no provedor
	// (melhor esforço: a sessão local já foi encerrada).
	ProviderErr error
}

// Logout encerra a sessão local, revoga no provedor, pelo servidor, o
// refresh token da sessão e devolve o endereço de encerramento da sessão do
// navegador no provedor (client_id e post_logout_redirect_uri, sem o ID
// token: os tokens do provedor não saem do servidor).
func (s *Service) Logout(ctx context.Context, sess Session, postLogoutRedirect string) (LogoutResult, error) {
	if err := s.sessions.Delete(ctx, sess.ID); err != nil {
		return LogoutResult{}, err
	}
	return LogoutResult{RedirectURL: s.idp.EndSessionURL(postLogoutRedirect), ProviderErr: s.idp.RevokeTokens(ctx, sess.Tokens)}, nil
}

// LogoutAll encerra todas as sessões do usuário (inclusive a atual) e
// revoga no provedor os refresh tokens de cada uma.
func (s *Service) LogoutAll(ctx context.Context, sess Session, postLogoutRedirect string) (LogoutResult, error) {
	all, err := s.sessions.ListForUser(ctx, sess.UserID)
	if err != nil {
		return LogoutResult{}, err
	}
	if err := s.sessions.DeleteAllForUser(ctx, sess.UserID); err != nil {
		return LogoutResult{}, err
	}
	if err := s.sessions.Delete(ctx, sess.ID); err != nil {
		return LogoutResult{}, err
	}
	return LogoutResult{RedirectURL: s.idp.EndSessionURL(postLogoutRedirect), ProviderErr: s.revokeAll(ctx, append(all, sess))}, nil
}

// revokeAll revoga no provedor, em paralelo e com prazo total limitado, os
// refresh tokens distintos das sessões.
func (s *Service) revokeAll(ctx context.Context, sessions []Session) error {
	timeout := s.policy.RevokeTimeout
	if timeout <= 0 {
		timeout = DefaultPolicy().RevokeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	seen := map[string]bool{}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, sess := range sessions {
		rt := sess.Tokens.RefreshToken
		if rt == "" || seen[rt] {
			continue
		}
		seen[rt] = true
		wg.Go(func() {
			if err := s.idp.RevokeTokens(ctx, sess.Tokens); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// RevokeTenantSessions encerra as sessões do usuário cuja organização ativa
// é o tenant informado (membro removido). As demais sessões continuam: o
// usuário pode participar de outras organizações. Devolve quantas foram
// encerradas.
func (s *Service) RevokeTenantSessions(ctx context.Context, userID, tenantID uuid.UUID) (int, error) {
	// A geração do usuário no tenant muda antes da remoção: uma sessão com o
	// tenant ativo regravada por uma requisição em voo continua recusada.
	if err := s.sessions.InvalidateTenant(ctx, userID, tenantID); err != nil {
		return 0, err
	}
	all, err := s.sessions.ListForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sess := range all {
		if sess.ActiveTenantID == nil || *sess.ActiveTenantID != tenantID {
			continue
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// User retorna um usuário.
func (s *Service) User(ctx context.Context, id uuid.UUID) (User, error) { return s.users.User(ctx, id) }

// IsPlatformStaff informa se o usuário integra a equipe da plataforma.
func (s *Service) IsPlatformStaff(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.users.IsPlatformStaff(ctx, id)
}
