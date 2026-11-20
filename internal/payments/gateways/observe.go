package gateways

import (
	"context"

	"github.com/ricartefelipe/recorvia/internal/payments"
)

// ChargeObserver recebe as falhas das chamadas ao provedor que criam uma
// cobrança (cadastro do cliente e criação da cobrança), por provedor e tipo
// de falha. Os rótulos não identificam tenant, cliente nem fatura.
type ChargeObserver interface {
	ChargeFailed(provider, kind string)
}

// FailureKind devolve o rótulo estável da classificação de uma falha do
// provedor: permanent, retryable, ambiguous ou not_found. Erros não
// classificados são ambíguos, como em payments.GatewayErrorKind.
func FailureKind(err error) string {
	switch payments.GatewayErrorKind(err) {
	case payments.ErrKindPermanent:
		return "permanent"
	case payments.ErrKindRetryable:
		return "retryable"
	case payments.ErrKindNotFound:
		return "not_found"
	}
	return "ambiguous"
}

// observedGateway acrescenta a contagem de falhas às chamadas de criação; as
// demais chamadas são repassadas sem alteração.
type observedGateway struct {
	payments.Gateway
	provider string
	obs      ChargeObserver
}

func (g observedGateway) EnsureCustomer(ctx context.Context, c payments.CustomerData) (string, error) {
	id, err := g.Gateway.EnsureCustomer(ctx, c)
	if err != nil {
		g.obs.ChargeFailed(g.provider, FailureKind(err))
	}
	return id, err
}

func (g observedGateway) CreateCharge(ctx context.Context, req payments.CreateChargeRequest) (payments.ProviderCharge, error) {
	pc, err := g.Gateway.CreateCharge(ctx, req)
	if err != nil {
		g.obs.ChargeFailed(g.provider, FailureKind(err))
	}
	return pc, err
}
