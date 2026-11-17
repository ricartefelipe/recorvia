// Package identity cuida da autenticação de usuários com OpenID Connect
// (Authorization Code + PKCE), de sessões mantidas no servidor e da proteção
// CSRF. O navegador recebe apenas um identificador opaco de sessão em cookie
// HttpOnly; tokens do provedor de identidade nunca saem do servidor.
//
// O ID token é validado uma única vez no login (assinatura, emissor,
// audiência, expiração, algoritmo e nonce) e não é aceito como credencial da
// API.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound indica sessão inexistente, expirada ou revogada.
var ErrSessionNotFound = errors.New("identity: sessão não encontrada")

// ErrLoginFlowNotFound indica estado OAuth desconhecido ou já utilizado.
var ErrLoginFlowNotFound = errors.New("identity: fluxo de login não encontrado")

// User é a identidade global de uma pessoa (pode participar de vários tenants).
type User struct {
	ID          uuid.UUID
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Status      string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// Claims são as informações extraídas de um ID token validado.
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Tokens são os tokens emitidos pelo provedor de identidade.
type Tokens struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// IdentityProvider é a porta para o provedor OIDC.
type IdentityProvider interface {
	AuthCodeURL(state, nonce, pkceVerifier string) string
	Exchange(ctx context.Context, code, pkceVerifier string) (Tokens, error)
	VerifyIDToken(ctx context.Context, rawIDToken, expectedNonce string) (Claims, error)
	Refresh(ctx context.Context, refreshToken string) (Tokens, error)
	EndSessionURL(idTokenHint, postLogoutRedirect string) string
	NewPKCEVerifier() string
}

// LoginFlow guarda o estado temporário entre o redirecionamento e o callback.
type LoginFlow struct {
	State        string
	Nonce        string
	PKCEVerifier string
	ReturnTo     string
	CreatedAt    time.Time
}

// Session é a sessão do navegador mantida no servidor.
type Session struct {
	ID             string
	UserID         uuid.UUID
	ActiveTenantID *uuid.UUID
	CSRFToken      string
	CreatedAt      time.Time
	LastSeenAt     time.Time
	ExpiresAt      time.Time
	Tokens         Tokens
}

// SessionStore persiste sessões e fluxos de login.
type SessionStore interface {
	SaveLoginFlow(ctx context.Context, f LoginFlow, ttl time.Duration) error
	// TakeLoginFlow lê e remove atomicamente (uso único).
	TakeLoginFlow(ctx context.Context, state string) (LoginFlow, error)
	Save(ctx context.Context, s Session, ttl time.Duration) error
	Get(ctx context.Context, id string) (Session, error)
	Delete(ctx context.Context, id string) error
	DeleteAllForUser(ctx context.Context, userID uuid.UUID) error
}

// UserStore persiste usuários.
type UserStore interface {
	UpsertFromClaims(ctx context.Context, id uuid.UUID, c Claims, now time.Time) (User, error)
	User(ctx context.Context, id uuid.UUID) (User, error)
	IsPlatformStaff(ctx context.Context, id uuid.UUID) (bool, error)
}

// RandomToken gera n bytes aleatórios em base64url.
func RandomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	return base64.RawURLEncoding.EncodeToString(b)
}

// ValidCSRF compara o token recebido com o da sessão em tempo constante.
func (s Session) ValidCSRF(token string) bool {
	if token == "" || s.CSRFToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRFToken)) == 1
}

// SafeReturnPath aceita somente caminhos relativos da própria aplicação,
// impedindo redirecionamento aberto após o login.
func SafeReturnPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") ||
		strings.ContainsAny(p, "\r\n\\") || len(p) > 512 {
		return "/"
	}
	return p
}
