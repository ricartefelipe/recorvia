package payments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

const (
	validAPIKey = "$aact_sandbox_chave_de_api_123456"
	validSecret = "token-do-webhook-com-mais-de-32-caracteres"
)

func asaasInput() AccountInput {
	return AccountInput{Provider: "asaas", Environment: "sandbox", APIKey: validAPIKey, WebhookSecret: validSecret}
}

func newAccountsFixture(t *testing.T) *payFixture {
	t.Helper()
	f := newPayFixture(t)
	delete(f.store.accounts, f.account.ID)
	f.store.plan = platformbilling.Plan{Code: "growth", Active: true, Features: []platformbilling.Feature{platformbilling.FeatureAsaasIntegration}}
	return f
}

func TestConfigureAccountValidation(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*AccountInput)
		field string
	}{
		{"provedor não suportado", func(in *AccountInput) { in.Provider = "stripe" }, "provider"},
		{"simulador fora do ambiente local", func(in *AccountInput) { in.Provider, in.Environment = "simulator", "production" }, "environment"},
		{"ambiente desconhecido no Asaas", func(in *AccountInput) { in.Environment = "local" }, "environment"},
		{"chave de API curta", func(in *AccountInput) { in.APIKey = "abc1234" }, "api_key"},
		{"chave de API longa", func(in *AccountInput) { in.APIKey = strings.Repeat("k", 513) }, "api_key"},
		{"chave de API com espaço", func(in *AccountInput) { in.APIKey = "chave com espaco" }, "api_key"},
		{"token do webhook com 31 caracteres", func(in *AccountInput) { in.WebhookSecret = strings.Repeat("s", 31) }, "webhook_secret"},
		{"token do webhook com 256 caracteres", func(in *AccountInput) { in.WebhookSecret = strings.Repeat("s", 256) }, "webhook_secret"},
		{"token do webhook com espaço", func(in *AccountInput) { in.WebhookSecret = strings.Repeat("s", 20) + " " + strings.Repeat("s", 20) }, "webhook_secret"},
		{"token do webhook com quebra de linha", func(in *AccountInput) { in.WebhookSecret = strings.Repeat("s", 40) + "\n" }, "webhook_secret"},
		{"token do webhook igual à chave", func(in *AccountInput) {
			in.APIKey = strings.Repeat("x", 40)
			in.WebhookSecret = in.APIKey
		}, "webhook_secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newAccountsFixture(t)
			in := asaasInput()
			c.edit(&in)
			_, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
			wantAppErr(t, err, apperr.KindInvalid, "invalid_provider_account")
			e, _ := apperr.As(err)
			found := false
			for _, fe := range e.Fields {
				found = found || fe.Field == c.field
			}
			if !found {
				t.Fatalf("campo %q ausente: %+v", c.field, e.Fields)
			}
			if f.store.txCount != 0 || len(f.gw.configureCalls) != 0 {
				t.Fatal("validação ocorre antes de qualquer acesso")
			}
		})
	}
}

func TestConfigureAccountAcceptsSecretBoundaries(t *testing.T) {
	for _, n := range []int{32, 255} {
		f := newAccountsFixture(t)
		in := asaasInput()
		in.WebhookSecret = strings.Repeat("s", n)
		if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in); err != nil {
			t.Fatalf("token com %d caracteres deve ser aceito: %v", n, err)
		}
	}
	f := newAccountsFixture(t)
	in := AccountInput{Provider: "simulator", Environment: "local", APIKey: strings.Repeat("a", 40), WebhookSecret: strings.Repeat("a", 40)}
	if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in); err != nil {
		t.Fatalf("no simulador o segredo pode coincidir com a chave: %v", err)
	}
}

