package gateways

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ricartefelipe/recorvia/internal/payments"
)

// Factory cria o adaptador de um provedor a partir da conta.
type Factory struct {
	Provider string
	Gateway  func(a payments.ProviderAccount, httpc *http.Client) (payments.Gateway, error)
	Verifier payments.WebhookVerifier
}

// Resolver implementa payments.GatewayResolver.
type Resolver struct {
	factories map[string]Factory
	httpc     *http.Client
}

// NewResolver registra os provedores disponíveis nesta instalação.
func NewResolver(timeout time.Duration, factories ...Factory) *Resolver {
	r := &Resolver{factories: map[string]Factory{}, httpc: NewHTTPClient(timeout)}
	for _, f := range factories {
		r.factories[f.Provider] = f
	}
	return r
}

// Gateway instancia o adaptador para a conta.
func (r *Resolver) Gateway(a payments.ProviderAccount) (payments.Gateway, error) {
	f, ok := r.factories[a.Provider]
	if !ok {
		return nil, fmt.Errorf("gateways: provedor %q indisponível", a.Provider)
	}
	return f.Gateway(a, r.httpc)
}

// Verifier retorna o verificador de webhooks do provedor.
func (r *Resolver) Verifier(provider string) (payments.WebhookVerifier, error) {
	f, ok := r.factories[provider]
	if !ok {
		return nil, fmt.Errorf("gateways: provedor %q indisponível", provider)
	}
	return f.Verifier, nil
}

// Supports informa se o provedor está habilitado.
func (r *Resolver) Supports(provider string) bool {
	_, ok := r.factories[provider]
	return ok
}

// Providers lista os provedores habilitados.
func (r *Resolver) Providers() []string {
	out := make([]string, 0, len(r.factories))
	for p := range r.factories {
		out = append(out, p)
	}
	return out
}
