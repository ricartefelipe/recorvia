package pgstore

import (
	"context"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
)

// Finalidades conferidas pela verificação do keyring.
const (
	KeyringPurposeProviderAccount = "credencial de provedor de pagamento"
	KeyringPurposeWebhookSigning  = "segredo de endpoint de webhook"
	KeyringPurposeNotification    = "conteúdo de notificação pendente"
)

// KeyringAudit é o resultado da conferência do keyring contra o banco.
type KeyringAudit struct {
	secrets.AuditReport
	// Tenants é o número de organizações percorridas.
	Tenants int
}

// AuditKeyring confere se o keyring configurado decifra os dados cifrados
// gravados no banco: credenciais das contas de provedor (chave de API e
// segredo de webhook), segredos dos endpoints de webhook e o conteúdo das
// notificações ainda não encerradas (uma por chave e por organização). Nada
// é alterado e nenhum conteúdo decifrado sai desta função.
//
// Contam como dados em uso as contas de provedor ativas, os segredos ainda
// válidos de endpoints não removidos e as notificações pendentes; contas
// desativadas e segredos expirados entram no relatório sem bloquear. O
// relatório decide por chave o que é engano do keyring (secrets.Audit): uma
// chave que decifra algum registro, de qualquer finalidade, está certa, e as
// chaves declaradas perdidas no keyring não bloqueiam.
//
// A leitura percorre as organizações uma a uma, cada qual na sua transação
// com contexto de tenant (a RLS impede uma leitura única entre tenants).
func (s *Store) AuditKeyring(ctx context.Context) (KeyringAudit, error) {
	catalog, err := s.Tenancy().PlatformCatalog(ctx)
	if err != nil {
		return KeyringAudit{}, err
	}
	audit := secrets.NewAudit(s.keyring)
	now := s.clock.Now()
	for _, t := range catalog {
		err := s.inTenant(ctx, t.TenantID, func(b base) error {
			accounts, err := b.q.KeyringProviderAccounts(ctx, t.TenantID)
			if err != nil {
				return err
			}
			for _, a := range accounts {
				inUse := a.Status == "active"
				if a.ApiKeyCiphertext != nil && a.ApiKeyKeyID != nil {
					audit.Check(KeyringPurposeProviderAccount, *a.ApiKeyKeyID, a.ApiKeyCiphertext, aad(t.TenantID, a.Provider, "api_key"), inUse)
				}
				if a.WebhookSecretCiphertext != nil && a.WebhookSecretKeyID != nil {
					audit.Check(KeyringPurposeProviderAccount, *a.WebhookSecretKeyID, a.WebhookSecretCiphertext,
						aad(t.TenantID, a.Provider, "webhook_secret"), inUse)
				}
			}
			hooks, err := b.q.KeyringWebhookSecrets(ctx, dbgen.KeyringWebhookSecretsParams{Now: &now, TenantID: t.TenantID})
			if err != nil {
				return err
			}
			for _, h := range hooks {
				audit.Check(KeyringPurposeWebhookSigning, h.SecretKeyID, h.SecretCiphertext, secretAAD(t.TenantID, h.EndpointID, h.ID), h.InUse)
			}
			pending, err := b.q.KeyringPendingNotifications(ctx, t.TenantID)
			if err != nil {
				return err
			}
			for _, n := range pending {
				if n.ContentKeyID != nil {
					audit.Check(KeyringPurposeNotification, *n.ContentKeyID, n.ContentCiphertext, contentAAD(t.TenantID, n.ID), true)
				}
			}
			return nil
		})
		if err != nil {
			return KeyringAudit{}, err
		}
	}
	return KeyringAudit{AuditReport: audit.Report(), Tenants: len(catalog)}, nil
}
