package payments

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// AccountInput configura um provedor de pagamento no tenant.
type AccountInput struct {
	Provider    string
	Environment string
	APIKey      string
	// WebhookSecret é o segredo de autenticação dos webhooks: segredo HMAC
	// no simulador; authToken no Asaas (enviado no header asaas-access-token).
	WebhookSecret string
	MakeDefault   bool
}

// ConfiguredAccount é o resultado da configuração. EndpointURL contém o
// token opaco e é exibido uma única vez.
type ConfiguredAccount struct {
	Account     ProviderAccount
	EndpointURL string
}

func validateAccount(in AccountInput) []apperr.FieldError {
	var f []apperr.FieldError
	switch in.Provider {
	case "simulator":
		if in.Environment != "local" {
			f = append(f, apperr.FieldError{Field: "environment", Message: "O simulador opera apenas no ambiente local."})
		}
	case "asaas":
		if in.Environment != "sandbox" && in.Environment != "production" {
			f = append(f, apperr.FieldError{Field: "environment", Message: "Use sandbox ou production."})
		}
	default:
		f = append(f, apperr.FieldError{Field: "provider", Message: "Provedor não suportado."})
	}
	if l := len(in.APIKey); l < 8 || l > 512 || strings.ContainsAny(in.APIKey, " \r\n\t") {
		f = append(f, apperr.FieldError{Field: "api_key", Message: "Informe a chave de API do provedor."})
	}
	// Regras do Asaas para authToken: 32 a 255 caracteres, sem espaços.
	if l := len(in.WebhookSecret); l < 32 || l > 255 || strings.ContainsAny(in.WebhookSecret, " \r\n\t") {
		f = append(f, apperr.FieldError{Field: "webhook_secret", Message: "O segredo do webhook deve ter de 32 a 255 caracteres, sem espaços."})
	}
	if in.Provider == "asaas" && in.WebhookSecret == in.APIKey {
		f = append(f, apperr.FieldError{Field: "webhook_secret", Message: "O token do webhook não pode ser a chave de API."})
	}
	return f
}

// ConfigureAccount valida as credenciais registrando o webhook no provedor
// (chamada externa, fora de transação) e depois grava a conta com segredos
// cifrados. Substitui a configuração anterior do mesmo provedor.
func (s *Service) ConfigureAccount(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in AccountInput) (ConfiguredAccount, error) {
	if f := validateAccount(in); len(f) > 0 {
		return ConfiguredAccount{}, apperr.Invalid("invalid_provider_account", "Configuração do provedor inválida.", f...)
	}
	if !s.resolver.Supports(in.Provider) {
		return ConfiguredAccount{}, apperr.Invalid("provider_unavailable", "Provedor indisponível nesta instalação.")
	}
	var existing *ProviderAccount
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if in.Provider == "asaas" {
			plan, err := tx.PlatformPlan(ctx)
			if err != nil {
				return err
			}
			if err := platformbilling.RequireFeature(plan, platformbilling.FeatureAsaasIntegration); err != nil {
				return err
			}
		}
		a, err := tx.AccountByProvider(ctx, in.Provider)
		if err == nil {
			existing = &a
			return nil
		}
		if apperr.IsKind(err, apperr.KindNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return ConfiguredAccount{}, err
	}
	now := s.clock.Now()
	account := ProviderAccount{ID: ids.New(), TenantID: tenantID, Provider: in.Provider, Environment: in.Environment,
		Status: "active", IsDefault: in.MakeDefault, APIKey: in.APIKey, APIKeyHint: hint(in.APIKey), Secret: in.WebhookSecret,
		CreatedAt: now, UpdatedAt: now, Version: 1}
	if existing != nil {
		account.ID, account.CreatedAt, account.Version = existing.ID, existing.CreatedAt, existing.Version
		account.IsDefault = in.MakeDefault || existing.IsDefault
	}
	token, tokenHash := tenancy.NewToken()
	endpoint := strings.TrimRight(s.cfg.WebhookBaseURL, "/") + "/" + in.Provider + "/" + token
	gw, err := s.resolver.Gateway(account)
	if err != nil {
		return ConfiguredAccount{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	if err := gw.ConfigureWebhook(callCtx, endpoint, in.WebhookSecret); err != nil {
		return ConfiguredAccount{}, apperr.Invalid("provider_rejected_configuration",
			"O provedor recusou a configuração. Verifique a chave de API e o ambiente.").Wrap(err)
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := tx.SaveAccount(ctx, account, tokenHash); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "integration.provider_configured", "payment_provider_account", account.ID.String(),
			map[string]any{"provider": in.Provider, "environment": in.Environment, "default": account.IsDefault}))
	})
	if err != nil {
		return ConfiguredAccount{}, err
	}
	return ConfiguredAccount{Account: account, EndpointURL: endpoint}, nil
}

func hint(key string) string {
	if len(key) <= 4 {
		return ""
	}
	return key[len(key)-4:]
}

// Accounts lista os provedores configurados (sem segredos).
func (s *Service) Accounts(ctx context.Context, tenantID uuid.UUID) ([]ProviderAccount, error) {
	var out []ProviderAccount
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Accounts(ctx)
		return err
	})
	for i := range out {
		out[i].APIKey, out[i].Secret = "", ""
	}
	return out, err
}

// RetryCharge cria nova tentativa de cobrança para a fatura, quando a última falhou.
func (s *Service) RetryCharge(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, invoiceID uuid.UUID) (*Charge, error) {
	c, err := s.RequestCharge(ctx, tenantID, invoiceID, actor)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, apperr.Conflict("charge_not_created", "Não há saldo a cobrar ou nenhum provedor padrão configurado.")
	}
	return c, nil
}

// Charge retorna uma cobrança.
func (s *Service) Charge(ctx context.Context, tenantID, id uuid.UUID) (Charge, error) {
	var c Charge
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		c, err = tx.Charge(ctx, id, false)
		return err
	})
	return c, err
}

// Now expõe o relógio (para cálculos de exibição nos adaptadores).
func (s *Service) Now() time.Time { return s.clock.Now() }
