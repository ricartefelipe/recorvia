package gateways

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

type failure struct{ provider, kind string }

type recorder struct{ got []failure }

func (r *recorder) ChargeFailed(provider, kind string) {
	r.got = append(r.got, failure{provider, kind})
}

// stubGateway responde com os erros configurados; os métodos não usados
// pelos testes vêm da interface embutida (nil).
type stubGateway struct {
	payments.Gateway
	customerErr, chargeErr error
}

func (s stubGateway) EnsureCustomer(context.Context, payments.CustomerData) (string, error) {
	if s.customerErr != nil {
		return "", s.customerErr
	}
	return "cus_1", nil
}

func (s stubGateway) CreateCharge(context.Context, payments.CreateChargeRequest) (payments.ProviderCharge, error) {
	if s.chargeErr != nil {
		return payments.ProviderCharge{}, s.chargeErr
	}
	return payments.ProviderCharge{ProviderChargeID: "ch_1"}, nil
}

func (s stubGateway) GetCharge(context.Context, string) (payments.ProviderCharge, error) {
	return payments.ProviderCharge{}, errors.New("falha de consulta")
}

func TestFailureKind(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"permanente", &payments.GatewayError{Kind: payments.ErrKindPermanent}, "permanent"},
		{"repetível", &payments.GatewayError{Kind: payments.ErrKindRetryable}, "retryable"},
		{"ambígua", &payments.GatewayError{Kind: payments.ErrKindAmbiguous}, "ambiguous"},
		{"inexistente", &payments.GatewayError{Kind: payments.ErrKindNotFound}, "not_found"},
		{"encadeada", fmt.Errorf("criação: %w", &payments.GatewayError{Kind: payments.ErrKindPermanent}), "permanent"},
		{"não classificada", context.DeadlineExceeded, "ambiguous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FailureKind(tc.err)
			if got != tc.want {
				t.Fatalf("FailureKind = %q, esperado %q", got, tc.want)
			}
			// O rótulo precisa estar entre as séries inicializadas pelas métricas.
			if !slices.Contains(telemetry.ChargeFailureKinds, got) {
				t.Fatalf("rótulo %q fora de telemetry.ChargeFailureKinds", got)
			}
		})
	}
}

func TestResolverObservesChargeCreationFailures(t *testing.T) {
	declined := &payments.GatewayError{Kind: payments.ErrKindPermanent, StatusCode: 400}
	cases := []struct {
		name                   string
		customerErr, chargeErr error
		want                   []failure
	}{
		{"sucesso não conta", nil, nil, nil},
		{"recusa na criação", nil, declined, []failure{{"simulator", "permanent"}}},
		{"cadastro do cliente indisponível", &payments.GatewayError{Kind: payments.ErrKindRetryable}, nil, []failure{{"simulator", "retryable"}}},
		{"timeout na criação", nil, context.DeadlineExceeded, []failure{{"simulator", "ambiguous"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			r := NewResolver(0, Factory{Provider: "simulator", Gateway: func(payments.ProviderAccount, *http.Client) (payments.Gateway, error) {
				return stubGateway{customerErr: tc.customerErr, chargeErr: tc.chargeErr}, nil
			}})
			r.Observe(rec)
			gw, err := r.Gateway(payments.ProviderAccount{Provider: "simulator"})
			if err != nil {
				t.Fatal(err)
			}
			_, cerr := gw.EnsureCustomer(context.Background(), payments.CustomerData{})
			_, err = gw.CreateCharge(context.Background(), payments.CreateChargeRequest{})
			if !errors.Is(cerr, tc.customerErr) || !errors.Is(err, tc.chargeErr) {
				t.Fatalf("erros devem ser repassados sem alteração: %v, %v", cerr, err)
			}
			// Consultas não contam como falha de criação.
			if _, err := gw.GetCharge(context.Background(), "ch_1"); err == nil {
				t.Fatal("a consulta deveria repassar o erro do adaptador")
			}
			if fmt.Sprint(rec.got) != fmt.Sprint(tc.want) {
				t.Fatalf("falhas observadas = %v, esperado %v", rec.got, tc.want)
			}
		})
	}
}

func TestResolverWithoutObserverReturnsAdapter(t *testing.T) {
	r := NewResolver(0, Factory{Provider: "simulator", Gateway: func(payments.ProviderAccount, *http.Client) (payments.Gateway, error) {
		return stubGateway{}, nil
	}})
	gw, err := r.Gateway(payments.ProviderAccount{Provider: "simulator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gw.(stubGateway); !ok {
		t.Fatalf("sem observador, o adaptador deve ser devolvido sem decoração: %T", gw)
	}
	if _, err := r.Gateway(payments.ProviderAccount{Provider: "asaas"}); err == nil {
		t.Fatal("provedor não registrado deveria falhar")
	}
}
