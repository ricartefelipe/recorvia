package identity

import (
	"context"
	"errors"
	"fmt"
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
}

// DefaultPolicy é a política padrão.
func DefaultPolicy() Policy {
	return Policy{Absolute: 12 * time.Hour, Idle: 2 * time.Hour, LoginFlowTTL: 10 * time.Minute, TouchInterval: time.Minute, RequireVerifiedEmail: true}
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

// BeginLogin cria o estado de login e retorna a URL de autorização.
func (s *Service) BeginLogin(ctx context.Context, returnTo string) (string, error) {
	f := LoginFlow{
		State: RandomToken(32), Nonce: RandomToken(32), PKCEVerifier: s.idp.NewPKCEVerifier(),
		ReturnTo: SafeReturnPath(returnTo), CreatedAt: s.clock.Now(),
	}
	if err := s.sessions.SaveLoginFlow(ctx, f, s.policy.LoginFlowTTL); err != nil {
		return "", fmt.Errorf("identity: estado de login: %w", err)
	}
	return s.idp.AuthCodeURL(f.State, f.Nonce, f.PKCEVerifier), nil
}

// CompleteLogin valida o callback, troca o código com o verificador PKCE,
// valida o ID token e cria uma sessão nova (rotação: nenhum identificador
// anterior é reaproveitado).
func (s *Service) CompleteLogin(ctx context.Context, state, code string) (Session, User, string, error) {
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
	sess := Session{
		ID: RandomToken(32), UserID: user.ID, CSRFToken: RandomToken(32),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.policy.Absolute), Tokens: tokens,
	}
	if err := s.sessions.Save(ctx, sess, s.ttl(sess, now)); err != nil {
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
	if err != nil || user.Status != "active" {
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
		if err := s.sessions.Save(ctx, sess, s.ttl(sess, now)); err != nil {
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
	now := s.clock.Now()
	next := sess
	next.ID = RandomToken(32)
	next.CSRFToken = RandomToken(32)
	next.ActiveTenantID = &tenantID
	next.LastSeenAt = now
	if err := s.sessions.Save(ctx, next, s.ttl(next, now)); err != nil {
		return Session{}, err
	}
	if err := s.sessions.Delete(ctx, sess.ID); err != nil {
		return Session{}, err
	}
	return next, nil
}

// Logout encerra a sessão local e devolve a URL de encerramento no provedor.
func (s *Service) Logout(ctx context.Context, sess Session, postLogoutRedirect string) (string, error) {
	if err := s.sessions.Delete(ctx, sess.ID); err != nil {
		return "", err
	}
	return s.idp.EndSessionURL(sess.Tokens.IDToken, postLogoutRedirect), nil
}

// RevokeAll encerra todas as sessões de um usuário.
func (s *Service) RevokeAll(ctx context.Context, userID uuid.UUID) error {
	return s.sessions.DeleteAllForUser(ctx, userID)
}

// User retorna um usuário.
func (s *Service) User(ctx context.Context, id uuid.UUID) (User, error) { return s.users.User(ctx, id) }

// IsPlatformStaff informa se o usuário integra a equipe da plataforma.
func (s *Service) IsPlatformStaff(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.users.IsPlatformStaff(ctx, id)
}
