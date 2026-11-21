package oidc

import (
	"context"
	"sync"
	"time"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// Lazy inicializa o provedor sob demanda e tenta novamente após falhas, para
// que a indisponibilidade do provedor de identidade não impeça a API de
// subir (nem provoque reinícios em cascata).
type Lazy struct {
	cfg   Config
	mu    sync.Mutex
	p     *Provider
	after time.Time
}

// NewLazy cria o provedor preguiçoso.
func NewLazy(cfg Config) *Lazy { return &Lazy{cfg: cfg} }

var errUnavailable = apperr.Unavailable("identity_provider_unavailable", "Provedor de identidade indisponível. Tente novamente em instantes.")

func (l *Lazy) get(ctx context.Context) (*Provider, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.p != nil {
		return l.p, nil
	}
	if time.Now().Before(l.after) {
		return nil, errUnavailable
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	p, err := New(ictx, l.cfg)
	if err != nil {
		l.after = time.Now().Add(5 * time.Second)
		return nil, errUnavailable.Wrap(err)
	}
	l.p = p
	return p, nil
}

// Ready informa se o provedor já foi descoberto (diagnóstico, não readiness).
func (l *Lazy) Ready(ctx context.Context) error {
	_, err := l.get(ctx)
	return err
}

// AuthCodeURL delega ao provedor.
func (l *Lazy) AuthCodeURL(state, nonce, verifier string) string {
	p, err := l.get(context.Background())
	if err != nil {
		return ""
	}
	return p.AuthCodeURL(state, nonce, verifier)
}

// Exchange delega ao provedor.
func (l *Lazy) Exchange(ctx context.Context, code, verifier string) (identity.Tokens, error) {
	p, err := l.get(ctx)
	if err != nil {
		return identity.Tokens{}, err
	}
	return p.Exchange(ctx, code, verifier)
}

// VerifyIDToken delega ao provedor.
func (l *Lazy) VerifyIDToken(ctx context.Context, raw, nonce string) (identity.Claims, error) {
	p, err := l.get(ctx)
	if err != nil {
		return identity.Claims{}, err
	}
	return p.VerifyIDToken(ctx, raw, nonce)
}

// Refresh delega ao provedor.
func (l *Lazy) Refresh(ctx context.Context, rt string) (identity.Tokens, error) {
	p, err := l.get(ctx)
	if err != nil {
		return identity.Tokens{}, err
	}
	return p.Refresh(ctx, rt)
}

// EndSessionURL delega ao provedor.
func (l *Lazy) EndSessionURL(redirect string) string {
	p, err := l.get(context.Background())
	if err != nil {
		return redirect
	}
	return p.EndSessionURL(redirect)
}

// RevokeTokens delega ao provedor.
func (l *Lazy) RevokeTokens(ctx context.Context, tokens identity.Tokens) error {
	p, err := l.get(ctx)
	if err != nil {
		return err
	}
	return p.RevokeTokens(ctx, tokens)
}

// NewPKCEVerifier gera o verificador PKCE.
func (l *Lazy) NewPKCEVerifier() string { return (&Provider{}).NewPKCEVerifier() }
