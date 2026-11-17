// Package oidc implementa identity.IdentityProvider com OpenID Connect
// (Authorization Code + PKCE S256) usando go-oidc e x/oauth2.
//
// A validação do ID token verifica assinatura (somente RS256), emissor,
// audiência (client_id), expiração e nonce.
//
// Em ambientes onde o navegador e o backend acessam o provedor por endereços
// diferentes (por exemplo, Docker Compose), DiscoveryURL aponta para o
// endereço interno enquanto Issuer permanece o emissor público presente nos
// tokens; a validação do emissor continua obrigatória.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ricartefelipe/recorvia/internal/identity"
)

// Config configura o provedor.
type Config struct {
	Issuer       string
	DiscoveryURL string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPTimeout  time.Duration
}

// Provider é o adaptador OIDC.
type Provider struct {
	oauth      oauth2.Config
	verifier   *gooidc.IDTokenVerifier
	endSession string
	client     *http.Client
}

// New descobre o provedor e prepara o verificador.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	ctx = gooidc.ClientContext(ctx, client)
	discovery := cfg.Issuer
	if cfg.DiscoveryURL != "" && cfg.DiscoveryURL != cfg.Issuer {
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.Issuer)
		discovery = cfg.DiscoveryURL
	}
	p, err := gooidc.NewProvider(ctx, discovery)
	if err != nil {
		return nil, fmt.Errorf("oidc: descoberta: %w", err)
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err != nil {
		return nil, fmt.Errorf("oidc: metadados: %w", err)
	}
	return &Provider{
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: []string{gooidc.ScopeOpenID, "profile", "email"}},
		verifier:   p.Verifier(&gooidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: []string{gooidc.RS256}}),
		endSession: meta.EndSession,
		client:     client,
	}, nil
}

func (p *Provider) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, p.client)
}

// NewPKCEVerifier gera um verificador PKCE (RFC 7636).
func (p *Provider) NewPKCEVerifier() string { return oauth2.GenerateVerifier() }

// AuthCodeURL monta a URL de autorização com state, nonce e desafio S256.
func (p *Provider) AuthCodeURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), gooidc.Nonce(nonce))
}

// Exchange troca o código usando o verificador PKCE.
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (identity.Tokens, error) {
	tok, err := p.oauth.Exchange(p.ctx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return identity.Tokens{}, fmt.Errorf("oidc: troca de código: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return identity.Tokens{}, errors.New("oidc: resposta sem id_token")
	}
	return identity.Tokens{IDToken: raw, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Expiry: tok.Expiry}, nil
}

// VerifyIDToken valida o ID token e o nonce esperado.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, nonce string) (identity.Claims, error) {
	tok, err := p.verifier.Verify(p.ctx(ctx), raw)
	if err != nil {
		return identity.Claims{}, fmt.Errorf("oidc: id_token inválido: %w", err)
	}
	if tok.Nonce == "" || tok.Nonce != nonce {
		return identity.Claims{}, errors.New("oidc: nonce divergente")
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Preferred     string `json:"preferred_username"`
	}
	if err := tok.Claims(&c); err != nil {
		return identity.Claims{}, err
	}
	if c.Email == "" {
		return identity.Claims{}, errors.New("oidc: id_token sem e-mail")
	}
	name := c.Name
	if name == "" {
		name = c.Preferred
	}
	return identity.Claims{Issuer: tok.Issuer, Subject: tok.Subject, Email: c.Email, EmailVerified: c.EmailVerified, Name: name}, nil
}

// Refresh renova os tokens. Falha indica sessão revogada ou usuário
// desativado no provedor.
func (p *Provider) Refresh(ctx context.Context, refreshToken string) (identity.Tokens, error) {
	src := p.oauth.TokenSource(p.ctx(ctx), &oauth2.Token{RefreshToken: refreshToken, Expiry: time.Unix(1, 0)})
	tok, err := src.Token()
	if err != nil {
		return identity.Tokens{}, fmt.Errorf("oidc: renovação: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string) // opcional na renovação
	return identity.Tokens{IDToken: raw, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, Expiry: tok.Expiry}, nil
}

// EndSessionURL monta a URL de logout no provedor (RP-initiated logout).
func (p *Provider) EndSessionURL(idTokenHint, postLogoutRedirect string) string {
	if p.endSession == "" {
		return postLogoutRedirect
	}
	u, err := url.Parse(p.endSession)
	if err != nil {
		return postLogoutRedirect
	}
	q := u.Query()
	q.Set("client_id", p.oauth.ClientID)
	q.Set("post_logout_redirect_uri", postLogoutRedirect)
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