func TestConfigureAccountRegistersWebhookAndSaves(t *testing.T) {
	f := newAccountsFixture(t)
	in := asaasInput()
	in.MakeDefault = true

	got, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "https://api.recorvia.test/webhooks/providers/asaas/"
	if !strings.HasPrefix(got.EndpointURL, prefix) {
		t.Fatalf("URL do endpoint: %q", got.EndpointURL)
	}
	token := strings.TrimPrefix(got.EndpointURL, prefix)
	if len(token) < 40 || strings.Contains(token, "/") {
		t.Fatalf("a URL deve terminar com o token opaco: %q", token)
	}
	if len(f.gw.configureCalls) != 1 {
		t.Fatalf("webhook registrado no provedor: %+v", f.gw.configureCalls)
	}
	call := f.gw.configureCalls[0]
	if call.url != got.EndpointURL || call.secret != validSecret || !call.deadline {
		t.Fatalf("registro do webhook: %+v", call)
	}
	if len(f.store.saved) != 1 {
		t.Fatalf("conta gravada: %+v", f.store.saved)
	}
	saved := f.store.saved[0]
	if string(saved.tokenHash) != string(HashEndpointToken(token)) {
		t.Fatal("o hash gravado deve corresponder ao token da URL")
	}
	a := saved.account
	if a.ID != got.Account.ID || a.Provider != "asaas" || a.Environment != "sandbox" || a.Status != "active" || !a.IsDefault ||
		a.APIKey != validAPIKey || a.APIKeyHint != "3456" || a.Secret != validSecret || a.Version != 1 || a.TenantID != f.tenantID {
		t.Fatalf("conta: %+v", a)
	}
	if len(f.resolver.accounts) != 1 || f.resolver.accounts[0].APIKey != validAPIKey {
		t.Fatal("o gateway é instanciado com as credenciais informadas")
	}
	if len(f.store.audits) != 1 {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	au := f.store.audits[0]
	if au.Action != "integration.provider_configured" || au.ResourceID != a.ID.String() || au.Details["provider"] != "asaas" ||
		au.Details["environment"] != "sandbox" || au.Details["default"] != true {
		t.Fatalf("auditoria: %+v", au)
	}
	for k, v := range au.Details {
		if s, ok := v.(string); ok && (strings.Contains(s, validAPIKey) || strings.Contains(s, validSecret) || strings.Contains(s, token)) {
			t.Fatalf("auditoria não pode conter segredos (%s)", k)
		}
	}

	// Cada configuração gera um token novo.
	in.ExpectedVersion = got.Account.Version
	again, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
	if err != nil || again.EndpointURL == got.EndpointURL || again.Account.Version != 2 {
		t.Fatalf("reconfiguração gera novo endpoint e a versão seguinte: %+v %v", again.Account, err)
	}
}

// A configuração exige a versão vista por quem configura (If-Match): 0 para o
// provedor ainda não configurado. Divergente, o provedor não é chamado e nada
// é gravado; a condição da gravação recusa também a configuração concorrente
// que passou pela recusa antecipada.
func TestConfigureAccountRequiresCurrentVersion(t *testing.T) {
	f := newAccountsFixture(t)
	in := asaasInput()
	in.ExpectedVersion = 1
	_, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
	wantAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if len(f.gw.configureCalls) != 0 || len(f.store.saved) != 0 || len(f.store.audits) != 0 {
		t.Fatal("versão divergente não chama o provedor nem grava")
	}
	in.ExpectedVersion = 0
	if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in); err != nil {
		t.Fatal(err)
	}
	// Outra pessoa configurou o provedor enquanto esta configuração consultava o provedor.
	f.gw.onConfigure = func() {
		for id, a := range f.store.accounts {
			if a.Provider == "asaas" {
				a.Version++
				f.store.accounts[id] = a
			}
		}
	}
	in.ExpectedVersion = 1
	_, err = f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
	wantAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if len(f.store.saved) != 1 || len(f.store.audits) != 1 {
		t.Fatal("a configuração concorrente não pode ser gravada")
	}
}

func TestConfigureAccountReplacesExistingConfiguration(t *testing.T) {
	f := newAccountsFixture(t)
	existing := ProviderAccount{ID: uuid.New(), TenantID: f.tenantID, Provider: "asaas", Environment: "sandbox", IsDefault: true,
		CreatedAt: payNow.AddDate(0, -2, 0), Version: 4}
	f.store.accounts[existing.ID] = existing
	in := asaasInput()
	in.ExpectedVersion = existing.Version
	got, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	a := got.Account
	if a.ID != existing.ID || !a.CreatedAt.Equal(existing.CreatedAt) || a.Version != 5 || !a.IsDefault {
		t.Fatalf("a configuração anterior é substituída preservando identidade e padrão: %+v", a)
	}
	if !a.UpdatedAt.Equal(payNow) || a.APIKey != validAPIKey {
		t.Fatalf("credenciais atualizadas: %+v", a)
	}
}

func TestConfigureAccountRules(t *testing.T) {
	t.Run("Asaas exige o recurso asaas_integration", func(t *testing.T) {
		f := newAccountsFixture(t)
		f.store.plan.Features = []platformbilling.Feature{platformbilling.FeatureRecurringBilling}
		_, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, asaasInput())
		wantAppErr(t, err, apperr.KindForbidden, "platform_feature_unavailable")
		if len(f.gw.configureCalls) != 0 || len(f.store.saved) != 0 {
			t.Fatal("sem o recurso nada é chamado nem gravado")
		}
	})
	t.Run("simulador não depende do plano", func(t *testing.T) {
		f := newAccountsFixture(t)
		f.store.plan.Features = nil
		in := AccountInput{Provider: "simulator", Environment: "local", APIKey: "sim_key_123456", WebhookSecret: validSecret}
		if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, in); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("provedor indisponível na instalação", func(t *testing.T) {
		f := newAccountsFixture(t)
		f.resolver.unsupported["asaas"] = true
		_, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, asaasInput())
		wantAppErr(t, err, apperr.KindInvalid, "provider_unavailable")
	})
	t.Run("provedor recusa o registro do webhook", func(t *testing.T) {
		f := newAccountsFixture(t)
		f.gw.configureErr = errPermanent
		_, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, asaasInput())
		wantAppErr(t, err, apperr.KindInvalid, "provider_rejected_configuration")
		if !errors.Is(err, errPermanent) {
			t.Fatal("a causa do provedor deve ficar encadeada")
		}
		if len(f.store.saved) != 0 || len(f.store.audits) != 0 {
			t.Fatal("falha no provedor não grava a conta")
		}
	})
	for _, m := range []string{"PlatformPlan", "AccountByProvider", "SaveAccount", "Audit"} {
		t.Run("falha em "+m, func(t *testing.T) {
			f := newAccountsFixture(t)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, asaasInput()); !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			if len(f.store.saved) != 0 || len(f.store.audits) != 0 {
				t.Fatal("falha não deixa conta gravada")
			}
		})
	}
	t.Run("gateway não pode ser instanciado", func(t *testing.T) {
		f := newAccountsFixture(t)
		f.resolver.gwErr = errors.New("chave mestra ausente")
		if _, err := f.svc.ConfigureAccount(context.Background(), f.tenantID, f.actor, asaasInput()); !errors.Is(err, f.resolver.gwErr) {
			t.Fatalf("erro: %v", err)
		}
	})
}

func TestAccountsHidesSecrets(t *testing.T) {
	f := newPayFixture(t)
	other := ProviderAccount{ID: uuid.New(), TenantID: f.tenantID, Provider: "asaas", APIKey: validAPIKey, APIKeyHint: "3456", Secret: validSecret}
	f.store.accounts[other.ID] = other
	list, err := f.svc.Accounts(context.Background(), f.tenantID)
	if err != nil || len(list) != 2 {
		t.Fatalf("Accounts: %v %d", err, len(list))
	}
	for _, a := range list {
		if a.APIKey != "" || a.Secret != "" {
			t.Fatalf("segredos não podem sair do serviço: %+v", a)
		}
	}
	if list[0].APIKeyHint != "3456" {
		t.Fatal("a dica da chave continua disponível")
	}
	if f.store.accounts[other.ID].APIKey != validAPIKey {
		t.Fatal("a listagem não altera o armazenamento")
	}
	f.store.errs["Accounts"] = errors.New("banco indisponível")
	if _, err := f.svc.Accounts(context.Background(), f.tenantID); err == nil {
		t.Fatal("erro deve ser propagado")
	}
}
